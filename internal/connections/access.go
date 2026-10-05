package connections

import (
	"context"
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// accessRepo applies the caller's connection access (auth.ConnectionFilter) to every
// read and change of the repository, so each use of the service (the REST API, MCP
// tools, the operations service resolving a connection for a backup or a restore)
// sees only the connections the caller may touch. Another connection answers
// ErrNotFound, exactly like one that does not exist, so its existence is not leaked.
type accessRepo struct {
	Repository
}

// ListConnections returns the connections the caller may touch.
func (r accessRepo) ListConnections(ctx context.Context) ([]*models.Connection, error) {
	list, err := r.Repository.ListConnections(ctx)
	if err != nil {
		return nil, err
	}
	allowed := auth.ConnectionFilter(ctx)
	if !allowed.Limited() {
		return list, nil
	}
	out := make([]*models.Connection, 0, len(list))
	for _, c := range list {
		if allowed.Allows(c.ID) {
			out = append(out, c)
		}
	}
	return out, nil
}

// GetConnection returns connection id, or ErrNotFound when the caller may not touch it.
func (r accessRepo) GetConnection(ctx context.Context, id string) (*models.Connection, error) {
	if !auth.ConnectionAllowed(ctx, id) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return r.Repository.GetConnection(ctx, id)
}

// SaveConnection stores c unless the caller may not touch it.
func (r accessRepo) SaveConnection(ctx context.Context, c *models.Connection) error {
	if !auth.ConnectionAllowed(ctx, c.ID) {
		return fmt.Errorf("%w: %s", ErrNotFound, c.ID)
	}
	return r.Repository.SaveConnection(ctx, c)
}

// DeleteConnection removes connection id unless the caller may not touch it.
func (r accessRepo) DeleteConnection(ctx context.Context, id string) error {
	if !auth.ConnectionAllowed(ctx, id) {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return r.Repository.DeleteConnection(ctx, id)
}
