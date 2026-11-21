package prefs

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
)

type PluginPrefs struct {
	Install string            `json:"install,omitempty"`
	Sources map[string]string `json:"sources,omitempty"`
	Allow   []string          `json:"allow,omitempty"`
}

var (
	pluginName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	repoOwner  = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}$`)
	repoName   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

func ValidPluginName(name string) bool {
	switch name {
	case "server", "worker", "fixture-model":
		return false
	}
	return pluginName.MatchString(name)
}

func ValidPluginSource(source string) bool {
	owner, repo, ok := strings.Cut(source, "/")
	return ok && repoOwner.MatchString(owner) && repoName.MatchString(repo) && repo != "." && repo != ".."
}

// UnmarshalJSON rejects misspelled policy fields even in a personal preferences file.
func (p *PluginPrefs) UnmarshalJSON(data []byte) error {
	type fields PluginPrefs
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*fields)(p))
}

func (p *PluginPrefs) validate() error {
	if p == nil {
		return nil
	}
	switch p.Install {
	case "", "on", "off":
	default:
		return errors.New("plugins.install: want on or off")
	}
	for name, source := range p.Sources {
		if !ValidPluginName(name) {
			return errors.New("plugins.sources: invalid or reserved plugin name")
		}
		if !ValidPluginSource(source) {
			return errors.New("plugins.sources: want owner/repo")
		}
	}
	for _, entry := range p.Allow {
		owner, repo, _ := strings.Cut(entry, "/")
		if !ValidPluginSource(entry) && !(repo == "*" && repoOwner.MatchString(owner)) {
			return errors.New("plugins.allow: want owner/repo or owner/*")
		}
	}
	return nil
}

func equalPluginPrefs(a, b *PluginPrefs) bool {
	return a == b || a != nil && b != nil &&
		a.Install == b.Install && maps.Equal(a.Sources, b.Sources) && slices.Equal(a.Allow, b.Allow)
}

func (p Prefs) PluginSource(name string) (string, bool) {
	if p.Plugins == nil {
		return "", false
	}
	source, ok := p.Plugins.Sources[name]
	return source, ok
}

func (p Prefs) PluginInstallOff() bool {
	return p.Plugins != nil && p.Plugins.Install == "off"
}

func (p Prefs) PluginAllowed(source string) bool {
	if p.Plugins == nil || len(p.Plugins.Allow) == 0 {
		return true
	}
	owner, _, _ := strings.Cut(source, "/")
	for _, entry := range p.Plugins.Allow {
		if strings.EqualFold(entry, source) || strings.EqualFold(entry, owner+"/*") {
			return true
		}
	}
	return false
}
