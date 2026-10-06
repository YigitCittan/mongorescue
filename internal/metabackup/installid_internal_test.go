package metabackup

import "testing"

func TestSetInstallIDMovesThePrefix(t *testing.T) {
	s := &Service{installID: "0123456789abcdef"}
	if err := s.SetInstallID("not-an-id"); err == nil {
		t.Fatal("an invalid install ID was accepted")
	}
	if err := s.SetInstallID("fedcba9876543210"); err != nil {
		t.Fatal(err)
	}
	if got := s.Prefix(); got != Prefix+"fedcba9876543210/" {
		t.Fatalf("Prefix = %q", got)
	}
}
