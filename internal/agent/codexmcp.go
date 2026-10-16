package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/cristiangirlea/harness/internal/mcp"
)

// codexMCPArgs are the -c overrides that give a codex run the run's --mcp
// servers and, when lean, none of the servers in the user's config.toml.
// codex merges -c mcp_servers={} rather than replacing it, and refuses a
// plugin's server or a quoted name by name, so only config.toml's bare ones go.
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
		for _, name := range codexConfigServers(codexConfigPath()) {
			if !ours[name] && isBareKey(name) {
				set(name, "enabled", "false")
			}
		}
	}
	// Overrides merge into a config.toml entry of the same name, which may
	// be disabled or carry args of its own.
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

// codexMCPEnv is the environment the run's MCP servers ask for. codex passes
// variables named in env_vars on from its own environment, so the values
// stay off the command line, the process list and the run's start line.
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
	type setBy struct{ value, server string }
	vars := map[string]setBy{}
	for _, s := range servers {
		for _, k := range sortedKeys(s.Env) {
			if k == "" || strings.Contains(k, "=") {
				return nil, fmt.Errorf("mcp %s: %q is not an environment variable name", s.Name, k)
			}
			if prev, ok := vars[k]; ok && prev.value != s.Env[k] {
				return nil, fmt.Errorf("mcp servers %s and %s set %s differently; codex starts them with one environment", prev.server, s.Name, k)
			}
			vars[k] = setBy{s.Env[k], s.Name}
		}
	}
	env := make([]string, 0, len(vars))
	for _, k := range sortedKeys(vars) {
		env = append(env, k+"="+vars[k].value)
	}
	return env, nil
}

// codexConfigPath is where codex reads its user config.
func codexConfigPath() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return filepath.Join(h, "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "config.toml")
}

// codexConfigServers names the [mcp_servers.<name>] tables of a config.toml,
// from table headers only: no value is read. A file that cannot be read has none.
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

// tomlHeader reads the dotted key of a table header up to its "]".
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

// tomlString quotes s as a TOML basic string, whose escapes differ from Go's.
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
