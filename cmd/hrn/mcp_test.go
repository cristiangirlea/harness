package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestHelperMCPFlags(t *testing.T) {
	if os.Getenv("HRN_TEST_MCP_FLAGS") != "1" {
		return
	}
	if err := cmdMCP(os.Args[3:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestMCPFlagsReachTheServer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		allow bool
	}{
		{"default", nil, false},
		{"enabled", []string{"--allow-dangerous-runs"}, true},
		{"disabled", []string{"--allow-dangerous-runs=false"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			dir := t.TempDir()
			wantDir, err := filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			args := append([]string{"-test.run=^TestHelperMCPFlags$", "--", "--dir", dir}, tc.flags...)
			client := &mcp.Client{}
			if err := client.Start(ctx, mcp.ServerConfig{
				Command: os.Args[0], Args: args,
				Env: map[string]string{"HRN_TEST_MCP_FLAGS": "1", "HRN_GUARDED": ""},
			}); err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			found := false
			for _, tool := range client.Tools() {
				if tool.Name != "run" {
					continue
				}
				found = true
				var schema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				}
				if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
					t.Fatal(err)
				}
				if got := schema.Properties["allow_dangerous"] != nil; got != tc.allow {
					t.Errorf("schema offers allow_dangerous = %v, want %v", got, tc.allow)
				}
			}
			if !found {
				t.Fatal("server did not offer run")
			}
			result, err := client.CallTool(ctx, "run", json.RawMessage(`{"prompt":"test flags","backend":"stub-cmd","no_switch":true,"allow_dangerous":true}`))
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError == tc.allow {
				t.Fatalf("allow_dangerous request: error=%v, text=%s", result.IsError, result.Text())
			}
			if !tc.allow {
				if !strings.Contains(result.Text(), "only the operator") {
					t.Fatalf("unexpected refusal: %s", result.Text())
				}
				result, err = client.CallTool(ctx, "run", json.RawMessage(`{"prompt":"test workspace","backend":"stub-cmd","no_switch":true}`))
				if err != nil || result.IsError {
					t.Fatalf("guarded run: %v, %s", err, result.Text())
				}
			}
			records, err := runlog.List(0)
			if err != nil || len(records) != 1 {
				t.Fatalf("records: count=%d error=%v", len(records), err)
			}
			gotDir, err := filepath.EvalSymlinks(records[0].Task.Dir)
			if err != nil || gotDir != wantDir || records[0].Task.AllowDangerous != tc.allow {
				t.Errorf("task dir=%q allow_dangerous=%v, want %q %v (error=%v)", gotDir, records[0].Task.AllowDangerous, wantDir, tc.allow, err)
			}
		})
	}
}
