package keyrotation_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/keyrotation"
)

// TestHAModeIsRefused checks that an instance of an active-passive HA cluster
// never rotates secret.key on its own.
func TestHAModeIsRefused(t *testing.T) {
	r := keyrotation.New(keyrotation.Config{HAMode: true})
	if !r.HAMode() {
		t.Fatal("HAMode not reported")
	}
	if _, err := r.Rotate(context.Background()); !errors.Is(err, keyrotation.ErrHAMode) {
		t.Fatalf("rotate in HA mode = %v; want ErrHAMode", err)
	}
}
