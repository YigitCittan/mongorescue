package collector

import (
	"context"
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// accessRepo applies the caller's connection access (auth.ConnectionFilter) to the
// stream reads of the service: a stream belongs to its connection, so a caller
// limited to some connections sees only their streams, and another stream reads as
// pitr.ErrNotFound, exactly like one that does not exist (the API answers 404).
// Every use case reads the stream through it before it touches the stream's chains,
// chunks or state. The collector's own work runs without a principal and sees every
// stream.
type accessRepo struct {
	pitr.Repository
}

// hidden is the error of a stream outside the caller's connections.
func hidden(id string) error {
	return fmt.Errorf("%w: stream %s", pitr.ErrNotFound, id)
}

// GetStream returns stream id, or pitr.ErrNotFound when the caller may not touch its
// connection.
func (r accessRepo) GetStream(ctx context.Context, id string) (*pitr.Stream, error) {
	st, err := r.Repository.GetStream(ctx, id)
	if err != nil {
		return nil, err
	}
	if !auth.ConnectionAllowed(ctx, st.ConnectionID) {
		return nil, hidden(id)
	}
	return st, nil
}

// GetStreamByConnection returns the stream of connectionID, or pitr.ErrNotFound when
// the caller may not touch that connection.
func (r accessRepo) GetStreamByConnection(ctx context.Context, connectionID string) (*pitr.Stream, error) {
	if !auth.ConnectionAllowed(ctx, connectionID) {
		return nil, fmt.Errorf("%w: stream of connection %s", pitr.ErrNotFound, connectionID)
	}
	return r.Repository.GetStreamByConnection(ctx, connectionID)
}

// ListStreams returns the streams of the connections the caller may touch.
func (r accessRepo) ListStreams(ctx context.Context) ([]*pitr.Stream, error) {
	list, err := r.Repository.ListStreams(ctx)
	if err != nil || !auth.ConnectionFilter(ctx).Limited() {
		return list, err
	}
	out := make([]*pitr.Stream, 0, len(list))
	for _, st := range list {
		if auth.ConnectionAllowed(ctx, st.ConnectionID) {
			out = append(out, st)
		}
	}
	return out, nil
}
