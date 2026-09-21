package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// instructionFiles are the project instruction files, read from the workspace
// root in this order. The agent CLIs read their own (claude: CLAUDE.md, codex
// and opencode: AGENTS.md), so this is what gives the api loop the same view.
var instructionFiles = []string{"HX.md", "AGENTS.md", "CLAUDE.md"}

const (
	maxInstructions = 32 * 1024
	truncatedMarker = "\n\n[truncated: project instructions exceed 32 KiB]"
)

// loadInstructions renders the instruction files found in the workspace root.
// The text is empty when there are none. Only the root is searched: nested and
// parent directories are deliberately out of scope.
func loadInstructions(root string) (text string, names []string, err error) {
	var b strings.Builder
	for _, name := range instructionFiles {
		data, err := os.ReadFile(filepath.Join(root, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		content := strings.TrimSpace(strings.ReplaceAll(string(data), "\r\n", "\n"))
		if content == "" {
			continue
		}
		section := "\n## " + name + "\n\n" + content + "\n"
		if b.Len() == 0 {
			section = "# Project instructions\n" + section
		}
		names = append(names, name)
		if room := maxInstructions - b.Len(); b.Len()+len(section) > maxInstructions {
			b.WriteString(trimToRune(section[:room]))
			b.WriteString(truncatedMarker)
			break
		}
		b.WriteString(section)
	}
	return b.String(), names, nil
}

// trimToRune drops a trailing partial rune, which a byte-wise cut can leave.
func trimToRune(s string) string {
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}
