package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/cost"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// CostService is the part of cost.Service the MCP tools use.
type CostService interface {
	// Report computes the storage usage and estimated cost of the last days days.
	Report(ctx context.Context, days int) (*cost.Report, error)
}

// storageUsageInput selects the range of get_storage_usage.
type storageUsageInput struct {
	Days int `json:"days,omitempty" jsonschema:"range of the daily history and of the egress estimate in days, 1-400 (default 30)"`
}

// registerCostTools adds get_storage_usage.
func (s *Server) registerCostTools() {
	addTool(s, &sdk.Tool{
		Name: ToolGetStorageUsage,
		Description: "Show how much MongoRescue stores per storage target (backup archives, copies, retained copies, PITR chunks) with a daily history, " +
			"the largest jobs and databases, an estimated monthly cost from the price entered on the target (if any) and the estimated egress " +
			"of verification, restore tests and copies. An estimate from MongoRescue's own records, not a bill. Not available to keys limited to some connections.",
		Annotations: readOnly("Get storage usage and cost"),
		InputSchema: schemaFor[storageUsageInput](func(p map[string]*jsonschema.Schema) {
			p["days"].Minimum, p["days"].Maximum = ptr(1.0), ptr(float64(cost.MaxDays))
		}),
	}, s.getStorageUsage)
}

func (s *Server) getStorageUsage(ctx context.Context, in storageUsageInput) (*cost.Report, string, error) {
	if s.cfg.Cost == nil {
		return nil, "", fmt.Errorf("%w: the storage usage report is not available", operations.ErrUnavailable)
	}
	if auth.ConnectionFilter(ctx).Limited() {
		return nil, "", auth.ErrConnectionsLimited
	}
	if in.Days < 0 {
		return nil, "", fmt.Errorf("%w: days must be between 1 and %d", errInvalidInput, cost.MaxDays)
	}
	rep, err := s.cfg.Cost.Report(ctx, in.Days)
	if err != nil {
		if errors.Is(err, cost.ErrInvalid) {
			return nil, "", fmt.Errorf("%w: %w", errInvalidInput, err)
		}
		return nil, "", err
	}
	var stored int64
	for _, t := range rep.Targets {
		stored += t.StoredBytes
	}
	return rep, fmt.Sprintf("%d storage target(s) hold %d bytes in total; history of %d day(s). Estimates only, see basis.", len(rep.Targets), stored, rep.Days), nil
}
