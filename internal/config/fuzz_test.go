package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fuzzEnv parses "NAME=value" lines into a getenv function. The identity file is
// moved into dir, so the fuzzer cannot point the loader at host files.
func fuzzEnv(lines, dir string) func(string) string {
	m := map[string]string{}
	for _, line := range strings.Split(lines, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	if p := m[EnvEncryptionIDFile]; p != "" {
		m[EnvEncryptionIDFile] = filepath.Join(dir, filepath.Base(p))
	}
	return func(k string) string { return m[k] }
}

// FuzzLoadLegacy checks that LoadLegacy never panics on arbitrary config.json content
// and environment, fails only for a config.json that cannot be parsed, and never puts
// a secret into Present or Problems (which are logged).
func FuzzLoadLegacy(f *testing.F) {
	f.Add(`{"server": {"api_key": "file-key-0123456789", "secure_cookies": false, "metrics_public": true},
		"storage": {"type": "local", "local": {"path": "/srv/backups"}},
		"defaults": {"retention_days": 7, "gzip": false},
		"backup": {"timeout": 3600000000000}, "restore": {"verify_before_restore": "ALWAYS"},
		"encryption": {"enabled": true, "recipients": ["age1x"]}, "database_path": "/old/db", "secret_key": "k"}`,
		EnvBackupTimeout+"=3h")
	f.Add(`{"storage": {"type": "s3", "s3": {"bucket": "b", "secret_key": "s3-secret-value-123", "use_path_style": true}},
		"mongo": {"uri": "mongodb://u:mongo-password-123@h/"}, "encryption": {"passphrase": "passphrase-value-123", "identity_file": "$DIR/id.txt"}}`,
		EnvStorageType+"=Nope\n"+EnvTrustProxy+"=yes-please\nAWS_SECRET_ACCESS_KEY=aws-secret-value-123")
	f.Add(`{"storage": {"type": "ftp"}}`, EnvS3PathStyle+"=1\n"+EnvEncryptionIDFile+"=missing.txt")
	f.Add(`{"backup": {"timeout": -9223372036854775808}, "defaults": {"retention_days": 99999999999}}`, "")
	f.Add(`{`, "")
	f.Add(`null`, EnvMongoURI+"=  mongodb://u:env-password-123@h/  \n"+EnvDBPath+"=../../x")
	f.Add(``, "")
	f.Fuzz(func(t *testing.T, file, envLines string) {
		dir := t.TempDir()
		// Keep the run hermetic: identity files must be "$DIR/..." (the test's
		// directory), never host paths (an automounted one could hang).
		file = strings.ReplaceAll(file, "$DIR", filepath.ToSlash(dir))
		var parsed legacyFile
		if json.Unmarshal([]byte(file), &parsed) == nil {
			if p := parsed.Encryption.IdentityFile; p != nil && *p != "" && !strings.HasPrefix(filepath.Clean(*p), dir+string(filepath.Separator)) {
				return
			}
		}
		if file != "" {
			if err := os.WriteFile(filepath.Join(dir, LegacyFileName), []byte(file), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "id.txt"), []byte("AGE-SECRET-KEY-1FUZZ\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		getenv := fuzzEnv(envLines, dir)
		l, err := LoadLegacy(dir, getenv)
		if err != nil {
			if file == "" {
				t.Fatalf("LoadLegacy without config.json: %v", err)
			}
			return
		}

		secrets := []string{l.APIKey, l.MongoURI, l.SecretKey, getenv(EnvEncryptionPass), getenv("AWS_SECRET_ACCESS_KEY")}
		if l.Storage != nil && l.Storage.Target.S3 != nil {
			secrets = append(secrets, l.Storage.Target.S3.SecretAccessKey)
		}
		present, problems := strings.Join(l.Present, "\n"), strings.Join(l.Problems, "\n")
		for _, s := range secrets {
			// Short values can coincide with source names; a value that also appears
			// elsewhere in the input (a path, a storage type) may be echoed from there.
			if len(strings.TrimSpace(s)) < 12 {
				continue
			}
			if strings.Contains(present, s) || (strings.Contains(problems, s) && strings.Count(file+envLines, s) < 2) {
				t.Fatalf("secret %q appears in %q / %q", s, present, problems)
			}
		}
	})
}

// TestLoadLegacyRejectsNonRegularFiles checks that a device or directory configured
// as the identity file is reported as a problem instead of being read (reading
// /dev/zero never ends).
func TestLoadLegacyRejectsNonRegularFiles(t *testing.T) {
	paths := []string{t.TempDir()}
	if runtime.GOOS != "windows" {
		paths = append(paths, "/dev/zero")
	}
	for _, p := range paths {
		l, err := LoadLegacy(t.TempDir(), env(map[string]string{EnvEncryptionIDFile: p}))
		if err != nil {
			t.Fatal(err)
		}
		if len(l.Problems) != 1 || !strings.Contains(l.Problems[0], "not a regular file") {
			t.Errorf("identity file %s: problems = %v", p, l.Problems)
		}
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, LegacyFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLegacy(dir, env(nil)); err == nil {
		t.Error("a config.json directory must be reported")
	}
}
