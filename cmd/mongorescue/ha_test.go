package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/ha"
)

func TestHAForceTakeoverWritesTheRequest(t *testing.T) {
	dir := t.TempDir()
	key := ha.SnapshotKey("prod", time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC), 3)
	env := map[string]string{config.EnvDataDir: dir, config.EnvHACluster: "prod"}
	var out, errOut bytes.Buffer
	code := dispatch([]string{"ha", "force-takeover", "--from-snapshot", key}, func(k string) string { return env[k] }, nil, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), key) {
		t.Fatalf("output %q does not name the snapshot", out.String())
	}
	o, err := ha.TakeOverride(dir)
	if err != nil || o == nil || o.Cluster != "prod" || o.Snapshot != key || o.KeepLocal || o.RequestedBy == "" {
		t.Fatalf("request %+v, %v", o, err)
	}
}

func TestHAForceTakeoverRefusesBadRequests(t *testing.T) {
	dir := t.TempDir()
	noEnv := func(string) string { return "" }
	for name, args := range map[string][]string{
		"no subcommand":  {"ha"},
		"unknown":        {"ha", "take"},
		"no choice":      {"ha", "force-takeover", "--data-dir", dir, "--cluster", "prod"},
		"both":           {"ha", "force-takeover", "--data-dir", dir, "--cluster", "prod", "--keep-local-database", "--from-snapshot", "x"},
		"no cluster":     {"ha", "force-takeover", "--data-dir", dir, "--keep-local-database"},
		"foreign key":    {"ha", "force-takeover", "--data-dir", dir, "--cluster", "prod", "--from-snapshot", "_mongorescue/metadata/ha/other/x.db.age"},
		"extra argument": {"ha", "force-takeover", "--data-dir", dir, "--cluster", "prod", "--keep-local-database", "now"},
	} {
		var out, errOut bytes.Buffer
		if code := dispatch(args, noEnv, nil, &out, &errOut); code != 2 {
			t.Errorf("%s: exit %d; want 2", name, code)
		}
	}
	if o, _ := ha.TakeOverride(dir); o != nil {
		t.Fatalf("a refused request was written: %+v", o)
	}
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"ha", "force-takeover", "--data-dir", dir, "--cluster", "prod", "--keep-local-database"}, noEnv, nil, &out, &errOut); code != 0 {
		t.Fatalf("keep-local: exit %d: %s", code, errOut.String())
	}
	if o, _ := ha.TakeOverride(dir); o == nil || !o.KeepLocal {
		t.Fatalf("keep-local request %+v", o)
	}
}
