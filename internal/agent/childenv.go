package agent

import (
	"slices"
	"strings"

	"github.com/cristiangirlea/harness/internal/tools"
)

// childEnvNames are what a shell, a login and the toolchains need on every system; see docs/backends.md.
var childEnvNames = []string{
	// The shell, the user and the terminal.
	"HOME", "USER", "USERNAME", "LOGNAME", "PATH", "SHELL", "TERM", "COLORTERM", "LANG", "TZ",
	"TMP", "TEMP", "TMPDIR",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR",
	// Proxies and the certificates a proxy needs.
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
	// Where each CLI keeps its login and settings.
	"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GEMINI_CLI_HOME", "OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR",
	// Where the toolchains an agent builds and tests with are.
	"GOPATH", "GOROOT", "GOCACHE", "GOMODCACHE", "GOTOOLCHAIN", "GOPROXY", "GOPRIVATE", "GONOSUMDB", "GOFLAGS",
	"JAVA_HOME", "CARGO_INCREMENTAL", "CARGO_HOME", "RUSTUP_HOME", "NVM_DIR", "PNPM_HOME", "BUN_INSTALL", "VIRTUAL_ENV", "PYENV_ROOT",
	"ANDROID_HOME", "ANDROID_SDK_ROOT",
}

// childEnvWindows are the variables nothing starts without on Windows, the
// places programs look for their own files, and claude's Git Bash.
var childEnvWindows = []string{
	"SYSTEMROOT", "WINDIR", "SYSTEMDRIVE", "COMSPEC", "PATHEXT", "APPDATA", "LOCALAPPDATA", "USERPROFILE",
	"HOMEDRIVE", "HOMEPATH", "PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMW6432", "PROGRAMDATA", "ALLUSERSPROFILE",
	"COMMONPROGRAMFILES", "COMMONPROGRAMFILES(X86)", "COMMONPROGRAMW6432", "PUBLIC", "OS", "COMPUTERNAME",
	"USERDOMAIN", "PROCESSOR_ARCHITECTURE", "NUMBER_OF_PROCESSORS", "PSMODULEPATH", "CLAUDE_CODE_GIT_BASH_PATH",
}

// childEnvPrefixes pass a whole family: the locale's, Git Credential Manager's settings and hrn's own.
var childEnvPrefixes = []string{"LC_", "GCM_", "HRN_"}

// sanitizedEnv returns the allowlisted part of env for an agent CLI, how many variables it dropped and the secret-looking set ones.
// keep names the CLI's own variables, which pass although unlisted or secret-looking.
func sanitizedEnv(env []string, goos string, keep ...string) (out []string, dropped int, secrets []string) {
	out = make([]string, 0, len(childEnvNames))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		key := strings.ToUpper(k) // Windows names are case-insensitive
		secret := tools.SecretEnv(key) || gcmSecret(key)
		if slices.Contains(keep, key) || !secret && childEnvAllowed(key, goos) {
			out = append(out, k+"="+withoutUserinfo(key, v))
			continue
		}
		dropped++
		if secret && v != "" && !slices.Contains(secrets, key) {
			secrets = append(secrets, key)
		}
	}
	slices.Sort(secrets)
	return out, dropped, secrets
}

func childEnvAllowed(key, goos string) bool {
	if slices.Contains(childEnvNames, key) || goos == "windows" && slices.Contains(childEnvWindows, key) {
		return true
	}
	// hrn's own addresses and files stay out: a database or server address
	// can carry a password, and a key file is a key (HRN_TLS_KEY_FILE).
	if strings.HasPrefix(key, "HRN_") && (strings.Contains(key, "_URL") || strings.HasSuffix(key, "KEY_FILE")) {
		return false
	}
	return slices.ContainsFunc(childEnvPrefixes, func(p string) bool { return strings.HasPrefix(key, p) })
}

// gcmSecret undoes the guard's GCM_ exemption for the allowlist, where the whole family passes;
// CREDENTIAL is ignored because GCM's plain settings use it (GCM_CREDENTIAL_STORE).
func gcmSecret(key string) bool {
	rest, ok := strings.CutPrefix(key, "GCM_")
	return ok && tools.SecretEnv("X_"+strings.ReplaceAll(rest, "CREDENTIAL", ""))
}

// proxyEnv are the allowed variables whose values are URLs that may carry a user and password.
var proxyEnv = []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "GCM_HTTP_PROXY", "GOPROXY"}

func withoutUserinfo(key, v string) string {
	if !slices.Contains(proxyEnv, key) {
		return v
	}
	if key != "GOPROXY" {
		// A password may hold '/', so the user part ends at the last '@'.
		if at := strings.LastIndexByte(v, '@'); at >= 0 {
			return v[:schemeEnd(v)] + v[at+1:]
		}
		return v
	}
	// GOPROXY is a list split by ',' and '|'; the separators are kept.
	var b strings.Builder
	for v != "" {
		i := strings.IndexAny(v, ",|")
		if i < 0 {
			i = len(v)
		}
		b.WriteString(stripUserinfo(v[:i]))
		if i < len(v) {
			b.WriteByte(v[i])
			i++
		}
		v = v[i:]
	}
	return b.String()
}

func schemeEnd(u string) int {
	if i := strings.Index(u, "://"); i >= 0 {
		return i + 3
	}
	return 0
}

func stripUserinfo(u string) string {
	start := schemeEnd(u)
	end := len(u)
	if i := strings.IndexAny(u[start:], "/?#"); i >= 0 {
		end = start + i
	}
	if at := strings.LastIndexByte(u[start:end], '@'); at >= 0 {
		return u[:start] + u[start+at+1:]
	}
	return u
}
