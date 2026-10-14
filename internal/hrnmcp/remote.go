package hrnmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/remote"
)

var remoteToolID = regexp.MustCompile("^[0-9a-f]{32}$")
var remoteToolRef = regexp.MustCompile("^[0-9a-f]{40}([0-9a-f]{24})?$")

type remoteScope struct {
	TenantID  string `json:"tenant_id"`
	ProjectID string `json:"project_id"`
}

type remoteJobScope struct {
	remoteScope
	JobID string `json:"job_id"`
}

func (s remoteScope) validate() error {
	if !remoteToolID.MatchString(strings.TrimPrefix(s.TenantID, "t_")) || !strings.HasPrefix(s.TenantID, "t_") ||
		!remoteToolID.MatchString(strings.TrimPrefix(s.ProjectID, "p_")) || !strings.HasPrefix(s.ProjectID, "p_") {
		return errors.New("tenant_id and project_id must be server-issued IDs")
	}
	return nil
}

func (s remoteJobScope) validate() error {
	if err := s.remoteScope.validate(); err != nil {
		return err
	}
	if !strings.HasPrefix(s.JobID, "j_") || !remoteToolID.MatchString(strings.TrimPrefix(s.JobID, "j_")) {
		return errors.New("job_id must be a server-issued ID")
	}
	return nil
}

func (s remoteScope) query() url.Values {
	return url.Values{"tenant_id": {s.TenantID}, "project_id": {s.ProjectID}}
}

func remoteArgs[T any](raw json.RawMessage) (T, error) {
	var args T
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return args, fmt.Errorf("invalid arguments: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return args, errors.New("invalid arguments: extra JSON content")
	}
	return args, nil
}

func remoteResponse(data []byte) (mcp.CallResult, error) {
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return mcp.CallResult{}, fmt.Errorf("invalid control response: %w", err)
	}
	if value == nil {
		return mcp.CallResult{}, errors.New("empty control response")
	}
	return text(string(data), value, false), nil
}

func remoteSchema(properties map[string]any, required ...string) map[string]any {
	req := make([]any, len(required))
	for i, key := range required {
		req[i] = key
	}
	return map[string]any{"type": "object", "properties": properties, "required": req, "additionalProperties": false}
}

func remoteScopeProperties() map[string]any {
	return map[string]any{
		"tenant_id":  str("Server-issued tenant ID. The configured tenant token must authorize it."),
		"project_id": str("Server-issued project ID registered under that tenant."),
	}
}

func remoteJobProperties() map[string]any {
	properties := remoteScopeProperties()
	properties["job_id"] = str("Server-issued job ID.")
	return properties
}

func remoteSubmitTool() mcp.Tool {
	properties := remoteScopeProperties()
	properties["prompt"] = str("Read-only task prompt, at most 100000 bytes.")
	properties["base_ref"] = str("Full Git commit hash registered for the project.")
	properties["idempotency_key"] = str("Stable key for safe submission retry, 1..128 bytes.")
	properties["max_cost_usd"] = map[string]any{"type": "number", "exclusiveMinimum": 0, "description": "Positive per-job spending ceiling in USD."}
	properties["max_turns"] = map[string]any{"type": "integer", "minimum": 1, "description": "Maximum model turns; defaults to 3."}
	return mcp.Tool{
		Name: "remote_submit", Title: "Submit a remote job",
		Description: "Queue one asynchronous read-only API job on a registered project. Returns a durable job ID; execution may take longer. Requires HRN_SERVER_URL and HRN_TENANT_TOKEN on this MCP server.",
		InputSchema: remoteSchema(properties, "tenant_id", "project_id", "prompt", "base_ref", "idempotency_key", "max_cost_usd"),
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(false), IdempotentHint: mcp.Bool(true), OpenWorldHint: mcp.Bool(true)},
		Handler: func(ctx context.Context, call mcp.Call) (mcp.CallResult, error) {
			args, err := remoteArgs[struct {
				remoteScope
				Prompt         string  `json:"prompt"`
				BaseRef        string  `json:"base_ref"`
				IdempotencyKey string  `json:"idempotency_key"`
				MaxCostUSD     float64 `json:"max_cost_usd"`
				MaxTurns       int     `json:"max_turns"`
			}](call.Arguments)
			if err != nil {
				return mcp.CallResult{}, err
			}
			if err := args.remoteScope.validate(); err != nil {
				return mcp.CallResult{}, err
			}
			if len(args.Prompt) == 0 || len(args.Prompt) > 100_000 ||
				!remoteToolRef.MatchString(args.BaseRef) ||
				len(args.IdempotencyKey) == 0 || len(args.IdempotencyKey) > 128 ||
				args.MaxCostUSD <= 0 || args.MaxTurns < 0 {
				return mcp.CallResult{}, errors.New("submit requires prompt (1..100000 bytes), full base_ref, idempotency_key (1..128 bytes), positive max_cost_usd and max_turns")
			}
			if args.MaxTurns == 0 {
				args.MaxTurns = 3
			}
			options := agent.Options{Mode: agent.ModeReadOnly, MaxCostUSD: args.MaxCostUSD, MaxTurns: args.MaxTurns}
			if err := options.Validate(); err != nil {
				return mcp.CallResult{}, err
			}
			client, err := remote.NewFromEnv()
			if err != nil {
				return mcp.CallResult{}, err
			}
			data, err := client.Request(ctx, http.MethodPost, "/v1/projects/"+args.ProjectID+"/jobs", struct {
				TenantID       string        `json:"tenant_id"`
				ProjectID      string        `json:"project_id"`
				Prompt         string        `json:"prompt"`
				Options        agent.Options `json:"options"`
				Backend        string        `json:"backend"`
				BaseRef        string        `json:"base_ref"`
				IdempotencyKey string        `json:"idempotency_key"`
			}{args.TenantID, args.ProjectID, args.Prompt, options, "api", args.BaseRef, args.IdempotencyKey})
			if err != nil {
				return mcp.CallResult{}, err
			}
			return remoteResponse(data)
		},
	}
}

