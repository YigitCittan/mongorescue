package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
)

// JSON-RPC error codes of this server (the implementation-defined server error range).
const (
	// CodeUnauthenticated answers a request without an API key principal.
	CodeUnauthenticated = -32001
	// CodeRateLimited answers a call over the per-key rate limit.
	CodeRateLimited = -32029
)

// MCP methods the middleware treats specially.
const (
	methodCallTool     = "tools/call"
	methodListTools    = "tools/list"
	methodReadResource = "resources/read"
	methodGetPrompt    = "prompts/get"
)

// unknownTool labels calls of tools that do not exist, bounding metric cardinality.
const unknownTool = "unknown"

// principalOf returns the authenticated caller of ctx, or nil.
func principalOf(ctx context.Context) *auth.Principal {
	return auth.PrincipalFrom(ctx)
}

// middleware authenticates, authorises, rate limits, audits and counts MCP requests.
// It is the single place where tool scopes are enforced.
func (s *Server) middleware(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		p := principalOf(ctx)
		if !p.Allows(auth.ScopeRead) {
			return nil, &jsonrpc.Error{Code: CodeUnauthenticated, Message: "unauthenticated: use an API key with at least the read scope"}
		}
		switch method {
		case methodCallTool:
			return s.callTool(ctx, p, next, method, req)
		case methodListTools:
			res, err := next(ctx, method, req)
			if list, ok := res.(*sdk.ListToolsResult); ok && err == nil {
				list.Tools = slices.DeleteFunc(slices.Clone(list.Tools), func(t *sdk.Tool) bool {
					spec, known := s.tools[t.Name]
					return !known || !p.Allows(spec.scope)
				})
			}
			return res, err
		case methodReadResource, methodGetPrompt:
			if ok, wait := s.limiter.allow(p.APIKeyID); !ok {
				return nil, rateLimitError(wait)
			}
			return next(ctx, method, req)
		default:
			return next(ctx, method, req)
		}
	}
}

// callTool handles tools/call: rate limit, scope, then the tool, with one audit
// entry and one metric sample per call. The rate limit comes first so that refused
// calls are bounded too; repeated refusals are coalesced by the audit service.
func (s *Server) callTool(ctx context.Context, p *auth.Principal, next sdk.MethodHandler, method string, req sdk.Request) (sdk.Result, error) {
	start := s.now()
	var name string
	var args json.RawMessage
	if ctr, ok := req.(*sdk.CallToolRequest); ok && ctr.Params != nil {
		name, args = ctr.Params.Name, ctr.Params.Arguments
	}
	entry := audit.Entry{
		Time: start, APIKeyID: p.APIKeyID, APIKeyName: p.APIKeyName, Transport: transportFrom(ctx),
		Tool: truncateName(name), Arguments: args,
	}
	label := name
	spec, known := s.tools[name]
	if !known {
		label = unknownTool
	}
	finish := func(result, errMsg string) {
		entry.Result, entry.Error = result, errMsg
		entry.DurationMS = s.now().Sub(start).Milliseconds()
		s.cfg.Audit.Record(ctx, entry)
		if s.cfg.ObserveCall != nil {
			s.cfg.ObserveCall(label, result)
		}
	}

	if ok, wait := s.limiter.allow(p.APIKeyID); !ok {
		rlErr := rateLimitError(wait)
		finish(audit.ResultRateLimited, rlErr.Message)
		return nil, rlErr
	}
	if known {
		if err := p.Require(spec.scope); err != nil {
			msg := fmt.Sprintf("forbidden: %s needs an API key with the %q scope; this key has %q", name, spec.scope, p.Scope)
			finish(audit.ResultDenied, msg)
			return errorResult(msg), nil
		}
	}

	res, err := next(ctx, method, req)
	switch r, _ := res.(*sdk.CallToolResult); {
	case err != nil:
		finish(audit.ResultError, err.Error())
	case r != nil && r.IsError:
		finish(audit.ResultError, resultText(r))
	default:
		finish(audit.ResultOK, "")
	}
	return res, err
}

// rateLimitError is the JSON-RPC error of a call over the rate limit.
func rateLimitError(wait time.Duration) *jsonrpc.Error {
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return &jsonrpc.Error{Code: CodeRateLimited, Message: fmt.Sprintf("%s: too many calls for this API key; retry in %ds", ErrRateLimited, secs)}
}

// errorResult is a tool result reporting msg as an error the model can read.
func errorResult(msg string) *sdk.CallToolResult {
	r := &sdk.CallToolResult{}
	r.SetError(fmt.Errorf("%s", msg))
	return r
}

// resultText concatenates the text content of r.
func resultText(r *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if t, ok := c.(*sdk.TextContent); ok {
			if b.Len() > 0 {
				b.WriteString(" ")
			}
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// truncateName bounds client-supplied tool names stored in the audit log.
func truncateName(name string) string {
	const maxToolName = 128
	if len(name) > maxToolName {
		name = name[:maxToolName]
	}
	return strings.ToValidUTF8(name, "?")
}
