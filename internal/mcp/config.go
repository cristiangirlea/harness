package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Config is the on-disk format, shared with Claude Code's .mcp.json and the
// Claude desktop app so existing files work unchanged:
//
//	{"mcpServers": {"name": {"command": "npx", "args": ["-y", "pkg"], "env": {"K": "${VAR}"}}}}
//
// ${VAR} and ${VAR:-default} expand from the environment. Entries with a
// "url" (http or sse transports) are skipped: hrn speaks stdio only.
type Config struct {
	Servers map[string]configEntry `json:"mcpServers"`
}

type configEntry struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
}

// DefaultConfigPath is $HRN_HOME/mcp.json, or ~/.hrn/mcp.json.
func DefaultConfigPath() string {
	if h := os.Getenv("HRN_HOME"); h != "" {
		return filepath.Join(h, "mcp.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hrn", "mcp.json")
}

// ConfigPaths returns the default config when it exists, followed by extra,
// all absolute. This is what gets recorded in a run's task.json.
func ConfigPaths(extra []string) ([]string, error) {
	var out []string
	if p := DefaultConfigPath(); p != "" {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	for _, p := range extra {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(abs); err != nil {
			return nil, fmt.Errorf("mcp config %s: %w", p, err)
		}
		out = append(out, abs)
	}
	return out, nil
}

// LoadConfig reads one file. Servers come back sorted by name so the tool
// list, and with it the prompt cache, is stable across runs.
func LoadConfig(path string) (servers []ServerConfig, skipped []string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	names := make([]string, 0, len(cfg.Servers))
	for n := range cfg.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		e := cfg.Servers[name]
		if e.URL != "" || (e.Type != "" && e.Type != "stdio") || e.Command == "" {
			skipped = append(skipped, name)
			continue
		}
		s := ServerConfig{Name: name, Command: expand(e.Command), Env: map[string]string{}}
		for _, a := range e.Args {
			s.Args = append(s.Args, expand(a))
		}
		for k, v := range e.Env {
			s.Env[k] = expand(v)
		}
		servers = append(servers, s)
	}
	return servers, skipped, nil
}

// LoadConfigs merges several files; a later file overrides an earlier
// server of the same name. Order is by name within each file, files in the
// order given.
func LoadConfigs(paths []string) (servers []ServerConfig, skipped []string, err error) {
	index := map[string]int{}
	for _, p := range paths {
		list, skip, err := LoadConfig(p)
		if err != nil {
			return nil, nil, err
		}
		skipped = append(skipped, skip...)
		for _, s := range list {
			if i, ok := index[s.Name]; ok {
				servers[i] = s
				continue
			}
			index[s.Name] = len(servers)
			servers = append(servers, s)
		}
	}
	return servers, skipped, nil
}

// Remote is a server reached by URL, which hrn's own client skips.
type Remote struct{ Name, URL string }

// LoadRemotes returns the url servers of paths, by name within each file; a
// later file overrides an earlier server of the same name.
func LoadRemotes(paths []string) ([]Remote, error) {
	index := map[string]int{}
	var out []Remote
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var cfg Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		names := make([]string, 0, len(cfg.Servers))
		for n := range cfg.Servers {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			if cfg.Servers[name].URL == "" {
				continue
			}
			r := Remote{Name: name, URL: expand(cfg.Servers[name].URL)}
			if i, ok := index[name]; ok {
				out[i] = r
				continue
			}
			index[name] = len(out)
			out = append(out, r)
		}
	}
	return out, nil
}

// expand substitutes ${VAR} and ${VAR:-default}; anything else is literal.
func expand(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		name, def, hasDef := strings.Cut(s[i+2:i+j], ":-")
		if v, ok := os.LookupEnv(name); ok && v != "" {
			b.WriteString(v)
		} else if hasDef {
			b.WriteString(def)
		}
		s = s[i+j+1:]
	}
}

var errNoServers = errors.New("no stdio servers in config")
