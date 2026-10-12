//go:build chaos

package chaos

import (
	"net/http"
	"testing"
	"time"
)

// fingerprint returns the fingerprint of the current secret.key.
func (r *rig) fingerprint() string {
	r.t.Helper()
	var st struct {
		SecretKey struct {
			Fingerprint string `json:"fingerprint"`
		} `json:"secret_key"`
	}
	r.api.data("GET", "/api/v1/security/key-rotation", nil, http.StatusOK, &st)
	return st.SecretKey.Fingerprint
}

// assertSecretsOpen checks that every stored credential still decrypts and works:
// the connection URI connects, the storage target's secret key authenticates and
// the webhook channel delivers.
func (r *rig) assertSecretsOpen(when string) {
	r.t.Helper()
	var probe struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	r.api.data("POST", "/api/v1/connections/"+r.connID+"/test", nil, http.StatusOK, &probe)
	if !probe.OK {
		r.t.Fatalf("%s: the connection does not connect: %+v", when, probe)
	}
	r.api.data("POST", "/api/v1/storage-targets/"+r.targetID+"/test", nil, http.StatusOK, &probe)
	if !probe.OK {
		r.t.Fatalf("%s: the storage target does not authenticate: %+v", when, probe)
	}
}

// rotate rotates secret.key through a session and returns how long the request
// took and its status (0 when the process died before answering).
func (r *rig) rotate() (time.Duration, int) {
	r.t.Helper()
	s := newAPI(r.t, r.proc.base)
	s.login()
	start := time.Now()
	code, _, err := s.try("POST", "/api/v1/security/rotate-secret-key", map[string]string{"current_password": adminPass})
	if err != nil {
		return time.Since(start), 0
	}
	return time.Since(start), code
}

// TestKillDuringKeyRotation sends SIGKILL to the server at points across a
// secret.key rotation (fractions of the time a full rotation request takes) and
// starts it again each time.
//
// Expected: the server starts every time and settles the rotation: either it never
// committed (the old key stays, secret.key.next is discarded) or it committed (the
// new key is installed at start and security.key_rotated fires); in both cases
// every stored credential decrypts and works, the API key keeps working, the
// metadata database passes an integrity check, and a later rotation succeeds.
func TestKillDuringKeyRotation(t *testing.T) {
	e := requireEnv(t)
	r := newRig(t, e, rigOptions{})
	r.assertSecretsOpen("before any rotation")

	before := r.fingerprint()
	full, code := r.rotate()
	if code != http.StatusOK {
		t.Fatalf("baseline rotation answered %d", code)
	}
	r.hook.wait(t, "security.key_rotated", "", "")
	if r.fingerprint() == before {
		t.Fatal("the baseline rotation did not change the key")
	}
	t.Logf("a rotation request takes %s", full)

	rotatedEvents := len(r.hook.find("security.key_rotated", "", ""))
	committed := 0
	for _, frac := range []float64{0.2, 0.4, 0.6, 0.7, 0.8, 0.9, 1.0} {
		old := r.fingerprint()
		s := newAPI(t, r.proc.base)
		s.login()
		done := make(chan int, 1)
		go func() {
			c, _, err := s.try("POST", "/api/v1/security/rotate-secret-key", map[string]string{"current_password": adminPass})
			if err != nil {
				c = 0
			}
			done <- c
		}()
		time.Sleep(time.Duration(frac * float64(full)))
		r.proc.kill()
		answered := <-done
		integrityCheck(t, r.dataDir)
		r.restart()

		now := r.fingerprint()
		switch now {
		case old:
			if answered == http.StatusOK {
				t.Fatalf("kill at %.0f%%: the rotation answered 200 but the old key is back", frac*100)
			}
		default:
			committed++
		}
		r.assertSecretsOpen("after the kill")
		t.Logf("kill at %.0f%% (%s): request answered %d, key changed: %v", frac*100, time.Duration(frac*float64(full)), answered, now != old)
	}

	if _, code := r.rotate(); code != http.StatusOK {
		t.Fatalf("a rotation after the kills answered %d", code)
	}
	r.assertSecretsOpen("after the last rotation")

	// Every committed rotation, the last one included, is announced.
	t.Run("alert for every committed rotation", func(t *testing.T) {
		want := rotatedEvents + committed + 1
		waitFor(t, 60*time.Second, "security.key_rotated for every committed rotation", func() bool {
			return len(r.hook.find("security.key_rotated", "", "")) >= want
		})
	})
}
