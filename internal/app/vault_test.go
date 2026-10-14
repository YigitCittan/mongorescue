package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/vault"
	"github.com/yigitcittan/mongorescue/internal/vaultcreds"
)

type countVault struct{ conns, targets int }

func (c countVault) CountVaultCredentials(context.Context) (int, int, error) {
	return c.conns, c.targets, nil
}

func TestStartNeedsVaultForVaultCredentials(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	cfg := config.Default()
	off, err := openVaultResolver(cfg, func(string) string { return "" }, logger)
	if err != nil || off.Configured() {
		t.Fatalf("without an address = %v, configured %v", err, off.Configured())
	}
	defer off.Close()
	if err = checkVaultUse(ctx, countVault{}, off, logger); err != nil {
		t.Fatalf("nothing uses Vault = %v", err)
	}
	if err = checkVaultUse(ctx, countVault{conns: 1}, off, logger); !errors.Is(err, vaultcreds.ErrNotConfigured) {
		t.Fatalf("a connection uses Vault = %v", err)
	}

	cfg.Vault.Addr = "https://vault.invalid:8200"
	if _, err = openVaultResolver(cfg, func(string) string { return "" }, logger); !errors.Is(err, vault.ErrNoAuth) ||
		!strings.Contains(err.Error(), config.EnvVaultAddr) {
		t.Fatalf("an address without authentication = %v", err)
	}
	on, err := openVaultResolver(cfg, func(k string) string { return map[string]string{vault.EnvToken: "s.tok"}[k] }, logger)
	if err != nil || !on.Configured() {
		t.Fatalf("with a token = %v", err)
	}
	defer on.Close()
	// An unreachable Vault does not stop the start (runs fail and alert).
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err = checkVaultUse(cctx, countVault{targets: 2}, on, logger); err != nil {
		t.Fatalf("unreachable Vault = %v", err)
	}
}
