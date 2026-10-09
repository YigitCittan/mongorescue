package settings

import (
	"slices"
	"strings"
	"testing"
)

func warningByID(s *Service, id string) (Warning, bool) {
	for _, w := range s.Warnings() {
		if w.ID == id {
			return w, true
		}
	}
	return Warning{}, false
}

// The runtime warnings (a full data directory, unreadable notification channels)
// follow their setters and are not stored.
func TestRuntimeWarnings(t *testing.T) {
	svc := newSvc(t, &memRepo{})
	svc.SetDiskFullWarning(true)
	if w, ok := warningByID(svc, WarningDiskFull); !ok || w.Message == "" {
		t.Fatalf("disk full warning = %+v, %v", w, ok)
	}
	svc.SetDiskFullWarning(false)
	if _, ok := warningByID(svc, WarningDiskFull); ok {
		t.Fatal("disk full warning still shown")
	}

	svc.SetChannelUnreadable("ch_b", "channel unreadable: decrypt failed")
	svc.SetChannelUnreadable("ch_a", "channel unreadable: decrypt failed")
	w, ok := warningByID(svc, WarningChannelUnreadable)
	if !ok || !slices.Equal(w.Channels, []string{"ch_a", "ch_b"}) || !strings.Contains(w.Message, "ch_a, ch_b") {
		t.Fatalf("channel warning = %+v, %v", w, ok)
	}
	svc.SetChannelUnreadable("ch_a", "")
	svc.SetChannelUnreadable("ch_b", "")
	if _, ok := warningByID(svc, WarningChannelUnreadable); ok {
		t.Fatal("channel warning still shown")
	}
}
