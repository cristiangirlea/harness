package agent

import (
	"fmt"
	"strconv"
	"strings"
)

// The environment an agent CLI runs in. hrn's own command guard checks the api
// backend's run_command; a CLI runs its tools itself, so the guard for every
// backend lives in what the CLI inherits: the credentials that would let a
// task publish or deploy are left out, git rewrites every push address to one
// it cannot reach, and gh starts with no login. Like the command guard it
// stops the plausible accident, not an agent set on getting around it.
// --allow-dangerous (Options.AllowDangerous) lifts it with the command guard.

// publishingEnv names the variables left out: tokens for publishing code,
// packages and releases, cloud and deploy credentials, and the ssh agent
// (an ssh push or deploy would otherwise use the operator's keys).
var publishingEnv = []string{
	"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
	"CLOUDFLARE_API_TOKEN", "CLOUDFLARE_API_KEY", "CF_API_TOKEN", "CF_API_KEY",
	"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	"AZURE_CLIENT_SECRET", "GOOGLE_APPLICATION_CREDENTIALS",
	"DIGITALOCEAN_ACCESS_TOKEN", "DIGITALOCEAN_TOKEN", "FLY_API_TOKEN", "HEROKU_API_KEY",
	"VERCEL_TOKEN", "NETLIFY_AUTH_TOKEN",
	"NPM_TOKEN", "NODE_AUTH_TOKEN", "CARGO_REGISTRY_TOKEN", "TWINE_PASSWORD", "DOCKER_PASSWORD",
	"PADDLE_API_KEY", "RESEND_API_KEY", "STRIPE_SECRET_KEY",
	"SSH_AUTH_SOCK",
}

// refusedPushScheme is where every push goes instead. git has no helper for
// it, so a push fails at once, and its error names who refused it.
const refusedPushScheme = "push-refused-by-hrn://"

// guardEnv returns env for an agent CLI under the guard. ghDir is an empty
// directory for gh's configuration, so gh has no login there.
func guardEnv(env []string, ghDir string) []string {
	drop := map[string]bool{"GH_CONFIG_DIR": true, "GH_PROMPT_DISABLED": true, "GIT_TERMINAL_PROMPT": true, "GIT_CONFIG_COUNT": true}
	for _, k := range publishingEnv {
		drop[k] = true
	}
	count := 0
	out := make([]string, 0, len(env)+16)
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		key := strings.ToUpper(k) // Windows names are case-insensitive
		if key == "GIT_CONFIG_COUNT" {
			count, _ = strconv.Atoi(v)
		}
		if !drop[key] {
			out = append(out, kv)
		}
	}
	// git reads GIT_CONFIG_KEY_n/VALUE_n for n below GIT_CONFIG_COUNT; the
	// operator's own entries stay and the guard's follow them.
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://", "git@"} {
		out = append(out,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=url.%s.pushinsteadof", count, refusedPushScheme),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", count, prefix))
		count++
	}
	return append(out,
		fmt.Sprintf("GIT_CONFIG_COUNT=%d", count),
		"GIT_TERMINAL_PROMPT=0",
		"GH_CONFIG_DIR="+ghDir,
		"GH_PROMPT_DISABLED=1",
	)
}
