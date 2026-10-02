package auditlog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// GenesisHash is the previous hash of the first entry of a chain that was never
// pruned: 64 zeros.
var GenesisHash = strings.Repeat("0", sha256.Size*2)

// canonicalEvent is the hashed form of an Event: every field but the hash, in this
// order. Changing it breaks every stored chain.
type canonicalEvent struct {
	ID           int64             `json:"id"`
	Time         string            `json:"time"`
	ActorKind    string            `json:"actor_kind"`
	ActorUserID  string            `json:"actor_user_id"`
	ActorName    string            `json:"actor_name"`
	ActorKeyID   string            `json:"actor_key_id"`
	ActorKeyName string            `json:"actor_key_name"`
	Action       string            `json:"action"`
	Targets      map[string]string `json:"targets"`
	Status       int               `json:"status"`
	Outcome      string            `json:"outcome"`
	ClientIP     string            `json:"client_ip"`
	UserAgent    string            `json:"user_agent"`
	Count        int               `json:"count"`
}

// FormatTime renders t as the entries' canonical time: RFC 3339 in UTC with
// nanoseconds, trailing zeros trimmed (time.RFC3339Nano), as in the JSON of an Event.
func FormatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// Canonical returns the canonical JSON of e without its hash: the fields of the
// exported Event in the order id, time, actor_kind, actor_user_id, actor_name,
// actor_key_id, actor_key_name, action, targets, status, outcome, client_ip,
// user_agent, count; no whitespace; targets with sorted keys ({} when empty); no
// HTML escaping. Stored strings hold no control characters (see Service.Record), so
// the only escapes are \" and \\.
func Canonical(e *Event) ([]byte, error) {
	targets := e.Targets
	if targets == nil {
		targets = map[string]string{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(canonicalEvent{
		ID: e.ID, Time: FormatTime(e.Time), ActorKind: e.ActorKind, ActorUserID: e.ActorUserID,
		ActorName: e.ActorName, ActorKeyID: e.ActorKeyID, ActorKeyName: e.ActorKeyName, Action: e.Action,
		Targets: targets, Status: e.Status, Outcome: e.Outcome, ClientIP: e.ClientIP, UserAgent: e.UserAgent,
		Count: e.Count,
	})
	if err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// ChainHash returns the hash of e chained to prev (the previous entry's hash, the
// anchor's after pruning, or GenesisHash): lowercase hex of
// SHA-256(prev || Canonical(e)), where prev is the 64 hex characters as ASCII.
func ChainHash(prev string, e *Event) (string, error) {
	doc, err := Canonical(e)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write(doc)
	return hex.EncodeToString(h.Sum(nil)), nil
}
