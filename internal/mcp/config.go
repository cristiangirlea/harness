package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cristiangirlea/harness/internal/retrieval"
)

// Config uses Claude's mcpServers format with ${VAR} and ${VAR:-default} expansion.
// The stdio client skips URL and other non-stdio transports.
type Config struct {
	Servers map[string]configEntry `json:"mcpServers"`
}

type configEntry struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// DefaultConfigPath is $HRN_HOME/mcp.json, or ~/.hrn/mcp.json.
func DefaultConfigPath() string {
	if h := os.Getenv("HRN_HOME"); h != "" {
		return filepath.Join(h, "mcp.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hrn", "mcp.json")
}

// ConfigPaths lists existing default and extra config paths for task.json.
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

// LoadConfig sorts server names to stabilize tool lists and prompt caches.
func LoadConfig(path string) (servers []ServerConfig, skipped []string, err error) {
	return loadConfig(path, os.LookupEnv)
}

func loadConfig(path string, lookup func(string) (string, bool)) (servers []ServerConfig, skipped []string, err error) {
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
		s := ServerConfig{Name: name, Command: expandWith(e.Command, lookup), Env: map[string]string{}}
		for _, a := range e.Args {
			s.Args = append(s.Args, expandWith(a, lookup))
		}
		for k, v := range e.Env {
			s.Env[k] = expandWith(v, lookup)
		}
		servers = append(servers, s)
	}
	return servers, skipped, nil
}

// LoadConfigs merges files in order and servers by name, with later names overriding.
func LoadConfigs(paths []string) (servers []ServerConfig, skipped []string, err error) {
	return LoadConfigsWithEnv(paths, os.LookupEnv)
}

// LoadConfigsWithEnv expands server fields with lookup.
func LoadConfigsWithEnv(paths []string, lookup func(string) (string, bool)) (servers []ServerConfig, skipped []string, err error) {
	index := map[string]int{}
	for _, p := range paths {
		list, skip, err := loadConfig(p, lookup)
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
type Remote struct {
	Name, URL, Type string
	Headers         map[string]string
}

// LoadRemotes lists URL servers by name per file, with later names overriding.
func LoadRemotes(paths []string) ([]Remote, error) {
	return LoadRemotesWithEnv(paths, os.LookupEnv)
}

// LoadRemotesWithEnv expands remote URLs with lookup.
func LoadRemotesWithEnv(paths []string, lookup func(string) (string, bool)) ([]Remote, error) {
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
			e := cfg.Servers[name]
			r := Remote{Name: name, URL: expandWith(e.URL, lookup), Type: e.Type, Headers: e.Headers}
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

// Hide hrn secrets during expansion so configs cannot expose them under other names.
func expand(s string) string {
	return expandWith(s, os.LookupEnv)
}

func expandWith(s string, lookup func(string) (string, bool)) string {
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
		if v, ok := lookup(name); ok && v != "" && len(retrieval.ExecutorEnv([]string{name + "="})) == 1 {
			b.WriteString(v)
		} else if hasDef {
			b.WriteString(def)
		}
		s = s[i+j+1:]
	}
}

var errNoServers = errors.New("no stdio servers in config")
