// name: tool-shapes-defs
// summary: Measure the api loop's tool definitions against the recorded context per model call.
// effects: read, exec
// parameters: none
// example: go test -count=1 -run Definitions ./scripts/tool-shapes
// hash: 938f1ed984269db325f50ba0ead7ea062f5274cc89a38710e63cb1a52519c276
package main

import (
	"encoding/json"
	"errors"
	"sort"

	"github.com/cristiangirlea/harness/internal/agent"
)

// callContexts holds each model call's context (input, cache read and cache
// write tokens) for api runs and for all runs.
type callContexts struct {
	api, all           []float64
	apiRuns, allRuns   int
	apiUsage, allUsage int
	apiRunsWithMCP     int
}

// add counts one value per call, as hrn report's median context does, but reads
// usage events, or else spreads the run's totals over its turns. Codex input
// already includes its cache, so its cache figures are not added.
func (c *callContexts) add(backend string, withMCP bool, events []event, total *int64, cacheWrite int64, turns int) {
	var calls []float64
	fromUsage := false
	for _, e := range events {
		if e.Kind == "usage" && e.Usage != nil {
			v := e.Usage.Input
			if backend != "codex" {
				v += e.Usage.Cache + e.Usage.CacheWrite
			}
			calls = append(calls, float64(v))
			fromUsage = true
		}
	}
	if backend == "codex" {
		cacheWrite = 0
	}
	if !fromUsage && (total != nil || cacheWrite > 0) && turns > 0 {
		run := cacheWrite
		if total != nil {
			run += *total
		}
		for range turns {
			calls = append(calls, float64(run)/float64(turns))
		}
	}
	if len(calls) == 0 {
		return
	}
	c.all = append(c.all, calls...)
	c.allRuns++
	if fromUsage {
		c.allUsage++
	}
	if backend == "api" {
		c.api = append(c.api, calls...)
		c.apiRuns++
		if fromUsage {
			c.apiUsage++
		}
		if withMCP {
			c.apiRunsWithMCP++
		}
	}
}

func median(v []float64) *float64 {
	if len(v) == 0 {
		return nil
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	m := s[len(s)/2]
	if len(s)%2 == 0 {
		m = (s[len(s)/2-1] + m) / 2
	}
	return &m
}

type callMedian struct {
	Runs      int      `json:"runs"`
	UsageRuns int      `json:"runs_from_usage"`
	Calls     int      `json:"calls"`
	Tokens    *float64 `json:"median_tokens"`
}
type definitionSize struct {
	Tools     int      `json:"tools"`
	Bytes     int      `json:"bytes"`
	Tokens    float64  `json:"tokens_estimate"`
	CallShare *float64 `json:"median_api_call_share"`
}
type toolSize struct {
	Tool  string  `json:"tool"`
	Bytes int     `json:"bytes"`
	Share float64 `json:"share"`
}
type definitionsReport struct {
	Days           int                       `json:"days"`
	Definitions    map[string]definitionSize `json:"definitions"`
	Largest        []toolSize                `json:"largest_with_shaping"`
	MCPDefinitions string                    `json:"mcp_definitions"`
	APIRunsWithMCP int                       `json:"api_runs_with_mcp"`
	API            callMedian                `json:"api"`
	All            callMedian                `json:"all"`
}

var definitionModes = []struct {
	name string
	opts agent.Options
}{
	{"readonly", agent.Options{Mode: agent.ModeReadOnly}},
	{"auto", agent.Options{Mode: agent.ModeAuto}},
	{"yolo", agent.Options{Mode: agent.ModeYolo}},
	{"shaping", agent.Options{Mode: agent.ModeYolo, ShapeTools: true}},
}

func definitions(r *report, days int) (*definitionsReport, error) {
	c := r.calls
	d := &definitionsReport{Days: days, Definitions: map[string]definitionSize{}, Largest: []toolSize{},
		MCPDefinitions: "not recorded", APIRunsWithMCP: c.apiRunsWithMCP,
		API: callMedian{Runs: c.apiRuns, UsageRuns: c.apiUsage, Calls: len(c.api), Tokens: median(c.api)},
		All: callMedian{Runs: c.allRuns, UsageRuns: c.allUsage, Calls: len(c.all), Tokens: median(c.all)}}
	b := agent.NewAPIBackend()
	var widest []byte
	for _, m := range definitionModes {
		defs, err := b.ToolDefinitions(agent.Task{Options: m.opts})
		if err != nil {
			return nil, errors.New("cannot build tool definitions")
		}
		var each []json.RawMessage
		if json.Unmarshal(defs, &each) != nil {
			return nil, errors.New("cannot read tool definitions")
		}
		size := definitionSize{Tools: len(each), Bytes: len(defs), Tokens: float64(len(defs)) / 4}
		if d.API.Tokens != nil && *d.API.Tokens > 0 {
			share := size.Tokens / *d.API.Tokens
			size.CallShare = &share
		}
		d.Definitions[m.name] = size
		if m.name == "shaping" {
			widest = defs
		}
	}
	var each []json.RawMessage
	_ = json.Unmarshal(widest, &each)
	for _, raw := range each {
		var t struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &t)
		d.Largest = append(d.Largest, toolSize{Tool: t.Name, Bytes: len(raw), Share: float64(len(raw)) / float64(len(widest))})
	}
	sort.SliceStable(d.Largest, func(i, j int) bool { return d.Largest[i].Bytes > d.Largest[j].Bytes })
	d.Largest = d.Largest[:min(5, len(d.Largest))]
	return d, nil
}

// definitionsFlag takes --definitions out of args, so options stays the
// parser for what both reports share.
func definitionsFlag(args []string) (bool, []string) {
	var rest []string
	found := false
	for _, a := range args {
		if a == "--definitions" {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return found, rest
}
