package vaultcreds

import (
	"context"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// TestTimeout bounds a test of a Vault reference.
const TestTimeout = 15 * time.Second

// TestResult is the outcome of a test of a Vault reference. It never holds a
// credential, only what was read and how.
type TestResult struct {
	// OK reports whether the credentials were resolved.
	OK bool `json:"ok"`
	// Engine and Source describe the reference ("kv-v2 secret secret/data/x").
	Engine models.VaultEngine `json:"engine,omitempty"`
	Source string             `json:"source,omitempty"`
	// Fields lists the kv-v2 fields that were found (names only).
	Fields []string `json:"fields,omitempty"`
	// Version is the kv-v2 version read.
	Version int `json:"version,omitempty"`
	// LeaseTTLSeconds and Renewable describe the lease of dynamic credentials;
	// the test revokes it right away.
	LeaseTTLSeconds int64 `json:"lease_ttl_seconds,omitempty"`
	Renewable       bool  `json:"renewable,omitempty"`
	// LatencyMS is the duration of the test in milliseconds.
	LatencyMS int64 `json:"latency_ms"`
	// Error is the redacted failure reason.
	Error string `json:"error,omitempty"`
}

// Test resolves ref for use without caching or keeping anything: kv-v2 fields
// are read and checked, dynamic credentials are issued and revoked at once. A
// failure is reported in the result (an invalid reference as an error).
func (r *Resolver) Test(ctx context.Context, use models.VaultUse, ref models.VaultRef) (TestResult, error) {
	if err := ref.Normalize(use); err != nil {
		return TestResult{}, err
	}
	res := TestResult{Engine: ref.Engine, Source: ref.String()}
	if err := r.ready(); err != nil {
		res.Error = err.Error()
		return res, nil
	}
	ctx, cancel := context.WithTimeout(ctx, TestTimeout)
	defer cancel()
	start := r.now()
	if ref.Engine == models.VaultKV2 {
		fields := Fields(ref)
		_, version, err := r.readKV(ctx, ref, fields)
		res.LatencyMS = r.now().Sub(start).Milliseconds()
		if err != nil {
			res.Error = redact.Text(err.Error())
			return res, nil
		}
		res.OK, res.Fields, res.Version = true, fields, version
		return res, nil
	}
	l, err := r.issue(ctx, ref)
	res.LatencyMS = r.now().Sub(start).Milliseconds()
	if err != nil {
		res.Error = redact.Text(err.Error())
		return res, nil
	}
	res.OK, res.Renewable, res.LeaseTTLSeconds = true, l.renewable, int64(l.ttl0/r.unit)
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	r.revoke(rctx, l.id)
	rcancel()
	return res, nil
}
