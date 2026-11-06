package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/server/platform"
)

func TestRunRejectsMissingWorkerTokenFileBeforeRequiredChecks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-token")
	t.Setenv("HRN_WORKER_TOKEN", "")
	t.Setenv("HRN_WORKER_TOKEN_FILE", path)
	t.Setenv("HRN_CONTROL_URL", "")
	t.Setenv("HRN_FIXTURE_API_URL", "")
	t.Setenv("HRN_PROJECT_ID", "")
	t.Setenv("HRN_WORKSPACE_ROOT", "")
	err := run("hrn")
	if err == nil || !strings.Contains(err.Error(), "HRN_WORKER_TOKEN_FILE") || !strings.Contains(err.Error(), strconv.Quote(path)) || strings.Contains(err.Error(), "are required") {
		t.Fatalf("run() = %v, want missing token file error before required settings check", err)
	}
}

func TestCheckHRN(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-hrn")
	if err := checkHRN(missing, exec.CommandContext); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing binary error = %v, want path %q", err, missing)
	}
	t.Setenv("HRN_TEST_FAKE_HRN", "1")
	command := func(ctx context.Context, path string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, path, append([]string{"-test.run=^TestHelperFakeHRN$", "--"}, args...)...)
	}
	if err := checkHRN(os.Args[0], command); err != nil {
		t.Fatalf("fake binary rejected: %v", err)
	}
}

func TestHelperFakeHRN(t *testing.T) {
	if os.Getenv("HRN_TEST_FAKE_HRN") != "1" {
		return
	}
	if os.Args[len(os.Args)-1] != "version" {
		os.Exit(1)
	}
	fmt.Println("hrn fake version")
	os.Exit(0)
}

func TestHRNHomeCheck(t *testing.T) {
	root := t.TempDir()
	w := platform.Worker{WorkspaceRoot: root}
	if err := w.PrepareHRNHome(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".hrn-home")
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("home %s: %v", path, err)
	}
	bad := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(bad, nil, 0600); err != nil {
		t.Fatal(err)
	}
	w.WorkspaceRoot = bad
	if err := w.PrepareHRNHome(); err == nil || !strings.Contains(err.Error(), filepath.Join(bad, ".hrn-home")) {
		t.Fatalf("home error: %v", err)
	}
}
