package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

// An agent gets the operator's environment less what would let a task
// publish or deploy: tokens and keys by name and by pattern, logins on disk
// pointed at an empty place, the ssh agent out of reach, and git and gh set
// up so that pushing and merging fail.
func TestTheGuardedEnvironmentTakesAwayPublishing(t *testing.T) {
	in := []string{
		"PATH=/bin", "HOME=/home/me",
		"GH_TOKEN=ghp_x", "github_token=ghp_y", "CLOUDFLARE_API_TOKEN=cf", "AWS_SECRET_ACCESS_KEY=aws",
		"NPM_TOKEN=npm", "SSH_AUTH_SOCK=/tmp/agent.sock", "ANTHROPIC_API_KEY=sk", "GEMINI_API_KEY=g",
		"OPENROUTER_API_KEY=or", "GITLAB_TOKEN=gl", "MY_SERVICE_SECRET=s", "DB_PASSWORD=p",
		"GH_PAT=pat", "AZURE_DEVOPS_EXT_PAT=ado", "STRIPE_KEY=sk_live", "SIGNING_PRIVATE_KEY_PEM=pem",
		"TF_TOKEN_app_terraform_io=tf", "OP_SESSION_my=op", "BW_SESSION=bw",
		"GIT_ASKPASS=/usr/lib/askpass", "SSH_ASKPASS=/usr/lib/ssh-askpass", "VSCODE_GIT_IPC_HANDLE=/tmp/vscode-git.sock",
		"VSCODE_GIT_ASKPASS_MAIN=/vscode/askpass-main.js",
		"GCM_CREDENTIAL_STORE=secretservice", // where GCM's store is, which a private fetch needs
		"CLAUDE_CODE_OAUTH_TOKEN=claude-plan-login",
		"KUBECONFIG=/home/me/.kube/config", "DOCKER_CONFIG=/home/me/.docker", "npm_config_userconfig=/home/me/.npmrc",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.autocrlf", "GIT_CONFIG_VALUE_0=false",
	}
	out, withheld := GuardEnv(in, "/tmp/hrn-guard")
	got := envMap(out)
	for _, gone := range []string{"GH_TOKEN", "GITHUB_TOKEN", "CLOUDFLARE_API_TOKEN", "AWS_SECRET_ACCESS_KEY", "NPM_TOKEN",
		"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY", "GITLAB_TOKEN", "MY_SERVICE_SECRET", "DB_PASSWORD",
		"GH_PAT", "AZURE_DEVOPS_EXT_PAT", "STRIPE_KEY", "SIGNING_PRIVATE_KEY_PEM", "TF_TOKEN_APP_TERRAFORM_IO", "OP_SESSION_MY", "BW_SESSION",
		"GIT_ASKPASS", "SSH_ASKPASS", "VSCODE_GIT_IPC_HANDLE", "VSCODE_GIT_ASKPASS_MAIN",
		"CLAUDE_CODE_OAUTH_TOKEN"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s is still in the agent's environment", gone)
		}
		if !slices.Contains(withheld, gone) {
			t.Errorf("%s was withheld but is not named among %v", gone, withheld)
		}
	}
	// A CLI's own plan login reaches that CLI, and only when it is asked for.
	kept, withheld := GuardEnv(in, "/tmp/hrn-guard", "CLAUDE_CODE_OAUTH_TOKEN")
	if envMap(kept)["CLAUDE_CODE_OAUTH_TOKEN"] != "claude-plan-login" || slices.Contains(withheld, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Error("claude's plan login was withheld from the claude CLI; it could not use the plan")
	}
	for k, want := range map[string]string{"PATH": "/bin", "HOME": "/home/me", "GCM_CREDENTIAL_STORE": "secretservice",
		"GH_PROMPT_DISABLED": "1", "GIT_TERMINAL_PROMPT": "0", "GCM_INTERACTIVE": "never",
		"GIT_CONFIG_KEY_0": "core.autocrlf", "GIT_CONFIG_VALUE_0": "false"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	// Logins kept on disk are looked for in the guard's empty directory.
	// npm reads its settings case-insensitively, so the operator's lower-case
	// npm_config_userconfig must be gone, not merely outnumbered.
	if v, ok := got["NPM_CONFIG_USERCONFIG"]; !ok || !strings.HasPrefix(filepath.ToSlash(v), "/tmp/hrn-guard") {
		t.Errorf("NPM_CONFIG_USERCONFIG = %q; want the guard's empty place", v)
	}
	for _, kv := range out {
		if strings.HasPrefix(kv, "npm_config_userconfig=") {
			t.Errorf("the operator's %s is still there", kv)
		}
	}
	for _, k := range []string{"GH_CONFIG_DIR", "NPM_CONFIG_USERCONFIG", "KUBECONFIG", "DOCKER_CONFIG", "AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE", "CLOUDSDK_CONFIG", "AZURE_CONFIG_DIR"} {
		if !strings.HasPrefix(filepath.ToSlash(got[k]), "/tmp/hrn-guard") {
			t.Errorf("%s = %q; want a place under the guard's empty directory", k, got[k])
		}
	}
	// The ssh agent: absent, or on Windows a pipe that is not there, since
	// Windows' ssh falls back to its own agent's pipe when the variable is unset.
	if runtime.GOOS == "windows" {
		if got["SSH_AUTH_SOCK"] == "" || got["SSH_AUTH_SOCK"] == `\\.\pipe\openssh-ssh-agent` {
			t.Errorf("SSH_AUTH_SOCK = %q; want a pipe no agent listens on", got["SSH_AUTH_SOCK"])
		}
	} else if _, ok := got["SSH_AUTH_SOCK"]; ok {
		t.Errorf("SSH_AUTH_SOCK = %q; want it unset", got["SSH_AUTH_SOCK"])
	}
	// The operator's own git config entries stay; the guard's follow them, one
	// per kind of address and one for every other address.
	var prefixes []string
	for k, v := range got {
		if strings.HasPrefix(k, "GIT_CONFIG_KEY_") && v == "url.push-refused-by-hrn://.pushinsteadof" {
			prefixes = append(prefixes, got["GIT_CONFIG_VALUE_"+strings.TrimPrefix(k, "GIT_CONFIG_KEY_")])
		}
	}
	slices.Sort(prefixes)
	if strings.Join(prefixes, " ") != " git:// git@ http:// https:// ssh://" {
		t.Errorf("pushInsteadOf prefixes = %q", prefixes)
	}
}

