package archtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixtures(t *testing.T) {
	cases := []struct {
		name     string
		files    map[string]string
		baseline string
		want     string
	}{
		{"ignored directories", map[string]string{".hrn/x/go.mod": "module example.com/root\n", ".hrn/x/internal/unknown/a.go": `package unknown`, "_scratch/internal/unknown/a.go": `package unknown`, "vendor/internal/unknown/a.go": `package unknown`, "testdata/internal/unknown/a.go": `package unknown`}, "", ""},
		{"unclassified import", map[string]string{"internal/mcp/a.go": `package mcp; import _ "example.com/root/internal/unknown"`}, "", "unclassified import: internal/unknown"},
		{"duplicate baseline", map[string]string{"internal/mcp/a.go": `package mcp; import _ "example.com/root/internal/retrieval"`}, "mcp -> retrieval\nmcp -> retrieval\n", "duplicate baseline: mcp -> retrieval"},
		{"allowed and exact baseline", map[string]string{"internal/tools/a.go": `package tools; import _ "example.com/root/internal/mcp"`, "internal/flow/a.go": `package flow; import _ "example.com/root/internal/runlog"`, "internal/runlog/a.go": `package runlog; import (_ "example.com/root/internal/kernel"; _ "example.com/root/internal/agent")`}, "runlog -> agent\n", ""},
		{"kernel denied runlog", map[string]string{"internal/kernel/a.go": `package kernel; import _ "example.com/root/internal/runlog"`}, "", "kernel -> runlog"},
		{"forbidden", map[string]string{"internal/mcp/a.go": `package mcp; import _ "example.com/root/internal/retrieval"`}, "", "mcp -> retrieval"},
		{"stale", map[string]string{"internal/mcp/a.go": `package mcp`}, "mcp -> retrieval\n", "stale"},
		{"unclassified", map[string]string{"internal/unknown/a.go": `package unknown`}, "", "unclassified"},
		{"tagged", map[string]string{"internal/mcp/a_windows.go": "//go:build windows\n\npackage mcp\nimport _ \"example.com/root/internal/retrieval\"\n"}, "", "mcp -> retrieval"},
		{"test edge", map[string]string{"internal/mcp/a.go": `package mcp; import _ "example.com/root/internal/retrieval"`, "internal/mcp/a_test.go": `package mcp_test; import _ "example.com/root/internal/retrieval"`}, "mcp -> retrieval\n", "mcp -> retrieval (test)"},
		{"nested module", map[string]string{"server/go.mod": "module example.com/server\n", "server/platform/a.go": `package platform; import _ "example.com/root/internal/agent"`}, "", "server/platform -> internal/agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{"go.mod": "module example.com/root\n"}
			for k, v := range tc.files {
				files[k] = v
			}
			for k, v := range files {
				p := filepath.Join(root, filepath.FromSlash(k))
				if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(v), 0644); err != nil {
					t.Fatal(err)
				}
			}
			err := check(root, strings.NewReader(tc.baseline))
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRepository(t *testing.T) {
	root := filepath.Join("..", "..")
	f, err := os.Open("baseline.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := check(root, f); err != nil {
		t.Fatal(err)
	}
}
