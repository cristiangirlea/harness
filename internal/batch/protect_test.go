package batch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

func init() {
	// wb-tamper does to the protected files what its prompt's first word says.
	agent.Register(stub{"wb-tamper", func(_ context.Context, task agent.Task) agent.Result {
		test := filepath.Join(task.Dir, "pkg", "a_test.go")
		switch strings.Fields(typed(task))[0] {
		case "change":
			_ = os.WriteFile(test, []byte("package pkg\n// weakened\n"), 0o644)
		case "remove":
			_ = os.Remove(test)
		case "add":
			_ = os.WriteFile(filepath.Join(task.Dir, "pkg", "b_test.go"), []byte("package pkg\n"), 0o644)
		case "hide":
			// Points git at a clean copy and plants an fsmonitor, then weakens the test.
			_ = exec.Command("git", "-C", task.Dir, "config", "core.fsmonitor", hideMarker).Run()
			_ = exec.Command("git", "-C", task.Dir, "config", "extensions.worktreeConfig", "true").Run()
			_ = exec.Command("git", "-C", task.Dir, "config", "--worktree", "core.worktree", hideClean).Run()
			_ = os.WriteFile(test, []byte("package pkg\n// weakened\n"), 0o644)
		case "commit":
			_ = os.WriteFile(test, []byte("package pkg\n// weakened\n"), 0o644)
			_ = exec.Command("git", "-C", task.Dir, "commit", "-q", "-am", "weaken").Run()
		}
		return agent.Result{Backend: "wb-tamper", Status: agent.StatusOK, Final: "done", Turns: 1}
	}})
}

// protectedRepo is initRepo with a test file and a gate script for the
// protect globs to cover.
func protectedRepo(t *testing.T) string {
	t.Helper()
	dir := initRepo(t)
	for name, body := range map[string]string{"pkg/a_test.go": "package pkg\n", "scripts/check.sh": "exit 0\n"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "tests")
	return dir
}

var protectGlobs = []string{"**/*_test.go", "scripts/check.sh"}

func TestAnAttemptThatChangesAProtectedFileIsFlaggedButPassesByDefault(t *testing.T) {
	for _, how := range []string{"change", "remove", "commit"} {
		t.Run(how, func(t *testing.T) {
			res, events := run(t, Spec{Dir: protectedRepo(t), Backend: "wb-tamper", Protect: protectGlobs,
				Tasks: []Task{{ID: "t", Prompt: how}}})
			tr := res.Tasks[0]
			if tr.Status != StatusPass {
				t.Fatalf("status = %s, want pass under report: %+v", tr.Status, tr)
			}
			if got := tr.Attempts[0].Protected; len(got) != 1 || got[0] != "pkg/a_test.go" {
				t.Errorf("protected = %q, want [pkg/a_test.go]", got)
			}
			if !slices.ContainsFunc(events, func(e Event) bool { return e.Text == "! protected files changed: pkg/a_test.go" }) {
				t.Errorf("events = %+v, want the batch to say which protected file changed", events)
			}
		})
	}
}

func TestAnAttemptThatChangesAProtectedFileFailsUnderFail(t *testing.T) {
	for _, how := range []string{"change", "remove", "commit"} {
		t.Run(how, func(t *testing.T) {
			res, _ := run(t, Spec{Dir: protectedRepo(t), Backend: "wb-tamper", Protect: protectGlobs, ProtectMode: ProtectFail,
				Gate: []string{"never run"}, Tasks: []Task{{ID: "t", Prompt: how}}})
			tr := res.Tasks[0]
			if tr.Status != StatusFail {
				t.Fatalf("status = %s, want fail: %+v", tr.Status, tr)
			}
			gates := tr.Attempts[0].Gates
			if len(gates) != 1 || gates[0].ExitCode == 0 || !strings.HasPrefix(gates[0].Command, "protect") ||
				!strings.Contains(gates[0].Output, "pkg/a_test.go") {
				t.Errorf("gates = %+v, want only the failed protect check naming pkg/a_test.go", gates)
			}
		})
	}
}

