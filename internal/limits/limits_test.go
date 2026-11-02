package limits

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

func TestALimitIsRememberedUntilItLifts(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now()
	resets := now.Add(time.Hour).Truncate(time.Second)
	if err := Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: resets}); err != nil {
		t.Fatal(err)
	}
	if got := Until("codex", now); !got.Equal(resets) {
		t.Errorf("Until = %v, want %v", got, resets)
	}
	if got := Until("codex", now.Add(2*time.Hour)); !got.IsZero() {
		t.Errorf("after the reset Until = %v, want none", got)
	}
	if got := Until("claude", now); !got.IsZero() {
		t.Errorf("claude was never limited, Until = %v", got)
	}
}

func TestOnlyALimitWithATimeIsRemembered(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now()
	for _, res := range []agent.Result{
		{Status: agent.StatusLimited},
		{Status: agent.StatusError, LimitResets: now.Add(time.Hour)},
	} {
		if err := Note("codex", res); err != nil {
			t.Fatal(err)
		}
	}
	if got := Until("codex", now); !got.IsZero() {
		t.Errorf("Until = %v: a limit with no time, or a run that was not limited, is not remembered", got)
	}
}

func TestALaterNoteReplacesAnEarlierOne(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now()
	first, second := now.Add(5*time.Hour).Truncate(time.Second), now.Add(time.Hour).Truncate(time.Second)
	_ = Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: first})
	_ = Note("claude", agent.Result{Status: agent.StatusLimited, LimitResets: first})
	_ = Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: second})
	if got := Until("codex", now); !got.Equal(second) {
		t.Errorf("codex Until = %v, want the latest note %v", got, second)
	}
	if got := Until("claude", now); !got.Equal(first) {
		t.Errorf("claude Until = %v, want %v: one backend's note leaves the other's", got, first)
	}
}

func TestAnUnreadableFileIsNoLimit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	if err := writeFile(home+"/limits.json", []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if got := Until("codex", time.Now()); !got.IsZero() {
		t.Errorf("Until = %v from a broken file", got)
	}
	// Noting starts the file over rather than failing every run.
	if err := Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if Until("codex", time.Now()).IsZero() {
		t.Error("the note after a broken file was not kept")
	}
}

func TestStatusSaysWhatIsRememberedAndHowToForgetIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	now := time.Now()
	if s := Status("codex", now); s != "" {
		t.Errorf("nothing remembered, Status = %q", s)
	}
	_ = Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: now.Add(time.Hour)})
	s := Status("codex", now)
	if !strings.Contains(s, "usage limit until") || !strings.Contains(s, "limits.json") {
		t.Errorf("Status = %q, want the time and the file to delete", s)
	}
}

func TestNotesFromParallelWorkersAreAllKept(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now()
	// A batch's workers note and read limits at once; on Windows a read can
	// also hold the file a rename wants to replace.
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(2)
		name := []string{"codex", "claude"}[i%2]
		go func() {
			defer wg.Done()
			if err := Note(name, agent.Result{Status: agent.StatusLimited, LimitResets: now.Add(time.Hour)}); err != nil {
				t.Errorf("note: %v", err)
			}
		}()
		go func() { defer wg.Done(); Until(name, now) }()
	}
	wg.Wait()
	for _, name := range []string{"codex", "claude"} {
		if Until(name, now).IsZero() {
			t.Errorf("%s's note was lost", name)
		}
	}
}

func TestANullFileIsNoLimit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	if err := writeFile(home+"/limits.json", []byte("null")); err != nil {
		t.Fatal(err)
	}
	if err := Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if Until("codex", time.Now()).IsZero() {
		t.Error("the note after a null file was not kept")
	}
}

// TestHelperNote is not a test: the test binary, run as a second hrn process,
// notes a limit over and over.
func TestHelperNote(t *testing.T) {
	name := os.Getenv("HRN_LIMITS_HELPER")
	if name == "" {
		return
	}
	for i := 0; i < 30; i++ {
		if err := Note(name, agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
	}
	os.Exit(0)
}

func TestNotesFromOtherProcessesDoNotFail(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	var cmds []*exec.Cmd
	for _, name := range []string{"codex", "claude"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperNote$")
		cmd.Env = append(os.Environ(), "HRN_LIMITS_HELPER="+name)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	// Reading meanwhile is what makes Windows refuse a rename.
	for i := 0; i < 100; i++ {
		Until("codex", time.Now())
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Errorf("a process noting a limit failed: %v", err)
		}
	}
}

// A time hrn would not have believed from a CLI is not believed from the file.
func TestARememberedLimitFurtherAheadThanACLICouldSayIsIgnored(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	now := time.Now()
	data := fmt.Sprintf(`{"codex":{"until":%q},"claude":{"until":%q}}`,
		now.Add(agent.MaxLimitWindow+time.Hour).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339))
	if err := writeFile(home+"/limits.json", []byte(data)); err != nil {
		t.Fatal(err)
	}
	if got := Until("codex", now); !got.IsZero() {
		t.Errorf("codex Until = %v, from a time beyond %v ahead", got, agent.MaxLimitWindow)
	}
	if Until("claude", now).IsZero() {
		t.Error("claude's ordinary limit was dropped with it")
	}
}
