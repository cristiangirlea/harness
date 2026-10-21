package ws

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/gitretry"
)

// Inherited variables must not redirect git away from -C's repository.
var repoVars = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
}

// Drop inherited git -c overrides as well as repository variables.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		drop := upper == "GIT_CONFIG_PARAMETERS" || upper == "GIT_CONFIG_COUNT" ||
			strings.HasPrefix(upper, "GIT_CONFIG_KEY_") || strings.HasPrefix(upper, "GIT_CONFIG_VALUE_")
		for _, v := range repoVars {
			drop = drop || upper == v
		}
		if !drop {
			env = append(env, kv)
		}
	}
	return env
}

func gitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = cleanEnv()
	return cmd
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := gitretry.Output(ctx, func() ([]byte, error) {
		cmd := gitCommand(ctx, dir, args...)
		cmd.Env = append(cmd.Environ(), "LC_ALL=C", "LANGUAGE=")
		return cmd.CombinedOutput()
	})
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return "", errors.New("git " + strings.Join(args, " ") + ": " + text)
	}
	return text, nil
}

func gitStdout(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := gitCommand(ctx, dir, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		text := strings.TrimSpace(stderr.String())
		if text == "" {
			text = err.Error()
		}
		return "", errors.New("git " + strings.Join(args, " ") + ": " + text)
	}
	return string(out), nil
}

// Compare paths after resolving git slashes, Windows case and 8.3 names.
func samePath(p string) string {
	p = filepath.Clean(filepath.FromSlash(p))
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}
