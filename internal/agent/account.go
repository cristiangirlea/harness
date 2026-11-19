package agent

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

func accountHomeEnv(backend string) string {
	if backend == "claude" {
		return "CLAUDE_CONFIG_DIR"
	}
	return "CODEX_HOME"
}

func accountHomeRedactor(home string) *strings.Replacer {
	const hidden = "[account home]"
	clean := filepath.Clean(home)
	paths := []string{home, filepath.ToSlash(home), clean, filepath.ToSlash(clean)}
	if real, err := filepath.EvalSymlinks(clean); err == nil {
		paths = append(paths, real, filepath.ToSlash(real))
	}
	var pairs []string
	for _, path := range paths {
		escaped, _ := json.Marshal(path)
		pairs = append(pairs, path, hidden, string(escaped[1:len(escaped)-1]), hidden)
	}
	return strings.NewReplacer(pairs...)
}
