package tools

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// The environment an agent CLI, or a command the api backend's run_command
// runs, gets. The command guard reads run_command's command lines; a CLI runs
// its tools itself, so the guard for every backend lives in what it inherits:
//
//   - tokens, keys, secrets and passwords are left out, by name and by
//     pattern, model API keys and git's askpass helpers among them;
//   - the logins tools keep on disk (gh, npm, kubectl, docker, aws, gcloud,
//     az) are looked for in an empty directory instead;
//   - the ssh agent is out of reach;
//   - git rewrites every push address to one it cannot reach.
//
// Like the command guard it stops the plausible accident, not an agent set on
// getting around it: a remote with its own pushurl, a longer pushInsteadOf in
// the repository, an unset GIT_CONFIG_COUNT, an ssh key file on disk and the
// logins kept elsewhere (wrangler's, cargo's, git's credential helper, gh's
// in the system's credential store) are still there. --allow-dangerous
// (Options.AllowDangerous) lifts it, and an hrn run inside it cannot.

// withheldEnv names variables left out besides the patterns in secretEnv:
// credentials whose names say nothing, the ssh agent, and git's askpass
// helpers, which hand out a login when git asks for one (VS Code's among them).
var withheldEnv = []string{
	"AWS_ACCESS_KEY_ID", "GOOGLE_APPLICATION_CREDENTIALS", "AZURE_CLIENT_ID", "AZURE_TENANT_ID",
	"TWINE_USERNAME", "DOCKER_USERNAME", "VAULT_ADDR", "BW_SESSION",
	"SSH_AUTH_SOCK", "GIT_ASKPASS", "SSH_ASKPASS",
}

// secretEnv reports whether a variable's name says it holds a secret, or
// hands one out.
func secretEnv(name string) bool {
	n := strings.ToUpper(name)
	if slices.Contains(withheldEnv, n) {
		return true
	}
	// Git Credential Manager's settings say where its store is, not what is
	// in it (GCM_CREDENTIAL_STORE); without them a private fetch fails.
	if strings.HasPrefix(n, "GCM_") {
		return false
	}
	for _, suffix := range []string{"_TOKEN", "_KEY", "_PAT", "_AUTH"} {
		if strings.HasSuffix(n, suffix) {
			return true
		}
	}
	// _TOKEN_ is Terraform's per-host TF_TOKEN_<host>.
	for _, part := range []string{"SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "PRIVATE_KEY", "_TOKEN_"} {
		if strings.Contains(n, part) {
			return true
		}
	}
	// 1Password's sessions, and VS Code's git login.
	for _, prefix := range []string{"OP_SESSION_", "VSCODE_GIT_"} {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

// redirects are the variables that point tools at their logins, each set to
// a place under dir where there is none.
func redirects(dir string) map[string]string {
	r := map[string]string{
		"GH_CONFIG_DIR":               filepath.Join(dir, "gh"),
		"NPM_CONFIG_USERCONFIG":       filepath.Join(dir, "npm", "npmrc"),
		"KUBECONFIG":                  filepath.Join(dir, "kube", "config"),
		"DOCKER_CONFIG":               filepath.Join(dir, "docker"),
		"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "aws", "credentials"),
		"AWS_CONFIG_FILE":             filepath.Join(dir, "aws", "config"),
		"CLOUDSDK_CONFIG":             filepath.Join(dir, "gcloud"),
		"AZURE_CONFIG_DIR":            filepath.Join(dir, "azure"),
	}
	if runtime.GOOS == "windows" {
		// Windows' ssh falls back to its agent's own pipe when the variable
		// is unset; a pipe nobody listens on keeps it from the operator's keys.
		r["SSH_AUTH_SOCK"] = `\\.\pipe\hrn-no-agent`
	}
	return r
}

// refusedPushScheme is where every push goes instead. git has no helper for
// it, so a push fails at once, and its error names who refused it.
const refusedPushScheme = "push-refused-by-hrn://"

// GuardedVar is set in every guarded environment. An hrn started inside a
// guarded run sees it and refuses to lift the guard: --allow-dangerous is the
// operator's to give, not an agent's, through the shell or an MCP server.
const GuardedVar = "HRN_GUARDED"

// Guarded reports whether this process runs inside a guarded run.
func Guarded() bool { return os.Getenv(GuardedVar) != "" }

// ghNoToken is the token gh gets under the guard. gh keeps its login in the
// system's credential store as well as under GH_CONFIG_DIR, and asks the
// store when no token is set; a token that is not one stops it there.
// `gh auth token --user NAME` still reads the store: the command rules refuse
// it, and codex, gemini and opencode have none.
const ghNoToken = "hrn-guard-no-token"

// fixedEnv are the variables the guard sets whatever the operator's
// environment says.
func fixedEnv() map[string]string {
	return map[string]string{
		GuardedVar:            "1",
		"GH_TOKEN":            ghNoToken,
		"GH_ENTERPRISE_TOKEN": ghNoToken,
	}
}

// GuardEnv returns env for an agent CLI, or a command an agent runs, under
// the guard, and the names of the variables it withheld that were set (for
// the run to say: a CLI that logs in with a key cannot, under the guard).
// dir is an empty directory of the run's own; the logins tools keep on disk
// are looked for under it. keep names secret-looking variables to pass on
// all the same: the plan login of the CLI being started, which needs it.
func GuardEnv(env []string, dir string, keep ...string) (out []string, withheld []string) {
	set := redirects(dir)
	maps.Copy(set, fixedEnv())
	set["GH_PROMPT_DISABLED"] = "1"
	set["GIT_TERMINAL_PROMPT"] = "0"
	set["GCM_INTERACTIVE"] = "never"
	count := 0
	out = make([]string, 0, len(env)+24)
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		key := strings.ToUpper(k) // Windows names are case-insensitive
		switch {
		case key == "GIT_CONFIG_COUNT":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				count = n
			}
		case slices.Contains(keep, key):
			out = append(out, kv)
		case secretEnv(key):
			if v != "" && !slices.Contains(withheld, key) {
				withheld = append(withheld, key)
			}
		case set[key] == "":
			out = append(out, kv)
		}
	}
	// git reads GIT_CONFIG_KEY_n/VALUE_n for n below GIT_CONFIG_COUNT; the
	// operator's own entries stay and the guard's follow them. The empty
	// prefix matches every address the others do not: scp-style aliases,
	// shorthands, upper case, local paths.
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://", "git@", ""} {
		out = append(out,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=url.%s.pushinsteadof", count, refusedPushScheme),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", count, prefix))
		count++
	}
	out = append(out, fmt.Sprintf("GIT_CONFIG_COUNT=%d", count))
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		out = append(out, k+"="+set[k])
	}
	slices.Sort(withheld)
	return out, withheld
}
