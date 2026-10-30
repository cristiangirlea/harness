package eval_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/followup"
	"github.com/cristiangirlea/harness/internal/game"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/route/table"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
	"github.com/cristiangirlea/harness/internal/update"
	"github.com/cristiangirlea/harness/internal/ws"
)

func TestRecordWritersPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes are not enforced on Windows")
	}
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	w, err := runlog.Create("stub", agent.Task{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{}); err != nil {
		t.Fatal(err)
	}
	if err := batch.SaveResult(batch.Result{ID: "batch"}); err != nil {
		t.Fatal(err)
	}
	if err := eval.SavePermissionTestRecord(); err != nil {
		t.Fatal(err)
	}
	if _, err := game.Append([]game.Event{{Key: "event"}}); err != nil {
		t.Fatal(err)
	}
	fw, err := flow.Open(filepath.Join(home, "flows", "flow.jsonl"), flow.Event{})
	if err != nil {
		t.Fatal(err)
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	task := agent.Task{}
	followup.Plan{Fresh: true}.Apply(&task, w.Dir(), func(e agent.Event) { t.Errorf("handoff: %s", e.Text) })
	if _, err := os.Stat(filepath.Join(w.Dir(), "handoff.md")); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(home)
	if err != nil {
		t.Fatal(err)
	}
	workspace.RunDir, workspace.ShapeTools = w.Dir(), true
	for _, tool := range tools.All() {
		if tool.Name == "run_command" {
			out, err := tool.Run(context.Background(), workspace, json.RawMessage(`{"command":"yes x | head -c 40000"}`))
			if err != nil || !strings.Contains(out, "original:") {
				t.Fatalf("original: %v: %s", err, out)
			}
		}
	}
	originals, err := filepath.Glob(filepath.Join(w.Dir(), "originals", "*.txt"))
	if err != nil || len(originals) != 1 {
		t.Fatalf("originals: %v, %v", originals, err)
	}
	if err := limits.Note("stub", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := prefs.Save(home, prefs.Prefs{}); err != nil {
		t.Fatal(err)
	}
	if err := table.Save(home, table.Table{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.TakeIssue(context.Background(), home, ws.IssueRef{Repo: "owner/repo", Number: 1}, ws.Lease{Run: "run"}, ws.IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	_, _ = update.StartDetached(filepath.Join(home, "missing-executable"), nil, filepath.Join(home, "stdout", "watch.log"), filepath.Join(home, "stderr", "watch.log"))
	for _, path := range []string{"limits.json", "backends.json", "route.json", "stdout/watch.log", "stderr/watch.log"} {
		if _, err := os.Stat(filepath.Join(home, path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// The test framework creates the home itself; only what hrn creates inside it counts.
		if path == home {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %04o", path, info.Mode().Perm())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
