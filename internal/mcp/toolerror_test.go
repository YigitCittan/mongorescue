package mcp

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/apierr"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

func TestToolErrorCarriesTheCode(t *testing.T) {
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, tc := range []struct {
		err  error
		code string
	}{
		{operations.ErrNotFound, apierr.NotFound},
		{connections.ErrNotFound, apierr.NotFound},
		{operations.ErrInvalid, apierr.InvalidRequest},
		{errInvalidInput, apierr.InvalidRequest},
		{operations.ErrKeyRequired, apierr.ValidationFailed},
		{operations.ErrBusy, apierr.Busy},
		{operations.ErrNotRunning, apierr.Conflict},
		{models.ErrShardedUnsupported, apierr.ShardedUnsupported},
		{auth.ErrForbidden, apierr.Forbidden},
		{operations.ErrUnavailable, apierr.Unavailable},
		{operations.ErrShuttingDown, apierr.Unavailable},
		{&operations.PreflightError{}, apierr.PreflightFailed},
		{errors.New("boom with mongodb://u:pw@h"), apierr.InternalError},
	} {
		err := s.toolError("t", fmt.Errorf("wrapped: %w", tc.err))
		var te *ToolError
		if !errors.As(err, &te) || te.Code != tc.code || !apierr.Known(te.Code) {
			t.Errorf("%v: %v; want code %s", tc.err, err, tc.code)
			continue
		}
		if !strings.HasSuffix(err.Error(), "(code: "+tc.code+")") || strings.Contains(err.Error(), ":pw@") {
			t.Errorf("%v: message %q", tc.err, err.Error())
		}
	}
}
