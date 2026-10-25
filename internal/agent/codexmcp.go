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
)

// Codex merges empty MCP tables; lean must disable names individually.
func codexMCPArgs(t Task) []string {
	// A config that does not load is reported by codexMCPEnv before the start.
	servers, _, _ := mcp.LoadConfigs(t.MCP)
	remotes, _ := mcp.LoadRemotes(t.MCP)
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
	servers, _, err := mcp.LoadConfigs(t.MCP)
	if err != nil {
		return nil, fmt.Errorf("mcp config: %w", err)
	}
	remotes, err := mcp.LoadRemotes(t.MCP)
	if err != nil {
		return nil, fmt.Errorf("mcp config: %w", err)
	}
	names := make([]string, 0, len(servers)+len(remotes))
	for _, s := range servers {
		names = append(names, s.Name)
	}
	for _, r := range remotes {
		names = append(names, r.Name)
	}
	for _, name := range names {
		if !isBareKey(name) {
			return nil, fmt.Errorf("mcp server %q: codex takes only names of letters, digits, '_' and '-'", name)
		}
	}
	for _, r := range remotes {
		if r.Headers || r.Type == "sse" {
			return nil, fmt.Errorf("mcp server %s: codex is passed only a url, so its headers or sse transport would be dropped", r.Name)
		}
	}
	type setBy struct{ value, server string }
	vars := map[string]setBy{}
	for _, s := range servers {
		for _, k := range sortedKeys(s.Env) {
			if k == "" || strings.Contains(k, "=") {
				return nil, fmt.Errorf("mcp %s: %q is not an environment variable name", s.Name, k)
			}
			if k == "CODEX_HOME" || runtime.GOOS == "windows" && strings.EqualFold(k, "CODEX_HOME") {
				return nil, fmt.Errorf("mcp %s: %s is not allowed in a server's env; codex must use the home hrn reads", s.Name, k)
			}
			if len(agentEnv([]string{k + "="})) == 0 {
				return nil, fmt.Errorf("mcp %s: hrn keeps %s out of an agent's environment, and codex would get it too", s.Name, k)
			}
			// The value lands in codex's own environment too, so it must not change one codex relies on.
			if have, ok := os.LookupEnv(k); ok && have != s.Env[k] {
				return nil, fmt.Errorf("mcp %s: %s is already set in hrn's environment to another value; codex starts its servers with its own environment", s.Name, k)
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