func TestABadGitConfigCountStartsTheGuardsEntriesAtZero(t *testing.T) {
	for _, count := range []string{"-3", "many"} {
		out, _ := GuardEnv([]string{"GIT_CONFIG_COUNT=" + count}, "/tmp/g")
		if got := envMap(out); got["GIT_CONFIG_KEY_0"] != "url.push-refused-by-hrn://.pushinsteadof" {
			t.Errorf("GIT_CONFIG_COUNT=%s: entry 0 = %q; git reads entries from 0", count, got["GIT_CONFIG_KEY_0"])
		}
	}
}

// The guard as git sees it: a push to any kind of address fails at once,
// without reaching the network, and says who refused it.
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
	// No operator config of any kind: the guard alone decides.
	plain := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(dir, "none"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "xdg"), "HOME="+dir, "USERPROFILE="+dir)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"},
		{"remote", "add", "origin", "https://github.com/example/none.git"}, {"remote", "add", "scp", "git@github.com:example/none.git"},
		{"remote", "add", "alias", "github-work:example/none.git"}, {"remote", "add", "upper", "HTTPS://github.com/example/none.git"},
		{"remote", "add", "local", filepath.Join(dir, "elsewhere.git")}} {
		if out, err := run(plain, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	guarded, _ := GuardEnv(plain, filepath.Join(dir, "guard"))
	for _, target := range []string{"origin", "scp", "alias", "upper", "local", "ssh://git@github.com/example/none.git", "http://example.com/x.git"} {
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
