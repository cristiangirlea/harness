package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/ws"
)

func TestHelperWS(t *testing.T) {
	if os.Getenv("HRN_WS_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"hrn", "ws"}, os.Args[i+1:]...)
			main()
			return
		}
	}
	os.Exit(99)
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestWSTakePrintsOnlyTheSlotPathOnStdout(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, origin := filepath.Join(root, "repo"), filepath.Join(root, "origin.git")
	gitT(t, root, "init", "-q", "-b", "main", repo)
	gitT(t, repo, "config", "user.email", "test@example.com")
	gitT(t, repo, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644)
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-q", "-m", "init")
	gitT(t, root, "clone", "-q", "--bare", repo, origin) // origin without a push, which hrn's guard refuses
	gitT(t, repo, "remote", "add", "origin", origin)
	gitT(t, repo, "fetch", "-q", "origin")

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperWS$", "--", "take", "--repo", repo, "--branch", "fix-x", "--for", "test")
	cmd.Env = append(os.Environ(), "HRN_WS_HELPER=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("hrn ws take: %v\n%s", err, stderr.String())
	}
	want := filepath.Join(repo, ".hrn", "slots", "1")
	if got := stdout.String(); got != want+"\n" {
		t.Errorf("stdout %q, want exactly %q and a newline (stderr %q)", got, want, stderr.String())
	}
}

func TestWSRefusesAMaxBelowOne(t *testing.T) {
	code, err := cmdWS([]string{"take", "--max", "0"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--max") {
		t.Errorf("got code %d, error %v; want 2 and an error naming --max", code, err)
	}
}

func TestWSRefusesAMaxAboveTen(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperWS$", "--", "take", "--max", "11")
	cmd.Env = append(os.Environ(), "HRN_WS_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "at most 10 slots per repository") {
		t.Errorf("got error %v and output %q; want non-zero exit and the repository limit", err, out)
	}
}

func TestWSListShowsOneRowPerSlot(t *testing.T) {
	since := time.Date(2026, 10, 13, 9, 30, 0, 0, time.Local)
	rows := []ws.Status{
		{N: 1, Path: "/r/.hrn/slots/1", State: "taken", Holder: "alice", Since: since, Branch: "fix-x", Folder: true, Contents: "clean"},
		{N: 2, Path: "/r/.hrn/slots/2", State: "free", Head: "abc1234", Folder: true, Contents: "clean"},
		{N: 3, Path: "/r/.hrn/slots/3", State: "stale", Holder: "batch", PID: 42, Since: since, Contents: "no folder"},
	}
	var out bytes.Buffer
	printWS(&out, rows)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want one line per slot, got:\n%s", out.String())
	}
	for i, wants := range [][]string{
		{"1", "/r/.hrn/slots/1", "taken", "alice", "2026-10-13 09:30", "fix-x", "clean"},
		{"2", "free", "(detached abc1234)"},
		{"3", "stale", "batch", "no folder"},
	} {
		for _, w := range wants {
			if !strings.Contains(lines[i], w) {
				t.Errorf("line %d %q lacks %q", i+1, lines[i], w)
			}
		}
	}
}
