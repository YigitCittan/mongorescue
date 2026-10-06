package operations

import (
	"context"
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// resolveCopyTargets checks the copy targets ids of a job or backup whose primary
// target is primary and returns them resolved, in order: mode must be a known copy
// mode, at most models.MaxCopyTargets distinct targets other than primary, each one
// a storage target the caller sees (a caller limited to some connections may only
// name the targets it sees, as for primaries). Expected failures: ErrInvalid and
// ErrUnknownStorageTarget.
func (s *Service) resolveCopyTargets(ctx context.Context, ids []string, primary string, mode models.CopyMode) ([]models.CopyTarget, error) {
	if !mode.Valid() {
		return nil, invalid(fmt.Errorf("%w: copy_mode must be %q or %q", models.ErrInvalidCopyTargets, models.CopyAsync, models.CopySync))
	}
	ids, err := models.NormalizeCopyTargets(ids, primary)
	if err != nil {
		return nil, invalid(err)
	}
	out := make([]models.CopyTarget, 0, len(ids))
	for _, id := range ids {
		t, resolveErr := s.ResolveTarget(ctx, id)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if t.ID == primary {
			return nil, invalid(fmt.Errorf("%w: the primary storage target cannot also be a copy target", models.ErrInvalidCopyTargets))
		}
		out = append(out, models.CopyTarget{ID: t.ID, Name: t.Name})
	}
	return out, nil
}
