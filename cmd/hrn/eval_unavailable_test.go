package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cristiangirlea/harness/internal/batch"
)

func TestEvalUnavailableProviders(t *testing.T) {
	const backend = "agy"
	const reason = "agy not found on PATH or in known install locations"
	unavailable := batch.Attempt{Backend: backend, DidNotStart: reason}
	started := batch.Attempt{Backend: backend}
	canceled := batch.Attempt{Backend: backend, DidNotStart: "context canceled"}
	for _, tc := range []struct {
		name      string
		installed bool
		result    batch.Result
		want      string
	}{
		{"unavailable and started", false, batch.Result{Tasks: []batch.TaskResult{{Attempts: []batch.Attempt{unavailable}}, {Attempts: []batch.Attempt{started}}}}, ""},
		{"canceled with installed backend", true, batch.Result{Tasks: []batch.TaskResult{{Attempts: []batch.Attempt{canceled}}}}, ""},
		{"no attempts", false, batch.Result{Tasks: []batch.TaskResult{{Attempts: []batch.Attempt{unavailable}}, {}}}, ""},
		{"every cell unavailable", false, batch.Result{Tasks: []batch.TaskResult{{Attempts: []batch.Attempt{unavailable}}, {Attempts: []batch.Attempt{unavailable}}}}, backend + ": " + reason},
		{"provider installed after attempt", true, batch.Result{Tasks: []batch.TaskResult{{Attempts: []batch.Attempt{unavailable}}}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PATH", dir)
			t.Setenv("HOME", dir)
			t.Setenv("LOCALAPPDATA", dir)
			if tc.installed {
				name := "agy"
				if runtime.GOOS == "windows" {
					name += ".exe"
				}
				if err := os.WriteFile(filepath.Join(dir, name), nil, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if got := evalUnavailableProviders(tc.result); got != tc.want {
				t.Errorf("evalUnavailableProviders() = %q, want %q", got, tc.want)
			}
		})
	}
}
