package postrestore

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func cmd(db, doc string) models.PostRestoreCommand {
	return models.PostRestoreCommand{Database: db, Command: json.RawMessage(doc)}
}

func TestParseAllowsDataCommands(t *testing.T) {
	cases := map[string]Command{
		`{"delete": "users", "deletes": [{"q": {"_id": {"$in": [1, 2, {"$oid": "65f0a1b2c3d4e5f601234567"}]}}, "limit": 0}]}`: {"delete", "users"},
		`{"update": "users", "updates": [{"q": {"email": "a@example.com"}, "u": {"$unset": {"phone": ""}}, "multi": true}]}`:  {"update", "users"},
		`{"update": "users", "updates": [{"q": {}, "u": [{"$set": {"x": 1}}]}], "writeConcern": {"w": 1}}`:                    {"update", "users"},
		`{"findAndModify": "users", "query": {"_id": 7}, "remove": true}`:                                                     {"findAndModify", "users"},
		`{"dropIndexes": "users", "index": "email_1"}`:                                                                        {"dropIndexes", "users"},
		`{"collMod": "users", "validationLevel": "off"}`:                                                                      {"collMod", "users"},
		`  {"drop": "sessions.old"}  `: {"drop", "sessions.old"},
		`{"delete": "a.b", "deletes": [{"q": {"role": "admin", "db": "local"}, "limit": 0}]}`: {"delete", "a.b"},
	}
	for doc, want := range cases {
		got, err := Parse(json.RawMessage(doc))
		if err != nil {
			t.Errorf("Parse(%s): %v", doc, err)
			continue
		}
		if got != want {
			t.Errorf("Parse(%s) = %+v; want %+v", doc, got, want)
		}
	}
}

func TestParseRefusals(t *testing.T) {
	cases := map[string]string{
		``:                              "JSON object",
		`[]`:                            "JSON object",
		`"delete"`:                      "JSON object",
		`{}`:                            "empty",
		`{"delete": "users"`:            "invalid JSON",
		`{"delete": "users"}x`:          "unexpected data",
		`{"eval": "db.dropDatabase()"}`: "server-side JavaScript",
		`{"$eval": "1"}`:                "top-level field",
		`{"applyOps": [{"op": "d", "ns": "admin.system.users", "o": {}}]}`:   "raw oplog entries",
		`{"aggregate": "users", "pipeline": [{"$out": "x"}], "cursor": {}}`:  "$out",
		`{"aggregate": "users", "pipeline": [{"$match": {}}], "cursor": {}}`: "aggregate is not allowed",
		`{"renameCollection": "shop.users", "to": "other.users"}`:            "another database",
		`{"dropDatabase": 1}`:                                     "whole database",
		`{"insert": "users", "documents": [{}]}`:                  "only removing or changing",
		`{"shutdown": 1}`:                                         "\"shutdown\" is not allowed",
		`{"Delete": "users"}`:                                     "\"Delete\" is not allowed",
		`{"findandmodify": "users", "query": {}, "remove": true}`: "not allowed",
		`{"delete": "users", "deletes": [{"q": {"$where": "sleep(1000)"}, "limit": 0}]}`:                       "$where",
		`{"update": "u", "updates": [{"q": {}, "u": [{"$merge": {"into": {"db": "admin", "coll": "x"}}}]}]}`:   "$merge",
		`{"update": "u", "updates": [{"q": {}, "u": [{"$lookup": {"from": {"db": "config", "coll": "x"}}}]}]}`: "$lookup",
		`{"update": "u", "updates": [{"q": {"$expr": {"$function": {"body": "x"}}}, "u": {}}]}`:                "$function",
		`{"delete": "users", "deletes": [], "$db": "admin"}`:                                                   "$db",
		`{"delete": "users", "$readPreference": {"mode": "primary"}}`:                                          "top-level field",
		`{"delete": "users", "delete": "other"}`:                                                               "twice",
		`{"delete": "users", "deletes": [{"q": {"a": 1, "a": 2}}]}`:                                            "twice",
		`{"delete": 1}`:             "must name a collection",
		`{"delete": ""}`:            "must name a collection",
		`{"delete": {"$out": "x"}}`: "must name a collection",
		`{"drop": "system.users"}`:  "system collections",
		`{"drop": "a$b"}`:           "invalid collection",
	}
	for doc, want := range cases {
		_, err := Parse(json.RawMessage(doc))
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%s) = %v; want ErrInvalid", doc, err)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%s) = %q; want it to mention %q", doc, err, want)
		}
	}
}

func TestParseRefusesIntegersJavaScriptWouldRound(t *testing.T) {
	ok := []string{
		`{"delete": "u", "deletes": [{"q": {"_id": 9007199254740991}, "limit": 0}]}`,
		`{"delete": "u", "deletes": [{"q": {"_id": -9007199254740991, "x": 1.5e300}, "limit": 0}]}`,
		`{"delete": "u", "deletes": [{"q": {"_id": {"$numberLong": "1234567890123456789"}}, "limit": 0}]}`,
	}
	for _, doc := range ok {
		if _, err := Parse(json.RawMessage(doc)); err != nil {
			t.Errorf("Parse(%s): %v", doc, err)
		}
	}
	for _, doc := range []string{
		`{"delete": "u", "deletes": [{"q": {"_id": 9007199254740992}, "limit": 0}]}`,
		`{"delete": "u", "deletes": [{"q": {"_id": 123456789012345678901234567890}, "limit": 0}]}`,
	} {
		if _, err := Parse(json.RawMessage(doc)); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "$numberLong") {
			t.Errorf("Parse(%s) = %v; want a refusal pointing to $numberLong", doc, err)
		}
	}
}

