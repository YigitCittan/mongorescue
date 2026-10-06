// Package postrestore validates and plans the post-restore commands of a connection
// (models.PostRestoreCommand): MongoDB commands that run against the databases a
// safe-clone restore created, before the restore is reported complete, to re-apply
// erasures to restored data.
//
// Only data commands on one collection of the restored database are allowed
// (AllowedCommands). Server-side JavaScript, aggregation stages that read or write
// other collections or databases, and any command that names a database are
// refused, so a command can only change the clone it runs in. Plan maps the
// commands onto the clones of one restore and refuses any target that is not a
// clone the restore created. The package does not run commands: internal/mongoconn
// does, and internal/restore orchestrates.
package postrestore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// Sentinel errors.
var (
	// ErrInvalid is returned for a command list or command that is refused; the
	// message says why.
	ErrInvalid = errors.New("post-restore commands")
	// ErrNotClone is returned by Plan when a command would run in a database that
	// is not a clone the restore created.
	ErrNotClone = errors.New("post-restore commands: target is not a database the restore created")
)

// Limits.
const (
	// MaxCommands caps the commands of one connection.
	MaxCommands = 100
	// MaxTotalBytes caps the size of all command documents of one connection.
	MaxTotalBytes = 512 << 10
	// maxDepth caps the nesting of a command document.
	maxDepth = 64
	// maxCollectionLength caps a collection name.
	maxCollectionLength = 255
)

// AllowedCommands are the command names a post-restore command may use: writes and
// schema changes on one collection of the database it runs in.
var AllowedCommands = []string{"delete", "update", "findAndModify", "dropIndexes", "collMod", "drop"}

// refusedReasons explains why commonly tried commands are refused.
var refusedReasons = map[string]string{
	"eval":             "runs server-side JavaScript",
	"$eval":            "runs server-side JavaScript",
	"applyOps":         "applies raw oplog entries to any namespace",
	"aggregate":        "can write to other collections and databases ($out, $merge)",
	"mapReduce":        "runs server-side JavaScript and can write to other databases",
	"renameCollection": "can move a collection to another database",
	"dropDatabase":     "drops the whole database",
	"insert":           "only removing or changing restored data is allowed",
	"create":           "only removing or changing restored data is allowed",
}

// refusedOperators are operators refused anywhere in a command document: they run
// server-side JavaScript, or read or write other collections or databases.
var refusedOperators = map[string]string{
	"$where":       "runs server-side JavaScript",
	"$function":    "runs server-side JavaScript",
	"$accumulator": "runs server-side JavaScript",
	"$out":         "writes to another collection or database",
	"$merge":       "writes to another collection or database",
	"$lookup":      "reads another collection or database",
	"$graphLookup": "reads another collection",
	"$unionWith":   "reads another collection or database",
	"$db":          "names a database",
}

// systemDatabases are never a post-restore target.
var systemDatabases = []string{models.AdminDatabase, "config", "local"}

// Command is a parsed, allowed command document.
type Command struct {
	// Name is the command name (one of AllowedCommands).
	Name string
	// Collection is the collection it acts on.
	Collection string
}

