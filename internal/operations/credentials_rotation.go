package operations

import (
	"context"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// credentialRotator rotates storage credentials (implemented by *targets.Service).
type credentialRotator interface {
	RotateCredentials(ctx context.Context, id string, c targets.Credentials) (*targets.RotationResult, error)
}

// RotateStorageCredentials replaces the S3 credentials of storage target id after
// probing them (write, read, list and delete on a temporary object); see
// targets.Service.RotateCredentials. It needs the admin scope. A failed probe
// returns the result with its steps and an error wrapping targets.ErrProbeFailed.
func (s *Service) RotateStorageCredentials(ctx context.Context, id string, c targets.Credentials) (*targets.RotationResult, error) {
	if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
		return nil, err
	}
	r, ok := s.cfg.Targets.(credentialRotator)
	if !ok {
		return nil, public("storage targets are not configured", ErrUnavailable)
	}
	res, err := r.RotateCredentials(ctx, id, c)
	if err != nil {
		return res, err
	}
	s.keyRotated(ctx, KeyKindStorageCredentials, "access key "+res.OldAccessKeyID, "access key "+res.NewAccessKeyID)
	return res, nil
}
