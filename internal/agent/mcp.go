package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/tools"
)

// Start servers in parallel but emit afterwards, so emit is never concurrent; a failed server is skipped.
func startMCP(ctx context.Context, task Task, root string, emit func(Event)) ([]*mcp.Client, []tools.Tool) {
	cfgs, skipped, err := mcp.LoadConfigs(task.MCP)
	if err != nil {
		emit(Event{Time: time.Now(), Kind: EventError, Text: "mcp config: " + err.Error()})
		return nil, nil
	}
	for _, name := range skipped {
		emit(Event{Time: time.Now(), Kind: EventInfo, Text: "mcp " + name + ": skipped (only stdio servers are supported)"})
	}

	type started struct {
		client *mcp.Client
		err    error
	}
	results := make([]started, len(cfgs))
	var wg sync.WaitGroup
	for i, cfg := range cfgs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg.Dir = root
			// Preserve the guard in child servers so nested hrn cannot lift it.
			if !task.AllowDangerous {
				if cfg.Env == nil {
					cfg.Env = map[string]string{}
				}
				cfg.Env[tools.GuardedVar] = "1"
			}
			cl := &mcp.Client{Info: mcp.Implementation{Name: "hrn", Version: Version}, Roots: []string{root}}
			results[i] = started{cl, cl.Start(ctx, cfg)}
		}()
	}
	wg.Wait()

	var clients []*mcp.Client
	var out []tools.Tool
	for i, r := range results {
		name := cfgs[i].Name
		if r.err != nil {
			emit(Event{Time: time.Now(), Kind: EventError, Text: fmt.Sprintf("mcp %s: %v", name, r.err)})
			continue
		}
		ts := tools.FromMCP(name, r.client)
		names := make([]string, 0, len(ts))
		for _, t := range r.client.Tools() {
			names = append(names, t.Name)
		}
		emit(Event{Time: time.Now(), Kind: EventInfo, Text: fmt.Sprintf("mcp %s: %d tools (%s)", name, len(ts), strings.Join(names, ", "))})
		clients = append(clients, r.client)
		out = append(out, ts...)
	}
	return clients, out
}
