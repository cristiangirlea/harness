package batch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestBatchValidatesCompactWindowsForSpecAndTaskBackends(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, tc := range []struct {
		name, backend, window   string
		taskBackend, taskWindow string
		bad                     bool
	}{
		{"spec claude", "claude", "50000", "", "", true},
		{"task claude", "api", "50000", "claude", "", true},
		{"task override", "api", "200000", "claude", "99999", true},
		{"api minimum", "api", "20480", "", "", false},
		{"claude minimum", "claude", "100000", "", "", false},
		{"api task", "claude", "100000", "api", "20480", false},
		{"every task overrides claude", "claude", "50000", "api", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := Spec{Backend: tc.backend, Options: agent.Options{CompactWindow: agent.CompactWindow(tc.window)},
				Tasks: []Task{{Prompt: "task", Backend: tc.taskBackend, Options: agent.Options{CompactWindow: agent.CompactWindow(tc.taskWindow)}}}}
			err := spec.normalize()
			if tc.bad && (err == nil || !strings.Contains(err.Error(), "compact_window")) || !tc.bad && err != nil {
				t.Fatalf("normalize error=%v want rejection=%v", err, tc.bad)
			}
		})
	}
}

func TestBatchRefusesEffectiveClaudeCompactWindowsBeforeCreatingASlotOrRun(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "routed"}[routed], func(t *testing.T) {
			dir := initRepo(t)
			previous, err := agent.Get("claude")
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			agent.Register(stub{"claude", func(context.Context, agent.Task) agent.Result {
				calls++
				return agent.Result{Status: agent.StatusOK}
			}})
			t.Cleanup(func() { agent.Register(previous) })
			if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"claude"}, DefaultName: "claude"}); err != nil {
				t.Fatal(err)
			}
			if routed {
				if err := os.WriteFile(filepath.Join(runlog.Home(), "route.json"), []byte(`{"review":{"backend":"claude"}}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			res, _ := run(t, Spec{Dir: dir, Options: agent.Options{CompactWindow: "50000", NoSwitch: true},
				Tasks: []Task{{ID: "task", Prompt: "review code", Purpose: "review"}}})
			tr := res.Tasks[0]
			if calls != 0 || len(tr.Attempts) != 0 || tr.BranchMade || !strings.Contains(tr.Error, "compact_window") {
				t.Fatalf("invalid effective window started execution: calls=%d task=%+v", calls, tr)
			}
			if exists(filepath.Join(dir, ".hrn", "slots", "1")) {
				t.Fatal("invalid effective window created a slot")
			}
			if records, err := runlog.List(0); err != nil || len(records) != 0 {
				t.Fatalf("invalid effective window left run records: %+v (%v)", records, err)
			}
		})
	}
}
