package hrnmcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

// writer writes <first word of the prompt>.txt, so two tasks never touch the
// same file and their branches merge cleanly.
type writer struct{}

func (writer) Name() string            { return "mcp-writer" }
func (writer) Detect() (string, error) { return "stub", nil }
func (writer) Run(_ context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
	name := strings.Fields(task.Prompt)[0] + ".txt"
	emit(agent.Event{Kind: agent.EventToolCall, Tool: "write_file", Text: name})
	_ = os.WriteFile(filepath.Join(task.Dir, name), []byte(task.Prompt+"\n"), 0o644)
	return agent.Result{Backend: "mcp-writer", Status: agent.StatusOK, Final: "wrote " + name, Turns: 1}
}

func init() { agent.Register(writer{}) }

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestBatchShowMerge(t *testing.T) {
	dir := gitRepo(t)
	s := connect(t, dir)

	res := s.call(t, "batch", `{"spec":{"backend":"mcp-writer","gate":["test -f alpha.txt || test -f beta.txt"],
		"tasks":[{"id":"a","prompt":"alpha first"},{"id":"b","prompt":"beta second"}]}}`)
	out := res.Text()
	if res.IsError || !strings.Contains(out, "a: pass on mcp-writer after 1 attempt(s)") || !strings.Contains(out, "2/2 passed") {
		t.Fatalf("batch = %v\n%s", res.IsError, out)
	}
	var id string
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "batch "); ok {
			id = strings.Fields(rest)[0]
		}
	}
	if id == "" {
		t.Fatalf("no batch id in:\n%s", out)
	}
	structured, _ := json.Marshal(res.StructuredContent)
	var got batchBrief
	if err := json.Unmarshal(structured, &got); err != nil || got.ID != id || got.Passed != 2 ||
		len(got.Tasks) != 2 || !got.Tasks[0].Changed || got.Tasks[0].Branch != "hrn/a" || !strings.Contains(got.Next, "batch_merge") {
		t.Errorf("structured content = %s (%v): hosts may show it instead of the text, so it must stand alone", structured, err)
	}
	if strings.Contains(string(structured), "worktree") {
		t.Errorf("structured content carries the full record: %s", structured)
	}
	s.mu.Lock()
	progress := strings.Join(s.progress, "\n")
	s.mu.Unlock()
	if !strings.Contains(progress, "[a] pass") || strings.Contains(progress, "→ write_file") {
		t.Errorf("progress should carry task outcomes but not every tool call:\n%s", progress)
	}

	// The list, one batch's lines, and one task's diff.
	if list := s.call(t, "batch_show", `{}`).Text(); !strings.Contains(list, id) || !strings.Contains(list, "2/2 passed") {
		t.Errorf("list = %s", list)
	}
	if show := s.call(t, "batch_show", `{"batch_id":"`+id+`"}`).Text(); !strings.Contains(show, "b: pass") {
		t.Errorf("show = %s", show)
	}
	diff := s.call(t, "batch_show", `{"batch_id":"`+id+`","task":"a"}`)
	if diff.IsError || !strings.Contains(diff.Text(), "+++ b/alpha.txt") || strings.Contains(diff.Text(), "beta.txt") {
		t.Errorf("diff of a = %s", diff.Text())
	}
	if r := s.call(t, "batch_show", `{"batch_id":"`+id+`","task":"zzz"}`); !r.IsError || !strings.Contains(r.Text(), "no task") {
		t.Errorf("unknown task = %s", r.Text())
	}

	// Land both branches in the main checkout.
	merged := s.call(t, "batch_merge", `{"batch_id":"`+id+`"}`)
	if merged.IsError || !strings.Contains(merged.Text(), "2 branch(es) merged") {
		t.Fatalf("merge = %s", merged.Text())
	}
	for _, f := range []string{"alpha.txt", "beta.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s not in the repository after the merge: %v", f, err)
		}
	}
}

func TestBatchRefusesBadSpecs(t *testing.T) {
	dir := gitRepo(t)
	s := connect(t, dir)
	for _, tc := range []struct{ args, want string }{
		{`{}`, "spec is required"},
		{`{"spec":{"gates":["go test"],"tasks":[{"prompt":"x"}]}}`, `unknown field "gates"`},
		{`{"spec":{"tasks":[{"prompt":"x","expekt":"y"}]}}`, `unknown field "expekt"`},
		{`{"spec":{"backend":"nope","tasks":[{"prompt":"x"}]}}`, "unknown backend"},
		{`{"spec":{"tasks":[]}}`, "no tasks"},
	} {
		if r := s.call(t, "batch", tc.args); !r.IsError || !strings.Contains(r.Text(), tc.want) {
			t.Errorf("%s: %s, want %q", tc.args, r.Text(), tc.want)
		}
	}
	if r := s.call(t, "batch_show", `{"task":"a"}`); !r.IsError {
		t.Error("a task without a batch id is refused")
	}
	if r := s.call(t, "batch_merge", `{"batch_id":"../x"}`); !r.IsError {
		t.Error("a path-like batch id is refused")
	}
}

func TestDecodeSpecResolvesDirAgainstTheWorkspace(t *testing.T) {
	base := t.TempDir()
	abs := t.TempDir()
	for _, tc := range []struct{ dir, want string }{
		{"", base},
		{"sub", filepath.Join(base, "sub")},
		{abs, abs},
	} {
		spec, err := decodeSpec([]byte(`{"dir":`+quote(tc.dir)+`,"tasks":[{"prompt":"x"}]}`), base)
		if err != nil || spec.Dir != tc.want {
			t.Errorf("dir %q: got %q %v, want %q", tc.dir, spec.Dir, err, tc.want)
		}
	}
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }
