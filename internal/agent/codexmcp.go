package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/tools"
)

// Codex merges empty MCP tables; lean must disable names individually.
func codexMCPArgs(t Task) []string {
	// A config that does not load is reported by codexMCPEnv before the start.
	servers, remotes, _, _ := codexMCPConfigs(t)
	ours := map[string]bool{}
	for _, s := range servers {
		ours[s.Name] = true
	}
	for _, r := range remotes {
		ours[r.Name] = true
	}
	var a []string
	set := func(name, field, value string) {
		a = append(a, "-c", "mcp_servers."+name+"."+field+"="+value)
	}
	if t.IsLean() {
		for _, name := range codexAllConfigServers(t.Dir) {
			if !ours[name] && isBareKey(name) {
				set(name, "enabled", "false")
			}
		}
	}
	for _, s := range servers {
		set(s.Name, "enabled", "true")
		set(s.Name, "command", tomlString(s.Command))
		set(s.Name, "args", tomlArray(s.Args))
		if len(s.Env) > 0 {
			set(s.Name, "env_vars", tomlArray(sortedKeys(s.Env)))
		}
	}
	for _, r := range remotes {
		if !slices.ContainsFunc(servers, func(s mcp.ServerConfig) bool { return s.Name == r.Name }) {
			set(r.Name, "enabled", "true")
			set(r.Name, "url", tomlString(r.URL))
			if token, ok := codexBearerToken(r.Headers); ok && token != "" {
				set(r.Name, "bearer_token_env_var", tomlString(token))
			}
		}
	}
	return a
}

// env_vars keeps MCP secrets off command lines.
func codexMCPEnv(t Task) ([]string, error) {
	servers, remotes, _, err := codexMCPConfigs(t)
	if err != nil {
		return nil, err
	}
	for _, name := range codexAllConfigServers(t.Dir) {
		if slices.ContainsFunc(servers, func(s mcp.ServerConfig) bool { return s.Name == name }) || slices.ContainsFunc(remotes, func(r mcp.Remote) bool { return r.Name == name }) {
			return nil, fmt.Errorf("mcp server %q: run server collides with codex config server", name)
		}
	}
	for _, r := range remotes {
		if r.Type == "sse" {
			return nil, fmt.Errorf("mcp server %s: codex speaks streamable HTTP; sse transport would be dropped", r.Name)
		}
		if _, ok := codexBearerToken(r.Headers); !ok {
			return nil, fmt.Errorf("mcp server %s: unsupported headers would be dropped by codex", r.Name)
		}
	}
	type setBy struct{ value, server string }
	vars := map[string]setBy{}
	lookup := codexMCPLookup(t)
	for _, s := range servers {
		for _, k := range sortedKeys(s.Env) {
			if k == "" || strings.Contains(k, "=") {
				return nil, fmt.Errorf("mcp %s: %q is not an environment variable name", s.Name, k)
			}
			if strings.HasPrefix(strings.ToUpper(k), "GIT_CONFIG_") {
				return nil, fmt.Errorf("mcp %s: %s is not allowed in a server's env; it conflicts with the git guard", s.Name, k)
			}
			if k == "CODEX_HOME" || runtime.GOOS == "windows" && strings.EqualFold(k, "CODEX_HOME") {
				return nil, fmt.Errorf("mcp %s: %s is not allowed in a server's env; codex must use the home hrn reads", s.Name, k)
			}
			if len(agentEnv([]string{k + "="})) == 0 {
				return nil, fmt.Errorf("mcp %s: hrn keeps %s out of an agent's environment, and codex would get it too", s.Name, k)
			}
			if strings.EqualFold(k, "ANTHROPIC_API_KEY") {
				return nil, fmt.Errorf("mcp %s: ANTHROPIC_API_KEY never reaches a CLI, so codex could not pass it to this server", s.Name)
			}
			// The value lands in codex's own environment too, so it must not change one codex relies on.
			if have, ok := lookup(k); ok && have != s.Env[k] {
				return nil, fmt.Errorf("mcp %s: %s is already set in codex's environment to another value; codex starts its servers with its own environment", s.Name, k)
			}
			key := k
			if runtime.GOOS == "windows" {
				key = strings.ToUpper(k)
			}
			if prev, ok := vars[key]; ok && prev.value != s.Env[k] {
				return nil, fmt.Errorf("mcp servers %s and %s set %s differently; codex starts them with one environment", prev.server, s.Name, k)
			}
			vars[key] = setBy{s.Env[k], s.Name}
		}
	}
	env := make([]string, 0, len(vars))
	for _, k := range sortedKeys(vars) {
		env = append(env, k+"="+vars[k].value)
	}
	return env, nil
}

