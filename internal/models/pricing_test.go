package models

import (
	"errors"
	"math"
	"testing"
)

func TestStoragePricingMath(t *testing.T) {
	p := &StoragePricing{Currency: "USD", StoragePerGiBMonth: 0.023, EgressPerGiB: 0.09}
	for _, tc := range []struct {
		bytes           int64
		storage, egress float64
	}{
		{0, 0, 0},
		{-5, 0, 0},
		{BytesPerGiB, 0.023, 0.09},
		{10 * BytesPerGiB, 0.23, 0.9},
		{BytesPerGiB / 2, 0.0115, 0.045},
		{1, 0, 0}, // a byte rounds to nothing
	} {
		if got := p.StorageCost(tc.bytes); got != tc.storage {
			t.Errorf("StorageCost(%d) = %v, want %v", tc.bytes, got, tc.storage)
		}
		if got := p.EgressCost(tc.bytes); got != tc.egress {
			t.Errorf("EgressCost(%d) = %v, want %v", tc.bytes, got, tc.egress)
		}
	}
	var none *StoragePricing
	if none.StorageCost(BytesPerGiB) != 0 || none.EgressCost(BytesPerGiB) != 0 {
		t.Error("a missing price costs something")
	}
}

func TestStoragePricingValidation(t *testing.T) {
	var none *StoragePricing
	if !none.IsZero() || !(&StoragePricing{Currency: "  "}).IsZero() || (&StoragePricing{Currency: "USD"}).IsZero() || (&StoragePricing{EgressPerGiB: 1}).IsZero() {
		t.Error("IsZero is wrong")
	}
	ok := StoragePricing{Currency: " eur ", StoragePerGiBMonth: 1.5}
	ok.Normalize()
	if ok.Currency != "EUR" || ok.Validate() != nil {
		t.Errorf("a valid price was refused: %+v", ok)
	}
	for _, p := range []StoragePricing{
		{Currency: "EU"}, {Currency: "eur"}, {Currency: "EUR", StoragePerGiBMonth: math.NaN()},
		{Currency: "EUR", EgressPerGiB: math.Inf(1)}, {Currency: "EUR", StoragePerGiBMonth: -0.1},
	} {
		if err := p.Validate(); !errors.Is(err, ErrInvalidPricing) {
			t.Errorf("Validate(%+v) = %v, want ErrInvalidPricing", p, err)
		}
	}
	target := &StorageTarget{Pricing: &ok}
	clone := target.Clone()
	clone.Pricing.Currency = "USD"
	if target.Pricing.Currency != "EUR" {
		t.Error("Clone shares the pricing")
	}
}
