package keyrotation_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/keyrotation"
	"github.com/yigitcittan/mongorescue/internal/keywrap"
	"github.com/yigitcittan/mongorescue/internal/keywrap/keywraptest"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

var wrapSpec = keywrap.Spec{Provider: keywrap.ProviderAWSKMS, KeyURI: "alias/mongorescue"}

var wrapRetry = keywrap.Retry{Attempts: 2, Initial: time.Millisecond}

// newWrappedEnv is newEnv with the key files wrapped by kms.
func newWrappedEnv(t *testing.T, kms *keywraptest.Fake) *env {
	t.Helper()
	dir := t.TempDir()
	codec := keywrap.NewCodec(wrapSpec, kms).WithRetry(wrapRetry, keywraptest.NoSleep)
	return &env{dir: dir, files: keyrotation.WrappedFilesIn(dir, codec), db: filepath.Join(dir, "mongorescue.db")}
}

// startWrapped unwraps secret.key.wrapped (creating it) and opens the store.
func (e *env) startWrapped(t *testing.T, kms *keywraptest.Fake) *keyrotation.Opened {
	t.Helper()
	loaded, err := keywrap.Load(context.Background(), keywrap.LoadConfig{DataDir: e.dir, Spec: wrapSpec, Wrapper: kms, Retry: wrapRetry,
		Sleep: keywraptest.NoSleep, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := keyrotation.Open(context.Background(), e.files, loaded.Key, false, e.open, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = opened.Store.Close() })
	return opened
}

func (e *env) unwrap(t *testing.T, path string) []byte {
	t.Helper()
	k, err := e.files.Codec.ReadKey(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// assertNoPlainKeyFiles fails when a plaintext key file exists or any file of
// the directory holds one of keys.
func (e *env) assertNoPlainKeyFiles(t *testing.T, keys ...[]byte) {
	t.Helper()
	entries, err := os.ReadDir(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range entries {
		name := de.Name()
		if strings.HasPrefix(name, secretbox.KeyFileName) && !strings.HasSuffix(name, keywrap.Suffix) {
			t.Errorf("plaintext key file %s exists", name)
		}
		data, _ := os.ReadFile(filepath.Join(e.dir, name))
		for _, k := range keys {
			if bytes.Contains(data, k) || bytes.Contains(data, []byte(secretbox.EncodeKey(k))) {
				t.Errorf("%s holds a key in plain form", name)
			}
		}
	}
}

func TestRotateWrappedKeys(t *testing.T) {
	kms := keywraptest.New("kms")
	e := newWrappedEnv(t, kms)
	opened := e.startWrapped(t, kms)
	seed(t, opened.Store)
	oldKey := opened.Key
	if _, err := rotator(e, opened, nil, nil).Rotate(context.Background()); err != nil {
		t.Fatal(err)
	}
	newKey := e.unwrap(t, e.files.Current)
	if bytes.Equal(newKey, oldKey) || !bytes.Equal(e.unwrap(t, e.files.Previous), oldKey) || exists(t, e.files.Next) {
		t.Fatal("want secret.key.wrapped = new key, secret.key.previous.wrapped = old key and no next file")
	}
	e.assertNoPlainKeyFiles(t, oldKey, newKey)
	_ = opened.Store.Close()
	again := e.startWrapped(t, kms)
	if again.Outcome != keyrotation.OutcomeNone || !bytes.Equal(again.Key, newKey) {
		t.Fatalf("restart = %q", again.Outcome)
	}
	checkSecrets(t, again.Store)
}

func TestWrappedRotationCrashAfterCommitCompletesAtStart(t *testing.T) {
	kms := keywraptest.New("kms")
	e := newWrappedEnv(t, kms)
	opened := e.startWrapped(t, kms)
	seed(t, opened.Store)
	oldKey := opened.Key
	_, err := rotator(e, opened, func(s keyrotation.Step) error {
		if s == keyrotation.StepCommitted {
			return errCrash
		}
		return nil
	}, nil).Rotate(context.Background())
	if !errors.Is(err, errCrash) {
		t.Fatalf("rotate = %v; want the injected crash", err)
	}
	_ = opened.Store.Close()
	again := e.startWrapped(t, kms)
	if again.Outcome != keyrotation.OutcomeCompleted || bytes.Equal(again.Key, oldKey) {
		t.Fatalf("outcome = %q", again.Outcome)
	}
	if !bytes.Equal(e.unwrap(t, e.files.Current), again.Key) || !bytes.Equal(e.unwrap(t, e.files.Previous), oldKey) {
		t.Fatal("the interrupted rotation was not installed")
	}
	e.assertNoPlainKeyFiles(t, oldKey, again.Key)
	checkSecrets(t, again.Store)
}

func TestWrappedRotationWithTheKMSDownChangesNothing(t *testing.T) {
	kms := keywraptest.New("kms")
	e := newWrappedEnv(t, kms)
	opened := e.startWrapped(t, kms)
	seed(t, opened.Store)
	kms.SetDown(-1)
	r := rotator(e, opened, nil, nil)
	if _, err := r.Rotate(context.Background()); !errors.Is(err, keywrap.ErrUnavailable) {
		t.Fatalf("rotate with the KMS down = %v; want ErrUnavailable", err)
	}
	checkSecrets(t, opened.Store)
	if exists(t, e.files.Next) {
		t.Fatal("secret.key.next.wrapped was left behind")
	}
	// The marker went: the rotation can run once the KMS is back.
	kms.SetDown(0)
	if _, err := r.Rotate(context.Background()); err != nil {
		t.Fatalf("rotate after the KMS came back = %v", err)
	}
	checkSecrets(t, opened.Store)
}