// Parse checks a command document and returns its name and collection. The document
// is a JSON object (extended JSON values such as {"$oid": "..."} are fine) whose
// first key is an allowed command naming a collection; no key may repeat in one
// object, no top-level key may start with "$" and none of the refused operators may
// appear at any depth.
func Parse(raw json.RawMessage) (Command, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return Command{}, fmt.Errorf("%w: a command must be a JSON object such as {\"delete\": \"users\", \"deletes\": [...]}", ErrInvalid)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	w := &walker{dec: dec}
	if err := w.value(0); err != nil {
		return Command{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Command{}, fmt.Errorf("%w: unexpected data after the command document", ErrInvalid)
	}
	if w.name == "" {
		return Command{}, fmt.Errorf("%w: the command document is empty", ErrInvalid)
	}
	if !slices.Contains(AllowedCommands, w.name) {
		if reason, ok := refusedReasons[w.name]; ok {
			return Command{}, fmt.Errorf("%w: %s is not allowed: it %s; allowed: %s", ErrInvalid, w.name, reason, strings.Join(AllowedCommands, ", "))
		}
		return Command{}, fmt.Errorf("%w: %s is not allowed; allowed: %s", ErrInvalid, quote(w.name), strings.Join(AllowedCommands, ", "))
	}
	if err := checkCollection(w.name, w.collection, w.collectionOK); err != nil {
		return Command{}, err
	}
	if err := checkNoUpsert(w.name, raw); err != nil {
		return Command{}, err
	}
	return Command{Name: w.name, Collection: w.collection}, nil
}

// checkNoUpsert refuses upserts: the commands re-apply erasures and must never
// insert. update refuses "upsert" in any of its updates and findAndModify at its top
// level, unless it is false.
func checkNoUpsert(name string, raw json.RawMessage) error {
	var doc struct {
		Upsert  json.RawMessage   `json:"upsert"`
		Updates []json.RawMessage `json:"updates"`
	}
	if name != "update" && name != "findAndModify" {
		return nil
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%w: %s: %s", ErrInvalid, name, jsonError(err))
	}
	upserts := []json.RawMessage{doc.Upsert}
	if name == "update" {
		upserts = upserts[:0]
		for _, u := range doc.Updates {
			var item struct {
				Upsert json.RawMessage `json:"upsert"`
			}
			if json.Unmarshal(u, &item) != nil {
				return fmt.Errorf("%w: update: every entry of \"updates\" must be an object", ErrInvalid)
			}
			upserts = append(upserts, item.Upsert)
		}
	}
	for _, u := range upserts {
		if len(u) > 0 && string(bytes.TrimSpace(u)) != "false" {
			return fmt.Errorf("%w: %s with upsert is not allowed: post-restore commands only remove or change restored data, never insert", ErrInvalid, name)
		}
	}
	return nil
}

// checkCollection checks the collection a command names.
func checkCollection(name, coll string, isString bool) error {
	switch {
	case !isString || coll == "":
		return fmt.Errorf("%w: %s must name a collection, e.g. {%q: \"users\", ...}", ErrInvalid, name, name)
	case len(coll) > maxCollectionLength:
		return fmt.Errorf("%w: %s: collection name longer than %d bytes", ErrInvalid, name, maxCollectionLength)
	case strings.HasPrefix(coll, "system."):
		return fmt.Errorf("%w: %s: system collections (%s) are not allowed", ErrInvalid, name, quote(coll))
	case strings.ContainsAny(coll, "$\x00") || !utf8.ValidString(coll):
		return fmt.Errorf("%w: %s: invalid collection name %s", ErrInvalid, name, quote(coll))
	}
	return nil
}

// walker reads a command document token by token, so the order of its keys (the
// first one is the command name) is kept.
type walker struct {
	dec *json.Decoder
	// name and collection are the first top-level key and its value.
	name, collection string
	collectionOK     bool
}

// value reads one JSON value at depth.
func (w *walker) value(depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("%w: the command document is nested more than %d levels deep", ErrInvalid, maxDepth)
	}
	tok, err := w.dec.Token()
	if err != nil {
		return fmt.Errorf("%w: invalid JSON: %s", ErrInvalid, jsonError(err))
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		if n, isNumber := tok.(json.Number); isNumber && !safeInteger(n) {
			return fmt.Errorf("%w: the integer %s is too large for JSON clients such as the dashboard; write it as {\"$numberLong\": \"%s\"}", ErrInvalid, n, n)
		}
		return nil
	}
	switch delim {
	case '{':
		return w.object(depth)
	case '[':
		for w.dec.More() {
			if itemErr := w.value(depth + 1); itemErr != nil {
				return itemErr
			}
		}
		_, err = w.dec.Token()
		if err != nil {
			return fmt.Errorf("%w: invalid JSON: %s", ErrInvalid, jsonError(err))
		}
		return nil
	}
	return fmt.Errorf("%w: invalid JSON", ErrInvalid)
}

// object reads the members of an object whose '{' was read.
func (w *walker) object(depth int) error {
	seen := map[string]bool{}
	first := true
	for w.dec.More() {
		tok, err := w.dec.Token()
		if err != nil {
			return fmt.Errorf("%w: invalid JSON: %s", ErrInvalid, jsonError(err))
		}
		key, _ := tok.(string)
		if seen[key] {
			return fmt.Errorf("%w: the key %s appears twice in one object", ErrInvalid, quote(key))
		}
		seen[key] = true
		if reason, ok := refusedOperators[key]; ok {
			return fmt.Errorf("%w: %s is not allowed: it %s", ErrInvalid, key, reason)
		}
		if depth == 0 && strings.HasPrefix(key, "$") {
			return fmt.Errorf("%w: the top-level field %s is not allowed", ErrInvalid, quote(key))
		}
		if depth == 0 && first {
			w.name = key
			if err := w.collectionValue(); err != nil {
				return err
			}
		} else if err := w.value(depth + 1); err != nil {
			return err
		}
		first = false
	}
	if _, err := w.dec.Token(); err != nil {
		return fmt.Errorf("%w: invalid JSON: %s", ErrInvalid, jsonError(err))
	}
	return nil
}

// collectionValue reads the value of the command name: the collection.
func (w *walker) collectionValue() error {
	if !w.dec.More() {
		return fmt.Errorf("%w: invalid JSON", ErrInvalid)
	}
	var v json.RawMessage
	if err := w.dec.Decode(&v); err != nil {
		return fmt.Errorf("%w: invalid JSON: %s", ErrInvalid, jsonError(err))
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		w.collection, w.collectionOK = s, true
	}
	return nil
}

// maxSafeInteger is the largest integer a JSON number keeps exactly in JavaScript
// (2^53 - 1).
const maxSafeInteger = 1<<53 - 1

