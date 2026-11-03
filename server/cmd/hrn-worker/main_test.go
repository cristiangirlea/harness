package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
