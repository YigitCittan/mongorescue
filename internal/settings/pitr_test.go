package settings

import (
	"context"
	"errors"
	"math"
	"testing"
)

func TestPITRBaseWindowRiskRatio(t *testing.T) {
	svc := newSvc(t, &memRepo{})
	ctx := context.Background()
	if got := svc.Current().PITR.BaseWindowRiskRatio; got != 0.5 {
		t.Fatalf("default = %v; want 0.5", got)
	}
	for _, v := range []float64{0, -0.1, 1.01, 5, math.NaN()} {
		if _, err := svc.Update(ctx, Patch{PITR: &PITRPatch{BaseWindowRiskRatio: ptr(v)}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("Update(%v) = %v; want ErrInvalid", v, err)
		}
	}
	if _, err := svc.Update(ctx, Patch{PITR: &PITRPatch{BaseWindowRiskRatio: ptr(0.25)}}); err != nil {
		t.Fatal(err)
	}
	if got := svc.Current().PITR.BaseWindowRiskRatio; got != 0.25 {
		t.Fatalf("ratio = %v; want 0.25", got)
	}
	// A patch without the group keeps it.
	if _, err := svc.Update(ctx, Patch{}); err != nil {
		t.Fatal(err)
	}
	if got := svc.Current().PITR.BaseWindowRiskRatio; got != 0.25 {
		t.Fatalf("ratio after an empty patch = %v", got)
	}
}