// safeInteger reports whether n is not an integer literal, or one JavaScript keeps
// exactly: an erasure log must never round an _id.
func safeInteger(n json.Number) bool {
	s := string(n)
	if strings.ContainsAny(s, ".eE") {
		return true
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return err == nil && v >= -maxSafeInteger && v <= maxSafeInteger
}

// jsonError shortens a JSON syntax error.
func jsonError(err error) string {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected end"
	}
	return strings.TrimPrefix(err.Error(), "json: ")
}

// quote quotes s for a message, cut to 64 bytes.
func quote(s string) string {
	if len(s) > 64 {
		s = s[:64] + "…"
	}
	return fmt.Sprintf("%q", s)
}

// Validate checks a connection's command list: at most MaxCommands commands of at
// most MaxTotalBytes together, each with a valid database ("*" or a database name
// other than admin, config and local) and an allowed command (see Parse).
func Validate(cmds []models.PostRestoreCommand) error {
	if len(cmds) > MaxCommands {
		return fmt.Errorf("%w: at most %d commands", ErrInvalid, MaxCommands)
	}
	total := 0
	for i, c := range cmds {
		total += len(c.Command)
		if err := validateOne(c); err != nil {
			return fmt.Errorf("command %d: %w", i+1, err)
		}
	}
	if total > MaxTotalBytes {
		return fmt.Errorf("%w: the commands may hold at most %d KiB together", ErrInvalid, MaxTotalBytes>>10)
	}
	return nil
}

// validateOne checks one command.
func validateOne(c models.PostRestoreCommand) error {
	if err := checkDatabase(c.Database); err != nil {
		return err
	}
	_, err := Parse(c.Command)
	return err
}

// checkDatabase checks the database of a command.
func checkDatabase(db string) error {
	switch {
	case db == models.PostRestoreAllDatabases:
		return nil
	case db == "":
		return fmt.Errorf("%w: database is required (a database name, or \"*\" for every restored database)", ErrInvalid)
	case slices.Contains(systemDatabases, db):
		return fmt.Errorf("%w: database %s is not allowed", ErrInvalid, db)
	}
	if err := models.ValidateDatabaseName(db); err != nil {
		return fmt.Errorf("%w: database: %w", ErrInvalid, err)
	}
	return nil
}

// Step is one command to run in one restored database.
type Step struct {
	// Index is the command's position in the connection's list (from 0).
	Index int
	// Source is the restored database and Target its clone, where the command runs.
	Source, Target string
	// Command is the parsed command.
	Command
	// Document is the command document.
	Document json.RawMessage
}

// Result returns the step as a result with status (models.PostRestoreCommand*).
func (s Step) Result(status string) models.PostRestoreResult {
	return models.PostRestoreResult{Index: s.Index, Database: s.Target, SourceDatabase: s.Source,
		Command: s.Name, Collection: s.Collection, Status: status}
}

// Plan maps cmds onto the clones one restore created (source database -> clone):
// "*" runs a command in every clone, in source name order; a database name runs it
// in that database's clone, and not at all when the restore did not restore that
// database. Commands are validated again (they may predate stricter rules). Every
// target must be one of the clones, and none may be a source database or a system
// database; otherwise Plan fails with ErrNotClone and nothing may run.
func Plan(cmds []models.PostRestoreCommand, clones map[string]string) ([]Step, error) {
	if len(cmds) == 0 {
		return nil, nil
	}
	if err := Validate(cmds); err != nil {
		return nil, err
	}
	sources := make([]string, 0, len(clones))
	created := make(map[string]bool, len(clones))
	for src, clone := range clones {
		sources = append(sources, src)
		created[clone] = true
	}
	slices.Sort(sources)
	var steps []Step
	for i, c := range cmds {
		parsed, _ := Parse(c.Command)
		targets := sources
		if c.Database != models.PostRestoreAllDatabases {
			targets = nil
			if _, ok := clones[c.Database]; ok {
				targets = []string{c.Database}
			}
		}
		for _, src := range targets {
			steps = append(steps, Step{Index: i, Source: src, Target: clones[src], Command: parsed, Document: c.Command})
		}
	}
	for _, s := range steps {
		_, isSource := clones[s.Target]
		if s.Target == "" || !created[s.Target] || isSource || s.Target == s.Source || slices.Contains(systemDatabases, s.Target) {
			return nil, fmt.Errorf("%w: %s", ErrNotClone, quote(s.Target))
		}
	}
	return steps, nil
}

// Planned returns the steps as results with status planned.
func Planned(steps []Step) []models.PostRestoreResult {
	out := make([]models.PostRestoreResult, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Result(models.PostRestoreCommandPlanned))
	}
	return out
}

// Describe summarises steps for a log line or a preflight message, e.g.
// "delete on users in shop_rescue_20261006_101500".
func Describe(steps []Step) string {
	parts := make([]string, 0, len(steps))
	for _, s := range steps {
		parts = append(parts, fmt.Sprintf("%s on %s in %s", s.Name, s.Collection, s.Target))
	}
	return strings.Join(parts, "; ")
}
