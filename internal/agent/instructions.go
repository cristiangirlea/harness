package agent

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/tools"
)

// instructionFiles are the project instruction files, read from the workspace
// root in this order. The agent CLIs read their own (claude: CLAUDE.md, codex
// and opencode: AGENTS.md), so this is what gives the api loop the same view.
// A function, not a package variable, so the order cannot be changed at runtime.
func instructionFiles() []string { return []string{"HRN.md", "AGENTS.md", "CLAUDE.md"} }

const (
	maxInstructions = 32 * 1024
	truncatedMarker = "\n\n[truncated: project instructions exceed 32 KiB]"
)

// loadInstructions renders the instruction files found in the workspace root.
// The text is empty when there are none, and truncated reports whether the cap
// dropped anything. Only the root is searched: nested and parent directories
// are deliberately out of scope.
//
// Every path goes through the workspace, so a file that is a symlink out of the
// tree is refused rather than read into the prompt.
func loadInstructions(ws *tools.Workspace) (text string, names []string, truncated bool, err error) {
	var b strings.Builder
	for _, name := range instructionFiles() {
		path, err := ws.Resolve(name)
		if err != nil {
			return "", nil, false, fmt.Errorf("%s: %w", name, err)
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", nil, false, err
		}
		content := strings.TrimSpace(strings.ReplaceAll(string(data), "\r\n", "\n"))
		if content == "" {
			continue
		}
		heading := "\n## " + name + "\n\n"
		if b.Len() == 0 {
			heading = "# Project instructions\n" + heading
		}
		if room := maxInstructions - b.Len(); b.Len()+len(heading)+len(content)+1 > maxInstructions {
			truncated = true
			// A section that cannot fit its heading and some content is left
			// out whole; a half-written heading would read as corruption.
			if room > len(heading) {
				b.WriteString(heading)
				b.WriteString(trimToRune(content[:room-len(heading)]))
				names = append(names, name)
			}
			b.WriteString(truncatedMarker)
			break
		}
		b.WriteString(heading)
		b.WriteString(content)
		b.WriteString("\n")
		names = append(names, name)
	}
	return b.String(), names, truncated, nil
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
