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

// Variables withheld besides secretEnv's patterns: unnamed credentials, the ssh agent, askpass helpers.
var withheldEnv = []string{
	"AWS_ACCESS_KEY_ID", "GOOGLE_APPLICATION_CREDENTIALS", "AZURE_CLIENT_ID", "AZURE_TENANT_ID",
	"TWINE_USERNAME", "DOCKER_USERNAME", "VAULT_ADDR", "BW_SESSION",
	"SSH_AUTH_SOCK", "GIT_ASKPASS", "SSH_ASKPASS",
}

func secretEnv(name string) bool {
	n := strings.ToUpper(name)
	if slices.Contains(withheldEnv, n) {
		return true
	}
	// Private fetches need these non-secret GCM settings.
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
	for _, prefix := range []string{"OP_SESSION_", "VSCODE_GIT_"} {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

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
		// Unset exposes keys via Windows ssh's default agent.
		r["SSH_AUTH_SOCK"] = `\\.\pipe\hrn-no-agent`
	}
	return r
}

// No helper serves this scheme, so pushes fail immediately.
const refusedPushScheme = "push-refused-by-hrn://"

// GuardedVar bars nested guard overrides.
const GuardedVar = "HRN_GUARDED"

// Guarded reports inherited guarding.
func Guarded() bool { return os.Getenv(GuardedVar) != "" }

// Invalid tokens stop store fallback; rules block auth token --user.
const ghNoToken = "hrn-guard-no-token"

func fixedEnv() map[string]string {
	return map[string]string{
		GuardedVar:            "1",
		"GH_TOKEN":            ghNoToken,
		"GH_ENTERPRISE_TOKEN": ghNoToken,
	}
}

// GuardEnv strips secrets and points logins at an empty dir, returning the withheld names; keep permits CLI login variables.
// CLIs run their own tools, so the guard lives in their environment (docs/permissions.md).
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
	// Append to operator config; empty prefix catches all addresses.
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
