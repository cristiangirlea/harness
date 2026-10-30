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
		for _, name := range codexConfigServers(codexConfigPath(t.Dir)) {
			if !ours[name] && isBareKey(name) {
				set(name, "enabled", "false")
			}
		}
	}
	// Overrides merge into a config.toml entry of the same name, which may be disabled.
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
	for _, r := range remotes {
		if r.Headers || r.Type == "sse" {
			return nil, fmt.Errorf("mcp server %s: codex is passed only a url, so its headers or sse transport would be dropped", r.Name)
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

// codexConfigServers reads table headers only: values may hold secrets and are never read.
func codexConfigServers(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[") || strings.HasPrefix(line, "[[") {
			continue
		}
		keys, ok := tomlHeader(line[1:])
		if ok && len(keys) == 2 && keys[0] == "mcp_servers" && !slices.Contains(names, keys[1]) {
			names = append(names, keys[1])
		}
	}
	return names
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