var codexBearer = regexp.MustCompile(`^Bearer \$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

func codexBearerToken(headers map[string]string) (string, bool) {
	if len(headers) == 0 {
		return "", true
	}
	if len(headers) != 1 {
		return "", false
	}
	for name, value := range headers {
		match := codexBearer.FindStringSubmatch(value)
		if strings.EqualFold(name, "Authorization") && len(match) == 2 {
			return match[1], true
		}
	}
	return "", false
}

func codexMCPLookup(t Task) func(string) (string, bool) {
	lookup := os.LookupEnv
	if !t.AllowDangerous {
		env := withoutEnv(agentEnv(os.Environ()), "ANTHROPIC_API_KEY")
		if !t.InheritEnv {
			env, _, _ = sanitizedEnv(env, runtime.GOOS)
		}
		env = append(env, t.ChildEnv...)
		env, _ = tools.GuardEnv(env, "")
		values := map[string]string{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			if runtime.GOOS == "windows" {
				k = strings.ToUpper(k)
			}
			values[k] = v
		}
		guarded, _ := tools.GuardEnv(nil, "")
		for _, kv := range guarded {
			k, _, _ := strings.Cut(kv, "=")
			if runtime.GOOS == "windows" {
				k = strings.ToUpper(k)
			}
			delete(values, k)
		}
		// The guard numbers its git config entries after the user's, so none of them are stable to expand.
		for k := range values {
			if strings.HasPrefix(strings.ToUpper(k), "GIT_CONFIG_") {
				delete(values, k)
			}
		}
		lookup = func(k string) (string, bool) {
			if runtime.GOOS == "windows" {
				k = strings.ToUpper(k)
			}
			v, ok := values[k]
			return v, ok
		}
	}
	return lookup
}

func codexMCPConfigs(t Task) ([]mcp.ServerConfig, []mcp.Remote, []string, error) {
	lookup := codexMCPLookup(t)
	servers, _, err := mcp.LoadConfigsWithEnv(t.MCP, lookup)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mcp config: %w", err)
	}
	remotes, err := mcp.LoadRemotesWithEnv(t.MCP, lookup)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mcp config: %w", err)
	}
	home := len(t.MCP) > 0 && filepath.Clean(t.MCP[0]) == filepath.Clean(mcp.DefaultConfigPath())
	explicit := t.MCP
	if home {
		explicit = explicit[1:]
	}
	expServers, _, err := mcp.LoadConfigsWithEnv(explicit, lookup)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mcp config: %w", err)
	}
	expRemotes, err := mcp.LoadRemotesWithEnv(explicit, lookup)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mcp config: %w", err)
	}
	for _, s := range expServers {
		if !isBareKey(s.Name) {
			return nil, nil, nil, fmt.Errorf("mcp server %q: codex takes only names of letters, digits, '_' and '-'", s.Name)
		}
	}
	for _, r := range expRemotes {
		if !isBareKey(r.Name) {
			return nil, nil, nil, fmt.Errorf("mcp server %q: codex takes only names of letters, digits, '_' and '-'", r.Name)
		}
	}
	var skipped []string
	servers = slices.DeleteFunc(servers, func(s mcp.ServerConfig) bool {
		if isBareKey(s.Name) {
			return false
		}
		skipped = append(skipped, s.Name)
		return true
	})
	remotes = slices.DeleteFunc(remotes, func(r mcp.Remote) bool {
		if isBareKey(r.Name) {
			return false
		}
		if !slices.Contains(skipped, r.Name) {
			skipped = append(skipped, r.Name)
		}
		return true
	})
	return servers, remotes, skipped, nil
}

func codexConfigPath(dir string) string {
	home := codexHomeDir(dir)
	if home == "" {
		return ""
	}
	return filepath.Join(home, "config.toml")
}

// codexHomeDir is the directory codex started in dir reads and writes; "" when unknown.
func codexHomeDir(dir string) string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		if !filepath.IsAbs(h) {
			h = filepath.Join(dir, h)
		}
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func codexAllConfigServers(dir string) []string {
	names := codexConfigServers(codexConfigPath(dir))
	for _, name := range codexConfigServers(filepath.Join(dir, ".codex", "config.toml")) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// Values can contain fake headers, so track multiline strings across lines.
func codexConfigServers(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var names, table []string
	multi := ""
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if multi == "" {
			if strings.HasPrefix(line, "[") && !strings.HasPrefix(line, "[[") {
				if keys, ok := tomlHeader(line[1:]); ok {
					table = keys
					if len(keys) >= 2 && keys[0] == "mcp_servers" && !slices.Contains(names, keys[1]) {
						names = append(names, keys[1])
					}
				}
			} else if i := strings.IndexByte(line, '='); i >= 0 {
				if keys, ok := tomlHeader(line[:i] + "]"); ok {
					full := append(slices.Clone(table), keys...)
					if len(full) >= 2 && full[0] == "mcp_servers" && !slices.Contains(names, full[1]) {
						names = append(names, full[1])
					}
				}
			}
		}
		for i := 0; i < len(raw); {
			if multi != "" {
				if strings.HasPrefix(raw[i:], multi) {
					i += 3
					multi = ""
				} else if multi == `"""` && raw[i] == '\\' && i+1 < len(raw) {
					i += 2
				} else {
					i++
				}
				continue
			}
			if raw[i] == '#' {
				break
			}
			if strings.HasPrefix(raw[i:], `"""`) || strings.HasPrefix(raw[i:], "'''") {
				multi = raw[i : i+3]
				i += 3
				continue
			}
			if raw[i] == '"' || raw[i] == '\'' {
				quote := raw[i]
				i++
				for i < len(raw) {
					if quote == '"' && raw[i] == '\\' && i+1 < len(raw) {
						i += 2
						continue
					}
					if raw[i] == quote {
						i++
						break
					}
					i++
				}
				continue
			}
			i++
		}
	}
	return names
}

func codexBatchArgsError(bin string, args []string) error {
	if !isBatchFile(bin) {
		return nil
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-c" && strings.ContainsAny(args[i+1], "\" &|<>^%\t\r\n") {
			return fmt.Errorf("codex -c value cannot safely pass through a cmd.exe shim; install codex as an exe")
		}
	}
	return nil
}

func tomlHeader(s string) ([]string, bool) {
	var keys []string
	for {
		s = strings.TrimLeft(s, " \t")
		var key string
		switch {
		case strings.HasPrefix(s, `"`):
			end := 1
			for end < len(s) && s[end] != '"' {
				if s[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(s) {
				return nil, false
			}
			k, err := strconv.Unquote(s[:end+1])
			if err != nil {
				return nil, false
			}
			key, s = k, s[end+1:]
		case strings.HasPrefix(s, "'"):
			end := strings.IndexByte(s[1:], '\'')
			if end < 0 {
				return nil, false
			}
			key, s = s[1:end+1], s[end+2:]
		default:
			n := len(bareKey.FindString(s))
			if n == 0 {
				return nil, false
			}
			key, s = s[:n], s[n:]
		}
		keys = append(keys, key)
		s = strings.TrimLeft(s, " \t")
		switch {
		case strings.HasPrefix(s, "."):
			s = s[1:]
		case strings.HasPrefix(s, "]"):
			rest := strings.TrimSpace(s[1:])
			return keys, rest == "" || strings.HasPrefix(rest, "#")
		default:
			return nil, false
		}
	}
}

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+`)

func isBareKey(name string) bool {
	return name != "" && len(bareKey.FindString(name)) == len(name)
}

// TOML string escapes differ from Go's.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func tomlArray(list []string) string {
	quoted := make([]string, len(list))
	for i, s := range list {
		quoted[i] = tomlString(s)
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