func remoteStatusTool() mcp.Tool {
	return mcp.Tool{
		Name: "remote_status", Title: "Show a remote job",
		Description: "Fetch current state and result for one tenant-scoped asynchronous job.",
		InputSchema: remoteSchema(remoteJobProperties(), "tenant_id", "project_id", "job_id"),
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(true), OpenWorldHint: mcp.Bool(true)},
		Handler: func(ctx context.Context, call mcp.Call) (mcp.CallResult, error) {
			args, err := remoteArgs[remoteJobScope](call.Arguments)
			if err != nil {
				return mcp.CallResult{}, err
			}
			if err := args.validate(); err != nil {
				return mcp.CallResult{}, err
			}
			client, err := remote.NewFromEnv()
			if err != nil {
				return mcp.CallResult{}, err
			}
			data, err := client.Request(ctx, http.MethodGet, "/v1/jobs/"+args.JobID+"?"+args.query().Encode(), nil)
			if err != nil {
				return mcp.CallResult{}, err
			}
			return remoteResponse(data)
		},
	}
}

func remoteEventsTool() mcp.Tool {
	properties := remoteJobProperties()
	properties["after"] = map[string]any{"type": "integer", "minimum": 0, "description": "Last durable event ID already seen; defaults to 0. Returns up to 100 subsequent events."}
	return mcp.Tool{
		Name: "remote_events", Title: "Read remote job events",
		Description: "Read the next page of durable events for one tenant-scoped job. Use the last event ID as after for the next call.",
		InputSchema: remoteSchema(properties, "tenant_id", "project_id", "job_id"),
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(true), OpenWorldHint: mcp.Bool(true)},
		Handler: func(ctx context.Context, call mcp.Call) (mcp.CallResult, error) {
			args, err := remoteArgs[struct {
				remoteJobScope
				After int64 `json:"after"`
			}](call.Arguments)
			if err != nil {
				return mcp.CallResult{}, err
			}
			if err := args.validate(); err != nil {
				return mcp.CallResult{}, err
			}
			if args.After < 0 {
				return mcp.CallResult{}, errors.New("after must be nonnegative")
			}
			query := args.query()
			query.Set("after", strconv.FormatInt(args.After, 10))
			client, err := remote.NewFromEnv()
			if err != nil {
				return mcp.CallResult{}, err
			}
			data, err := client.Request(ctx, http.MethodGet, "/v1/jobs/"+args.JobID+"/events?"+query.Encode(), nil)
			if err != nil {
				return mcp.CallResult{}, err
			}
			return remoteResponse(data)
		},
	}
}

func remoteCancelTool() mcp.Tool {
	return mcp.Tool{
		Name: "remote_cancel", Title: "Cancel a remote job",
		Description: "Request cancellation of one tenant-scoped asynchronous job. A running worker acknowledges cancellation on its next heartbeat.",
		InputSchema: remoteSchema(remoteJobProperties(), "tenant_id", "project_id", "job_id"),
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(false), IdempotentHint: mcp.Bool(true), OpenWorldHint: mcp.Bool(true)},
		Handler: func(ctx context.Context, call mcp.Call) (mcp.CallResult, error) {
			args, err := remoteArgs[remoteJobScope](call.Arguments)
			if err != nil {
				return mcp.CallResult{}, err
			}
			if err := args.validate(); err != nil {
				return mcp.CallResult{}, err
			}
			client, err := remote.NewFromEnv()
			if err != nil {
				return mcp.CallResult{}, err
			}
			data, err := client.Request(ctx, http.MethodPost, "/v1/jobs/"+args.JobID+"/cancel?"+args.query().Encode(), nil)
			if err != nil {
				return mcp.CallResult{}, err
			}
			return remoteResponse(data)
		},
	}
}
