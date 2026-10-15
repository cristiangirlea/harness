package agent

import (
	"slices"
	"strings"

	"github.com/cristiangirlea/harness/internal/tools"
)

// An agent CLI starts from an allowlisted environment rather than the
// operator's whole one: an unattended run gets what a shell, the CLI's own
// login and the usual toolchains need, and nothing the operator set for
// something else (#411). Names that say they hold a secret stay out even when
// they match, except the CLI's own plan login; the guard (tools.GuardEnv)
// still applies on top. Options.InheritEnv turns this off.

// childEnvNames are the variables passed on every system.
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
	"JAVA_HOME", "CARGO_HOME", "RUSTUP_HOME", "NVM_DIR", "PNPM_HOME", "BUN_INSTALL", "VIRTUAL_ENV", "PYENV_ROOT",
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

// childEnvPrefixes pass a whole family: the locale's, Git Credential
// Manager's settings (where its store is, which a private fetch needs) and
// hrn's own, so an hrn MCP server the CLI starts from --mcp-config finds its
// home.
var childEnvPrefixes = []string{"LC_", "GCM_", "HRN_"}

// sanitizedEnv returns the allowlisted part of env for an agent CLI on goos,
// and how many variables it left out. keep names the CLI's plan login, which
// passes although its name says secret.
func sanitizedEnv(env []string, goos string, keep ...string) ([]string, int) {
	out := make([]string, 0, len(childEnvNames))
	dropped := 0
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		key := strings.ToUpper(k) // Windows names are case-insensitive
		if slices.Contains(keep, key) || !tools.SecretEnv(key) && childEnvAllowed(key, goos) {
			out = append(out, kv)
		} else {
			dropped++
		}
	}
	return out, dropped
}

func childEnvAllowed(key, goos string) bool {
	if slices.Contains(childEnvNames, key) || goos == "windows" && slices.Contains(childEnvWindows, key) {
		return true
	}
	return slices.ContainsFunc(childEnvPrefixes, func(p string) bool { return strings.HasPrefix(key, p) })
}
