package notify

import (
	"testing"

	"github.com/yigitcittan/mongorescue/internal/events"
)

// backup.skipped is opt-in: a rule built from every offered event type never
// receives it, a rule that names it does.
func TestSkippedEventIsOptIn(t *testing.T) {
	all := events.RuleTypes()
	for _, et := range all {
		if et == events.BackupSkipped {
			t.Fatal("RuleTypes offers backup.skipped")
		}
	}
	if !events.BackupSkipped.Subscribable() || !events.BackupSkipped.OptIn() {
		t.Fatal("backup.skipped must stay selectable by name")
	}
	e := events.Event{Type: events.BackupSkipped, JobID: "job_a"}
	everything := &Rule{Enabled: true, Events: all}
	if everything.Matches(e) {
		t.Fatal("a rule of all events received backup.skipped")
	}
	optIn := &Rule{Enabled: true, Events: []events.EventType{events.BackupFailed, events.BackupSkipped}}
	if !optIn.Matches(e) {
		t.Fatal("a rule that names backup.skipped did not receive it")
	}
}
