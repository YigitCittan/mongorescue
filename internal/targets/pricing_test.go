package targets_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// TestPricingIsValidatedKeptAndCleared proves that a target's price is normalized
// and validated, kept by an update that names none, replaced by one that does and
// removed by an empty one.
func TestPricingIsValidatedKeptAndCleared(t *testing.T) {
	svc, _ := regionFixture(t)
	ctx := context.Background()
	in := awsInput("priced")
	in.Pricing = &models.StoragePricing{Currency: " usd ", StoragePerGiBMonth: 0.023, EgressPerGiB: 0.09}
	created, err := svc.Create(ctx, in)
	if err != nil || created.Pricing == nil || *created.Pricing != (models.StoragePricing{Currency: "USD", StoragePerGiBMonth: 0.023, EgressPerGiB: 0.09}) {
		t.Fatalf("create = %+v, %v; want the normalized price", created, err)
	}

	// No price in the update keeps the stored one.
	in.Pricing = nil
	kept, err := svc.Update(ctx, created.ID, in)
	if err != nil || kept.Pricing == nil || kept.Pricing.Currency != "USD" {
		t.Fatalf("update without pricing = %+v, %v; want the price kept", kept, err)
	}
	if got, _ := svc.Get(ctx, created.ID); got.Pricing == nil || got.Pricing.StoragePerGiBMonth != 0.023 {
		t.Errorf("stored target = %+v", got)
	}

	for name, bad := range map[string]models.StoragePricing{
		"no currency":      {StoragePerGiBMonth: 1},
		"long currency":    {Currency: "USDT", StoragePerGiBMonth: 1},
		"digit currency":   {Currency: "U5D", StoragePerGiBMonth: 1},
		"negative storage": {Currency: "USD", StoragePerGiBMonth: -1},
		"negative egress":  {Currency: "USD", EgressPerGiB: -0.01},
		"huge":             {Currency: "USD", StoragePerGiBMonth: models.MaxPrice + 1},
	} {
		in.Pricing = &bad
		if _, err = svc.Update(ctx, created.ID, in); !errors.Is(err, targets.ErrInvalid) || !errors.Is(err, models.ErrInvalidPricing) {
			t.Errorf("%s: update = %v; want ErrInvalid wrapping ErrInvalidPricing", name, err)
		}
	}

	// A free target is a price too: a currency with prices of 0.
	in.Pricing = &models.StoragePricing{Currency: "EUR"}
	if free, err := svc.Update(ctx, created.ID, in); err != nil || free.Pricing == nil || free.Pricing.Currency != "EUR" {
		t.Errorf("a currency with zero prices = %+v, %v", free, err)
	}

	// An empty object removes the price.
	in.Pricing = &models.StoragePricing{}
	if cleared, err := svc.Update(ctx, created.ID, in); err != nil || cleared.Pricing != nil {
		t.Errorf("an empty price = %+v, %v; want it removed", cleared, err)
	}
}
