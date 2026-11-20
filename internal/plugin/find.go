// Package plugin discovers and runs hrn plugins over the v1 protocol.
package plugin

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/cristiangirlea/harness/internal/pluginproto"
	"github.com/cristiangirlea/harness/internal/runpaths"
)

// ErrNotFound means no executable was found for a plugin name.
var ErrNotFound = errors.New("plugin not found")

type Found struct {
	Name      string
	Path      string // Absolute executable path.
	Installed bool   // Found under $HRN_HOME/plugins.
}

// Find looks in $HRN_HOME/plugins/<name>/ first, then on PATH.
func Find(name string) (Found, error) {
	if !pluginproto.ValidName(name) {
		return Found{}, fmt.Errorf("plugin: invalid name %q", name)
	}
	if isReservedName(name) {
		return Found{}, fmt.Errorf("plugin: %q is reserved for one of hrn's own programs", name)
	}
	root := filepath.Join(runpaths.Home(), "plugins")
	if path := installedPath(root, name); isExecutable(path) {
		return Found{Name: name, Path: path, Installed: true}, nil
	}
	path, err := exec.LookPath(executableName(name))
	if err != nil || !filepath.IsAbs(path) || (runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(path), ".exe")) {
		return Found{}, fmt.Errorf("%w: %s (install it with: %s)", ErrNotFound, name, InstallHint(name))
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return Found{}, fmt.Errorf("plugin %s: resolve executable path: %w", name, err)
	}
	return Found{Name: name, Path: path}, nil
}

// List returns plugins sorted by name, with installed plugins shadowing those on PATH.
func List() ([]Found, error) {
	root := filepath.Join(runpaths.Home(), "plugins")
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("plugin discovery: read installed plugins: %w", err)
	}
	found := make(map[string]Found)
	for _, entry := range entries {
		name := entry.Name()
		if !pluginproto.ValidName(name) || isReservedName(name) {
			continue
		}
		if path := installedPath(root, name); isExecutable(path) {
			found[name] = Found{Name: name, Path: path, Installed: true}
		}
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := pathPluginName(entry.Name())
			if !pluginproto.ValidName(name) || isReservedName(name) || found[name].Name != "" {
				continue
			}
			if path := filepath.Join(dir, entry.Name()); isExecutable(path) {
				found[name] = Found{Name: name, Path: path}
			}
		}
	}
	return slices.SortedFunc(maps.Values(found), func(a, b Found) int {
		return strings.Compare(a.Name, b.Name)
	}), nil
}

// InstallHint is the command that installs a plugin.
func InstallHint(name string) string { return "hrn plugin install " + name }

func installedPath(root, name string) string {
	return filepath.Join(root, name, executableName(name))
}

func executableName(name string) string {
	base := "hrn-" + name
	if runtime.GOOS == "windows" {
		base += ".exe"
	}
	return base
}

func pathPluginName(base string) string {
	name, ok := strings.CutPrefix(base, "hrn-")
	if !ok {
		return ""
	}
	if runtime.GOOS == "windows" {
		ext := filepath.Ext(name)
		if !strings.EqualFold(ext, ".exe") {
			return ""
		}
		name = strings.TrimSuffix(name, ext)
	}
	return name
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode()&0o111 != 0)
}

func isReservedName(name string) bool {
	switch name {
	case "server", "worker", "fixture-model":
		return true
	default:
		return false
	}
}
