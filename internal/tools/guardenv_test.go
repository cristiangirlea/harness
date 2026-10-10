package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[strings.ToUpper(k)] = v
	}
	return m
}

// An agent CLI gets the operator's environment less what would let a task
// publish or deploy, and git and gh set up so that pushing and merging fail.
func TestTheGuardedEnvironmentTakesAwayPublishing(t *testing.T) {
	in := []string{
		"PATH=/bin", "HOME=/home/me",
		"GH_TOKEN=ghp_x", "github_token=ghp_y", "CLOUDFLARE_API_TOKEN=cf", "AWS_SECRET_ACCESS_KEY=aws",
		"NPM_TOKEN=npm", "SSH_AUTH_SOCK=/tmp/agent.sock",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.autocrlf", "GIT_CONFIG_VALUE_0=false",
	}
	got := envMap(GuardEnv(in, "/tmp/hrn-gh"))
	for _, gone := range []string{"GH_TOKEN", "GITHUB_TOKEN", "CLOUDFLARE_API_TOKEN", "AWS_SECRET_ACCESS_KEY", "NPM_TOKEN", "SSH_AUTH_SOCK"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s is still in the agent's environment", gone)
		}
	}
	for k, want := range map[string]string{"PATH": "/bin", "HOME": "/home/me", "GH_CONFIG_DIR": "/tmp/hrn-gh",
		"GH_PROMPT_DISABLED": "1", "GIT_TERMINAL_PROMPT": "0",
		"GIT_CONFIG_KEY_0": "core.autocrlf", "GIT_CONFIG_VALUE_0": "false"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	// The operator's own git config entries stay, and the guard's follow them.
	n := got["GIT_CONFIG_COUNT"]
	if n == "1" || n == "" {
		t.Fatalf("GIT_CONFIG_COUNT = %q; want the guard's entries added after the operator's", n)
	}
	var pushes int
	for k, v := range got {
		if strings.HasPrefix(k, "GIT_CONFIG_KEY_") && v == "url.push-refused-by-hrn://.pushinsteadof" {
			pushes++
		}
	}
	if pushes < 3 {
		t.Errorf("%d pushInsteadOf entries; want https, ssh and scp-style addresses covered", pushes)
	}
}

// The guard as git sees it: a push to any address of the usual kinds fails at
// once, without reaching the network, and says who refused it.
func TestGitCannotPushUnderTheGuard(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(env []string, args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	plain := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir, "USERPROFILE="+dir)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"},
		{"remote", "add", "origin", "https://github.com/example/none.git"}, {"remote", "add", "scp", "git@github.com:example/none.git"}} {
		if out, err := run(plain, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	guarded := GuardEnv(plain, filepath.Join(dir, "gh"))
	for _, target := range []string{"origin", "scp", "ssh://git@github.com/example/none.git", "https://github.com/example/other.git"} {
		out, err := run(guarded, "push", target, "main")
		if err == nil || !strings.Contains(out, "push-refused-by-hrn") {
			t.Errorf("git push %s under the guard: %v\n%s", target, err, out)
		}
	}
	// Fetching keeps its address.
	if out, err := run(guarded, "remote", "get-url", "origin"); err != nil || strings.TrimSpace(out) != "https://github.com/example/none.git" {
		t.Errorf("fetch address = %q, %v", out, err)
	}
}
