package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Module is the import path go install builds hrn from.
const Module = "github.com/cristiangirlea/harness/cmd/hrn"

// ErrNoGo is the error when --ref is asked for without a Go toolchain.
var ErrNoGo = errors.New("hrn update --ref needs Go on PATH; use a release instead")

// Builder runs go install. The zero value uses PATH and runs the command;
// tests replace both.
type Builder struct {
	LookPath func(file string) (string, error)
	Run      func(cmd *exec.Cmd) error
}

// BuildRef installs cmd/hrn at ref (a branch, tag or commit) into dir, which
// becomes GOBIN, and returns the binary's path. GOFLAGS is dropped so the
// caller's shell settings do not leak into the build; GOPROXY and the rest
// of the Go environment stay as configured.
func (b Builder) BuildRef(ctx context.Context, ref, dir string) (string, error) {
	if ref == "" {
		return "", errors.New("--ref needs a branch, tag or commit")
	}
	lookPath, run := b.LookPath, b.Run
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if run == nil {
		run = (*exec.Cmd).Run
	}
	goBin, err := lookPath("go")
	if err != nil {
		return "", ErrNoGo
	}
	cmd := exec.CommandContext(ctx, goBin, "install", Module+"@"+ref)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GOFLAGS=") || strings.HasPrefix(kv, "GOBIN=") {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, "GOBIN="+dir)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := run(cmd); err != nil {
		return "", fmt.Errorf("go install %s@%s: %w", Module, ref, err)
	}
	path := filepath.Join(dir, "hrn")
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("go install %s@%s left no binary: %w", Module, ref, err)
	}
	return path, nil
}
