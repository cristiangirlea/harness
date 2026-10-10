package tools

import (
	"fmt"
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
//     pattern, model API keys among them;
//   - the logins tools keep on disk (gh, kubectl, docker, aws, gcloud, az) are
//     looked for in an empty directory instead;
//   - the ssh agent is out of reach;
//   - git rewrites every push address to one it cannot reach.
//
// Like the command guard it stops the plausible accident, not an agent set on
// getting around it: a remote with its own pushurl, an operator's longer
// pushInsteadOf, an ssh key file on disk and logins kept elsewhere (wrangler's)
// are still there. --allow-dangerous (Options.AllowDangerous) lifts it.

// withheldEnv names variables left out besides the patterns in secretEnv:
// credentials whose names say nothing, and the ssh agent.
var withheldEnv = []string{
	"AWS_ACCESS_KEY_ID", "GOOGLE_APPLICATION_CREDENTIALS", "AZURE_CLIENT_ID", "AZURE_TENANT_ID",
	"TWINE_USERNAME", "DOCKER_USERNAME", "VAULT_ADDR",
	"SSH_AUTH_SOCK",
}

// keptEnv are secret-looking names the CLIs need for their own plan login.
var keptEnv = []string{"CLAUDE_CODE_OAUTH_TOKEN"}

// secretEnv reports whether a variable's name says it holds a secret.
func secretEnv(name string) bool {
	n := strings.ToUpper(name)
	if slices.Contains(keptEnv, n) {
		return false
	}
	if slices.Contains(withheldEnv, n) {
		return true
	}
	for _, suffix := range []string{"_TOKEN", "_API_KEY", "_ACCESS_KEY", "_AUTH"} {
		if strings.HasSuffix(n, suffix) {
			return true
		}
	}
	for _, part := range []string{"SECRET", "PASSWORD", "PASSWD", "CREDENTIAL"} {
		if strings.Contains(n, part) {
			return true
		}
	}
	return false
}

// refusedPushScheme is where every push goes instead. git has no helper for
// it, so a push fails at once, and its error names who refused it.
const refusedPushScheme = "push-refused-by-hrn://"

// GuardEnv returns env for an agent CLI, or a command an agent runs, under
// the guard, and the names of the variables it withheld that were set (for
// the run to say: a CLI that logs in with a key cannot, under the guard).
// dir is an empty directory of the run's own; the logins tools keep on disk
// are looked for under it.
func GuardEnv(env []string, dir string) (out []string, withheld []string) {
	set := map[string]string{
		"GH_CONFIG_DIR":               filepath.Join(dir, "gh"),
		"KUBECONFIG":                  filepath.Join(dir, "kube", "config"),
		"DOCKER_CONFIG":               filepath.Join(dir, "docker"),
		"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "aws", "credentials"),
		"AWS_CONFIG_FILE":             filepath.Join(dir, "aws", "config"),
		"CLOUDSDK_CONFIG":             filepath.Join(dir, "gcloud"),
		"AZURE_CONFIG_DIR":            filepath.Join(dir, "azure"),
		"GH_PROMPT_DISABLED":          "1",
		"GIT_TERMINAL_PROMPT":         "0",
		"GCM_INTERACTIVE":             "never",
	}
	if runtime.GOOS == "windows" {
		// Windows' ssh falls back to its agent's own pipe when the variable
		// is unset; a pipe nobody listens on keeps it from the operator's keys.
		set["SSH_AUTH_SOCK"] = `\\.\pipe\hrn-no-agent`
	}
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