func TestParseRefusesDeepNesting(t *testing.T) {
	doc := `{"delete": "u", "deletes": ` + strings.Repeat("[", maxDepth+2) + strings.Repeat("]", maxDepth+2) + `}`
	if _, err := Parse(json.RawMessage(doc)); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "nested") {
		t.Fatalf("deep nesting: %v", err)
	}
}

func TestValidate(t *testing.T) {
	ok := cmd("*", `{"drop": "tmp"}`)
	if err := Validate([]models.PostRestoreCommand{ok, cmd("shop", `{"drop": "tmp"}`)}); err != nil {
		t.Fatalf("valid list: %v", err)
	}
	refused := map[string]models.PostRestoreCommand{
		"database is required": cmd("", `{"drop": "tmp"}`),
		"database admin":       cmd("admin", `{"drop": "tmp"}`),
		"database config":      cmd("config", `{"drop": "tmp"}`),
		"database local":       cmd("local", `{"drop": "tmp"}`),
		"database:":            cmd("a/b", `{"drop": "tmp"}`),
		"applyOps":             cmd("shop", `{"applyOps": []}`),
	}
	for want, c := range refused {
		err := Validate([]models.PostRestoreCommand{ok, c})
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), want) || !strings.HasPrefix(err.Error(), "command 2:") {
			t.Errorf("Validate(%+v) = %v; want ErrInvalid naming command 2 and %q", c, err, want)
		}
	}
	many := slices.Repeat([]models.PostRestoreCommand{ok}, MaxCommands+1)
	if err := Validate(many); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%d commands: %v", len(many), err)
	}
	big := cmd("*", `{"delete": "u", "deletes": [{"q": {"x": "`+strings.Repeat("a", MaxTotalBytes)+`"}, "limit": 0}]}`)
	if err := Validate([]models.PostRestoreCommand{big}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "KiB") {
		t.Fatalf("oversized: %v", err)
	}
}

func TestPlanExpandsStarOverTheClones(t *testing.T) {
	cmds := []models.PostRestoreCommand{
		cmd("*", `{"delete": "users", "deletes": [{"q": {"_id": 1}, "limit": 0}]}`),
		cmd("crm", `{"drop": "leads"}`),
		cmd("billing", `{"drop": "invoices"}`), // not restored: does not run
	}
	clones := map[string]string{"shop": "shop_rescue_x", "crm": "crm_rescue_x"}
	steps, err := Plan(cmds, clones)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range steps {
		got = append(got, s.Name+" "+s.Collection+" "+s.Source+"->"+s.Target)
	}
	want := []string{
		"delete users crm->crm_rescue_x",
		"delete users shop->shop_rescue_x",
		"drop leads crm->crm_rescue_x",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("steps = %q; want %q", got, want)
	}
	if steps[2].Index != 1 || string(steps[2].Document) != `{"drop": "leads"}` {
		t.Fatalf("step 3 = %+v", steps[2])
	}
	planned := Planned(steps)
	if len(planned) != 3 || planned[0].Status != models.PostRestoreCommandPlanned || planned[0].Database != "crm_rescue_x" || planned[0].SourceDatabase != "crm" {
		t.Fatalf("planned = %+v", planned)
	}
	if d := Describe(steps[:1]); d != "delete on users in crm_rescue_x" {
		t.Fatalf("Describe = %q", d)
	}
}

func TestPlanNeverTargetsASourceDatabase(t *testing.T) {
	cmds := []models.PostRestoreCommand{cmd("*", `{"drop": "tmp"}`)}
	for name, clones := range map[string]map[string]string{
		"clone is the source":        {"shop": "shop"},
		"clone is another source":    {"shop": "crm", "crm": "crm_rescue_x"},
		"clone is a system database": {"shop": "admin"},
		"no clone name":              {"shop": ""},
	} {
		if steps, err := Plan(cmds, clones); !errors.Is(err, ErrNotClone) || steps != nil {
			t.Errorf("%s: Plan = %v, %v; want ErrNotClone and nothing to run", name, steps, err)
		}
	}
	// A named database runs only in its own clone, never in the database it names.
	steps, err := Plan([]models.PostRestoreCommand{cmd("shop", `{"drop": "tmp"}`)}, map[string]string{"shop": "shop_rescue_x"})
	if err != nil || len(steps) != 1 || steps[0].Target != "shop_rescue_x" {
		t.Fatalf("Plan = %+v, %v", steps, err)
	}
	// Nothing restored: nothing runs.
	if steps, err := Plan(cmds, nil); err != nil || len(steps) != 0 {
		t.Fatalf("Plan without clones = %+v, %v", steps, err)
	}
}

func TestPlanRevalidatesStoredCommands(t *testing.T) {
	_, err := Plan([]models.PostRestoreCommand{cmd("*", `{"eval": "1"}`)}, map[string]string{"shop": "shop_rescue_x"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Plan with a refused command: %v", err)
	}
}
