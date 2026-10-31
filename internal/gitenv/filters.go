package gitenv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// FiltersOff disables workspace-configured filters; filtered files may appear changed.
func FiltersOff(dir string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "--no-optional-locks", "-c", "core.fsmonitor=false", "-C", dir, "config", "--name-only", "--get-regexp", `^filter\.`)
	cmd.Env = append(WithoutRepoVars(os.Environ()), "GIT_NO_LAZY_FETCH=1")
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	env := WithoutRepoVars(os.Environ())
	count := 0
	for _, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		if strings.EqualFold(key, "GIT_CONFIG_COUNT") {
			count, _ = strconv.Atoi(value)
			if count < 0 {
				count = 0
			}
		}
	}
	var off []string
	seen := map[string]bool{}
	for _, key := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		name := strings.TrimPrefix(key, "filter.")
		i := strings.LastIndex(name, ".")
		if i <= 0 || seen[name[:i]] {
			continue
		}
		name = name[:i]
		seen[name] = true
		for _, setting := range []string{"clean", "smudge", "process", "required"} {
			value := ""
			if setting == "required" {
				value = "false"
			}
			off = append(off, fmt.Sprintf("GIT_CONFIG_KEY_%d=filter.%s.%s", count, name, setting), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", count, value))
			count++
		}
	}
	return append(off, fmt.Sprintf("GIT_CONFIG_COUNT=%d", count)), nil
}