func TestAnAddedTestFileIsNotAChangeToAProtectedFile(t *testing.T) {
	res, _ := run(t, Spec{Dir: protectedRepo(t), Backend: "wb-tamper", Protect: protectGlobs, ProtectMode: ProtectFail,
		Tasks: []Task{{ID: "t", Prompt: "add"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts[0].Protected) != 0 {
		t.Errorf("status = %s, protected = %q; a new test file is allowed", tr.Status, tr.Attempts[0].Protected)
	}
}

func TestARetryIsToldWhichProtectedFileItChanged(t *testing.T) {
	var second string
	agent.Register(stub{"wb-retry-seen", func(_ context.Context, task agent.Task) agent.Result {
		second = task.Prompt
		return agent.Result{Backend: "wb-retry-seen", Status: agent.StatusOK, Final: "looked", Turns: 1}
	}})
	run(t, Spec{Dir: protectedRepo(t), Backend: "wb-tamper", Retry: []string{"wb-retry-seen"},
		Protect: protectGlobs, ProtectMode: ProtectFail, Tasks: []Task{{ID: "t", Prompt: "change"}}})
	if !strings.Contains(second, "pkg/a_test.go") || !strings.Contains(second, "protect") {
		t.Errorf("the retry was sent %q, want the protected file it changed", second)
	}
}

func TestATasksProtectSettingsOverrideTheSpecs(t *testing.T) {
	res, _ := run(t, Spec{Dir: protectedRepo(t), Backend: "wb-tamper", Protect: []string{"README.md"}, ProtectMode: ProtectFail,
		Tasks: []Task{{ID: "t", Prompt: "change", Protect: protectGlobs, ProtectMode: ProtectReport}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts[0].Protected) != 1 {
		t.Errorf("status = %s, protected = %q; want the task's globs under report", tr.Status, tr.Attempts[0].Protected)
	}
}

func TestProtectSettingsAreCheckedBeforeAnythingRuns(t *testing.T) {
	for name, spec := range map[string]Spec{
		"mode":      {ProtectMode: "warn", Protect: []string{"a"}},
		"glob":      {Protect: []string{"pkg/[a"}},
		"absolute":  {Protect: []string{"/etc/passwd"}},
		"dot":       {Protect: []string{"./scripts/check.sh"}},
		"trailing":  {Protect: []string{"scripts/"}},
		"empty":     {Protect: []string{"a//b"}},
		"parent":    {Protect: []string{"../x"}},
		"task mode": {Tasks: []Task{{Prompt: "x", ProtectMode: "nope"}}},
	} {
		t.Run(name, func(t *testing.T) {
			spec.Dir, spec.Backend = initRepo(t), "wb-ok"
			if len(spec.Tasks) == 0 {
				spec.Tasks = []Task{{Prompt: "x"}}
			}
			if _, err := Run(context.Background(), spec, nil); err == nil || !strings.Contains(err.Error(), "protect") {
				t.Errorf("err = %v, want a protect error", err)
			}
		})
	}
}

func TestProtectGlobsMatchWholePathSegments(t *testing.T) {
	for _, c := range []struct {
		glob, path string
		want       bool
	}{
		{"**/*_test.go", "a_test.go", true},
		{"**/*_test.go", "x/y/a_test.go", true},
		{"**/*_test.go", "x/a.go", false},
		{"scripts/check.sh", "scripts/check.sh", true},
		{"scripts/check.sh", "other/scripts/check.sh", false},
		{"docs/**", "docs/a/b.md", true},
		{"docs/**", "docsx/a.md", false},
		{"internal/*/testdata/**", "internal/batch/testdata/x.json", true},
	} {
		if got := matchProtect(c.glob, c.path); got != c.want {
			t.Errorf("matchProtect(%q, %q) = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}

// What wb-tamper's hide plants; a test sets them.
var hideMarker, hideClean string

func TestAnAgentsGitConfigCannotHideAChangedProtectedFile(t *testing.T) {
	dir := protectedRepo(t)
	hideClean = filepath.ToSlash(t.TempDir())
	for name, body := range map[string]string{"README.md": "hello\n", "pkg/a_test.go": "package pkg\n", "scripts/check.sh": "exit 0\n"} {
		p := filepath.Join(hideClean, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("HRN_TEST_HOOK_MARKER", marker)
	hideMarker = markerScript(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-tamper", Protect: protectGlobs, ProtectMode: ProtectFail,
		Worktrees: WorktreesFresh, Tasks: []Task{{ID: "t", Prompt: "hide"}}})
	tr := res.Tasks[0]
	if got := tr.Attempts[0].Protected; tr.Status != StatusFail || len(got) != 1 || got[0] != "pkg/a_test.go" {
		t.Fatalf("status = %s, protected = %q; want the weakened test found through the planted config", tr.Status, got)
	}
	assertNotRan(t, marker, "core.fsmonitor while checking protected files", func() {
		_ = exec.Command("git", "-C", dir, "status").Run()
	})
}
