package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestMCPSubcommandErrors(t *testing.T) {
	for _, tc := range []struct {
		args []string
		env  map[string]string
		code int
		want string
	}{
		{nil, nil, 1, EnvMCPAPIKey},
		{[]string{"--url", "ftp://x"}, map[string]string{EnvMCPAPIKey: "mr_k"}, 1, "--url"},
		{nil, map[string]string{EnvMCPAPIKey: "mr_k", EnvMCPURL: "not a url"}, 1, "--url"},
		{[]string{"--bogus"}, nil, 2, "flag provided but not defined"},
		{[]string{"extra"}, nil, 2, "unexpected arguments"},
		{[]string{"--log-level", "loud"}, nil, 2, "log level"},
	} {
		var stdout, stderr bytes.Buffer
		code := runMCP(tc.args, env(tc.env), strings.NewReader(""), &stdout, &stderr)
		if code != tc.code || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("mcp %v = %d, stderr %q; want %d mentioning %q", tc.args, code, stderr.String(), tc.code, tc.want)
		}
		if stdout.Len() != 0 {
			t.Errorf("mcp %v wrote to stdout (the protocol stream): %q", tc.args, stdout.String())
		}
	}
}

func TestMCPSubcommandHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runMCP([]string{"-h"}, env(nil), strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("help exit code = %d", code)
	}
	if !strings.Contains(stderr.String(), EnvMCPAPIKey) || stdout.Len() != 0 {
		t.Fatalf("help must go to stderr and name %s: %q", EnvMCPAPIKey, stderr.String())
	}
}

func TestMCPAPIKeyFlagWarnsAndIsNotLogged(t *testing.T) {
	var stdout, stderr bytes.Buffer
	const key = "mr_abcdefgh_secretsecretsecretsecretsecretse"
	// Nothing listens on port 1, so the bridge fails after reading its flags.
	code := runMCP([]string{"--api-key", key, "--url", "http://127.0.0.1:1"}, env(nil), strings.NewReader(""), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "process list") || strings.Contains(stderr.String(), "secretsecret") {
		t.Fatalf("exit %d, stderr %q; want a warning about the flag and no key in the logs", code, stderr.String())
	}
}
