package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/ws"
)

func TestWSLimitMachineFileAndExitCodes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "init", "-q")
	call := func(args ...string) (string, string, int) {
		t.Helper()
		cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestHelperWS$", "--"}, args...)...)
		cmd.Env = append(os.Environ(), "HRN_WS_HELPER=1")
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		cmd.Run()
		if cmd.ProcessState == nil {
			t.Fatalf("the helper did not start: %s", stderr.String())
		}
		return out.String(), stderr.String(), cmd.ProcessState.ExitCode()
	}
	machine := filepath.Join(home, "ws.json")
	if err := os.WriteFile(machine, []byte(`{"max_slots":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, stderr, code := call("list", "--repo", repo); code != 0 || !strings.HasSuffix(out, "limit: 3 slots ("+machine+")\n") {
		t.Errorf("machine file: %q, %s, exit %d", out, stderr, code)
	}
	// Both refusals of a --max are misuse; a broken settings file is not.
	for _, max := range []string{"5", "11"} {
		if _, stderr, code := call("take", "--repo", repo, "--max", max); code != 2 || !strings.Contains(stderr, "--max "+max+": this repository allows 3 slots ("+machine+")") {
			t.Errorf("--max %s: %s, exit %d; want 2 and the limit with its file", max, stderr, code)
		}
	}
	if err := os.WriteFile(machine, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := call("take", "--repo", repo); code != 1 || !strings.Contains(stderr, machine) {
		t.Errorf("invalid file: %s, exit %d; want 1", stderr, code)
	}
	if _, stderr, code := call("take", "--repo", repo, "--max", "11"); code != 2 {
		t.Errorf("--max 11 with an invalid file: %s, exit %d; want 2", stderr, code)
	}
}

func TestWSListSlotLimit(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "init", "-q")
	path := filepath.Join(repo, ".hrn", "ws.json")
	call := func(args ...string) (string, string, error) {
		t.Helper()
		cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestHelperWS$", "--"}, args...)...)
		cmd.Env = append(os.Environ(), "HRN_WS_HELPER=1")
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		return out.String(), stderr.String(), err
	}
	for _, configured := range []bool{false, true} {
		want := "limit: 5 slots by default, --max up to 10\n"
		if configured {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{"max_slots":3}`), 0o644); err != nil {
				t.Fatal(err)
			}
			want = "limit: 3 slots (" + path + ")\n"
		}
		for _, populated := range []bool{false, true} {
			folder := filepath.Join(repo, ".hrn", "slots", "4")
			if populated {
				if err := os.MkdirAll(folder, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			out, stderr, err := call("list", "--repo", repo)
			if err != nil || !strings.HasSuffix(out, want) {
				t.Errorf("configured=%v populated=%v: %q, %s, %v; want suffix %q", configured, populated, out, stderr, err, want)
			}
			out, stderr, err = call("list", "--repo", repo, "--json")
			rows := []ws.Status{}
			if populated {
				rows = append(rows, ws.Status{N: 4, Path: folder, State: "free", Folder: true, Contents: "not a worktree"})
			}
			encoded, _ := json.MarshalIndent(rows, "", "  ")
			if err != nil || out != string(encoded)+"\n" {
				t.Errorf("JSON = %q, %s, %v; want %s", out, stderr, err, encoded)
			}
			if populated {
				if err := os.Remove(folder); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"list", "--repo", repo}, {"list", "--repo", repo, "--json"}} {
		_, stderr, err := call(args...)
		if err == nil || !strings.Contains(stderr, path) {
			t.Errorf("invalid settings: %s, %v", stderr, err)
		}
	}
}
