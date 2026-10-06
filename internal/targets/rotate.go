package targets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// ErrProbeFailed is returned by RotateCredentials when the new credentials fail a
// probe; nothing was changed.
var ErrProbeFailed = errors.New("targets: the new credentials failed the storage probes; nothing was changed")

// Credentials are new S3 credentials for RotateCredentials.
type Credentials struct {
	// AccessKeyID and SecretAccessKey are the new key pair. Both are required.
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

// ProbeStep is one check of the credential probes.
type ProbeStep struct {
	// Name is write, read, list or delete.
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Error is the failure reason (never containing credentials).
	Error string `json:"error,omitempty"`
}

// RotationResult is the outcome of RotateCredentials.
type RotationResult struct {
	// Target is the updated target (secrets masked); nil when nothing changed.
	Target *models.StorageTarget `json:"target,omitempty"`
	// Steps are the probes in the order they ran.
	Steps []ProbeStep `json:"steps"`
	// OldAccessKeyID and NewAccessKeyID name the key pairs (public identifiers).
	OldAccessKeyID string `json:"old_access_key_id"`
	NewAccessKeyID string `json:"new_access_key_id"`
}

// RotateCredentials replaces the S3 credentials of target id. The new credentials
// are first probed on a temporary object below ProbePrefix: write, read back, list
// and delete, each of which MongoRescue needs (backups, restores, scans, retention).
// Only when every probe passes is the target updated, in one write that fails with
// ErrConflict when the target changed meanwhile. A failed probe returns the steps
// and an error wrapping ErrProbeFailed.
func (s *Service) RotateCredentials(ctx context.Context, id string, c Credentials) (*RotationResult, error) {
	existing, err := s.repo.GetStorageTarget(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing.Type != models.StorageS3 || existing.S3 == nil {
		return nil, fmt.Errorf("%w: only S3 targets have credentials to rotate", ErrInvalid)
	}
	c.AccessKeyID, c.SecretAccessKey = strings.TrimSpace(c.AccessKeyID), strings.TrimSpace(c.SecretAccessKey)
	if c.AccessKeyID == "" || c.SecretAccessKey == "" || c.SecretAccessKey == models.SecretMask {
		return nil, fmt.Errorf("%w: access_key_id and secret_access_key are required", ErrInvalid)
	}
	if c.AccessKeyID == existing.S3.AccessKeyID && c.SecretAccessKey == existing.S3.SecretAccessKey {
		return nil, fmt.Errorf("%w: the new credentials are the current ones", ErrInvalid)
	}
	candidate := existing.Clone()
	candidate.S3.AccessKeyID, candidate.S3.SecretAccessKey = c.AccessKeyID, c.SecretAccessKey
	res := &RotationResult{OldAccessKeyID: existing.S3.AccessKeyID, NewAccessKeyID: c.AccessKeyID}
	res.Steps = s.credentialProbes(ctx, candidate)
	for _, st := range res.Steps {
		if !st.OK {
			return res, fmt.Errorf("%w: %s: %s", ErrProbeFailed, st.Name, st.Error)
		}
	}

	now := s.now().UTC()
	candidate.UpdatedAt = now
	if !candidate.UpdatedAt.After(existing.UpdatedAt) {
		candidate.UpdatedAt = existing.UpdatedAt.Add(time.Microsecond)
	}
	candidate.LastTestAt, candidate.LastTestOK, candidate.LastTestError = &now, true, ""
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.repo.UpdateStorageTarget(ctx, candidate, existing.UpdatedAt, false); err != nil {
		return res, err
	}
	delete(s.drivers, candidate.ID)
	res.Target = candidate.Redacted()
	return res, nil
}

// credentialProbes runs the write, read, list and delete probes with t on a
// temporary object. The probe object is removed whatever happens.
func (s *Service) credentialProbes(ctx context.Context, t *models.StorageTarget) []ProbeStep {
	ctx, cancel := context.WithTimeout(ctx, s.testTimeout)
	defer cancel()
	fail := func(steps []ProbeStep, name string, err error) []ProbeStep {
		return append(steps, ProbeStep{Name: name, Error: scrub(err, t)})
	}
	driver, err := s.build(ctx, t)
	if err != nil {
		return fail(nil, "write", err)
	}
	suffix, err := randomHex(8)
	if err != nil {
		return fail(nil, "write", err)
	}
	key := ProbePrefix + "rotate-" + suffix
	payload := []byte("mongorescue credential probe " + suffix)
	var steps []ProbeStep
	if _, err = driver.Save(ctx, key, bytes.NewReader(payload)); err != nil {
		return fail(steps, "write", err)
	}
	deleted := false
	defer func() {
		if !deleted {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = driver.Delete(cleanup, key)
		}
	}()
	steps = append(steps, ProbeStep{Name: "write", OK: true})

	if err = readBack(ctx, driver, key, payload); err != nil {
		return fail(steps, "read", err)
	}
	steps = append(steps, ProbeStep{Name: "read", OK: true})

	list, err := driver.List(ctx, ProbePrefix)
	if err == nil && !containsKey(list, key) {
		err = errors.New("the probe object is missing from the listing")
	}
	if err != nil {
		return fail(steps, "list", err)
	}
	steps = append(steps, ProbeStep{Name: "list", OK: true})

	if err = driver.Delete(ctx, key); err != nil {
		return fail(steps, "delete", err)
	}
	if _, err = driver.Stat(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		if err == nil {
			err = errors.New("the probe object still exists after the delete")
		}
		return fail(steps, "delete", err)
	}
	deleted = true
	return append(steps, ProbeStep{Name: "delete", OK: true})
}

// readBack checks that key holds payload.
func readBack(ctx context.Context, driver storage.Storage, key string, payload []byte) error {
	rc, err := driver.Retrieve(ctx, key)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(io.LimitReader(rc, int64(len(payload))+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(got, payload) {
		return errors.New("content differs from what was written")
	}
	return nil
}

// containsKey reports whether list holds an object whose key ends with key (drivers
// may report keys with or without the target prefix).
func containsKey(list []*models.StorageObject, key string) bool {
	for _, o := range list {
		if o.Key == key || strings.HasSuffix(o.Key, "/"+key) {
			return true
		}
	}
	return false
}
