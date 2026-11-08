package models

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// BytesPerGiB is the size of a GiB: prices are per GiB (2^30 bytes), the unit
// providers bill in.
const BytesPerGiB = 1 << 30

// MaxPrice is the largest price per GiB a target accepts: a guard against typos
// (and values that overflow arithmetic), not a business rule.
const MaxPrice = 1_000_000

// ErrInvalidPricing is wrapped by StoragePricing.Validate.
var ErrInvalidPricing = errors.New("invalid pricing")

// StoragePricing is the price an operator entered for a storage target, to turn
// stored bytes and egress into an estimate (internal/cost). It is configuration,
// not a secret, and MongoRescue never asks a provider for its real prices.
type StoragePricing struct {
	// Currency is the ISO 4217 code of both prices ("USD", "EUR"); required.
	Currency string `json:"currency"`
	// StoragePerGiBMonth is the price of keeping one GiB for a month.
	StoragePerGiBMonth float64 `json:"storage_per_gib_month"`
	// EgressPerGiB is the price of reading one GiB back out of the target.
	EgressPerGiB float64 `json:"egress_per_gib"`
}

// IsZero reports whether p sets nothing: no currency and no price.
func (p *StoragePricing) IsZero() bool {
	return p == nil || (strings.TrimSpace(p.Currency) == "" && p.StoragePerGiBMonth == 0 && p.EgressPerGiB == 0)
}

// Normalize trims and upper-cases the currency code.
func (p *StoragePricing) Normalize() {
	if p != nil {
		p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	}
}

// Validate checks a normalized, non-zero pricing: a three-letter currency code and
// finite prices between 0 and MaxPrice.
func (p *StoragePricing) Validate() error {
	if len(p.Currency) != 3 || strings.ContainsFunc(p.Currency, func(r rune) bool { return r < 'A' || r > 'Z' }) {
		return fmt.Errorf("%w: currency must be a three-letter code such as USD or EUR", ErrInvalidPricing)
	}
	for _, f := range []struct {
		name string
		v    float64
	}{{"storage_per_gib_month", p.StoragePerGiBMonth}, {"egress_per_gib", p.EgressPerGiB}} {
		if math.IsNaN(f.v) || math.IsInf(f.v, 0) || f.v < 0 || f.v > MaxPrice {
			return fmt.Errorf("%w: %s must be between 0 and %d", ErrInvalidPricing, f.name, MaxPrice)
		}
	}
	return nil
}

// StorageCost is the cost of holding bytes for a month at p, in p's currency.
func (p *StoragePricing) StorageCost(bytes int64) float64 {
	if p == nil || bytes <= 0 {
		return 0
	}
	return roundMoney(float64(bytes) / BytesPerGiB * p.StoragePerGiBMonth)
}

// EgressCost is the cost of reading bytes out of the target at p, in p's currency.
func (p *StoragePricing) EgressCost(bytes int64) float64 {
	if p == nil || bytes <= 0 {
		return 0
	}
	return roundMoney(float64(bytes) / BytesPerGiB * p.EgressPerGiB)
}

// roundMoney rounds to four decimals: enough for sub-cent amounts, stable in JSON.
func roundMoney(v float64) float64 { return math.Round(v*1e4) / 1e4 }
