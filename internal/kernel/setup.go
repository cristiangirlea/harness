package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
)

// Setup fingerprints loaded configuration without storing contents.
type Setup struct {
	// Backend identifies which configuration the adapter loads.
	Backend string `json:"backend,omitempty"`
	// Digest includes options that can distinguish otherwise identical runs.
	Digest string `json:"digest"`
	// BaseDigest excludes options to group equivalent setups.
	BaseDigest string `json:"base_digest"`
	// Files maps paths to hashes or unreadable/outside markers.
	Files   map[string]string `json:"files,omitempty"`
	MCP     []string          `json:"mcp,omitempty"`
	Plugins []string          `json:"plugins,omitempty"`
	Skills  []string          `json:"skills,omitempty"`
	// Truncated warns that setup identity is incomplete.
	Truncated bool `json:"truncated,omitempty"`
}

// WithOptions adds digest inputs for prompt-only variants.
func (s *Setup) WithOptions(lines ...string) *Setup {
	if s == nil || len(lines) == 0 {
		return s
	}
	c := *s
	c.Digest = digestLines(append([]string{s.Digest}, lines...))
	return &c
}

func digestLines(lines []string) string {
	h := sha256.New()
	for _, l := range lines {
		_, _ = io.WriteString(h, l+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}
