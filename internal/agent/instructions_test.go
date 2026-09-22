package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/tools"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, dir string) (string, []string, bool, error) {
	t.Helper()
	ws, err := tools.NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	return loadInstructions(ws)
}

func TestLoadInstructions(t *testing.T) {
	dir := t.TempDir()
	if text, names, truncated, err := load(t, dir); err != nil || text != "" || names != nil || truncated {
		t.Fatalf("empty workspace: text=%q names=%v truncated=%v err=%v", text, names, truncated, err)
	}

	// Files are read in a fixed order, whatever order they were created in.
	writeFile(t, dir, "CLAUDE.md", "Run gofmt.\r\n")
	writeFile(t, dir, "AGENTS.md", "  Use table tests.  \n")
	writeFile(t, dir, "README.md", "not an instruction file")
	text, names, truncated, err := load(t, dir)
	if err != nil || truncated {
		t.Fatalf("truncated=%v err=%v", truncated, err)
	}
	want := "# Project instructions\n\n## AGENTS.md\n\nUse table tests.\n\n## CLAUDE.md\n\nRun gofmt.\n"
	if text != want {
		t.Errorf("text =\n%q\nwant\n%q", text, want)
	}
	if strings.Join(names, ",") != "AGENTS.md,CLAUDE.md" {
		t.Errorf("names = %v", names)
	}
	if strings.Contains(text, "not an instruction file") {
		t.Error("README.md must not be read")
	}
}

func TestLoadInstructionsSkipsEmptyAndPrefersHX(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "HRN.md", "hrn first")
	writeFile(t, dir, "AGENTS.md", "   \n\n")
	text, names, _, err := load(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "HRN.md" {
		t.Errorf("an empty file should be skipped: %v", names)
	}
	if !strings.HasPrefix(text, "# Project instructions\n\n## HRN.md\n\nhrn first") {
		t.Errorf("text = %q", text)
	}
}

func TestLoadInstructionsTruncates(t *testing.T) {
	dir := t.TempDir()
	// Multi-byte runes so a naive cut would leave invalid UTF-8.
	writeFile(t, dir, "AGENTS.md", strings.Repeat("é", 40*1024))
	writeFile(t, dir, "CLAUDE.md", "never reached")
	text, names, truncated, err := load(t, dir)
	if err != nil || !truncated {
		t.Fatalf("truncated=%v err=%v", truncated, err)
	}
	if !strings.HasSuffix(text, truncatedMarker) {
		t.Errorf("text should end with the marker, got %q", text[max(0, len(text)-80):])
	}
	if len(text) > maxInstructions+len(truncatedMarker) {
		t.Errorf("len(text) = %d, want <= %d", len(text), maxInstructions+len(truncatedMarker))
	}
	if !strings.HasPrefix(text, "# Project instructions\n\n## AGENTS.md\n\n") {
		t.Error("the heading must survive truncation")
	}
	if strings.Join(names, ",") != "AGENTS.md" {
		t.Errorf("names = %v; nothing is read after the cap", names)
	}
	for _, r := range text {
		if r == '�' {
			t.Fatal("truncation must not split a rune")
		}
	}
}

func TestLoadInstructionsDropsASectionThatCannotFit(t *testing.T) {
	dir := t.TempDir()
	// AGENTS.md leaves less room than CLAUDE.md's heading needs.
	writeFile(t, dir, "AGENTS.md", strings.Repeat("a", maxInstructions-40))
	writeFile(t, dir, "CLAUDE.md", "dropped whole")
	text, names, truncated, err := load(t, dir)
	if err != nil || !truncated {
		t.Fatalf("truncated=%v err=%v", truncated, err)
	}
	if strings.Join(names, ",") != "AGENTS.md" {
		t.Errorf("a file that contributed nothing must not be listed: %v", names)
	}
	if strings.Contains(text, "## CLAUDE.md") {
		t.Error("a heading without content must not be written")
	}
	if strings.Contains(text, "dropped whole") {
		t.Error("the dropped file's content must not appear")
	}
}

func TestLoadInstructionsRefusesSymlinkOutOfTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("private key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Skip("symlinks not available:", err)
	}
	text, _, _, err := load(t, dir)
	if err == nil {
		t.Fatal("a symlink out of the workspace must be refused")
	}
	if strings.Contains(text, "private key") {
		t.Fatal("the linked file's content was read")
	}
}

func TestLoadInstructionsReadError(t *testing.T) {
	dir := t.TempDir()
	// A directory named like an instruction file cannot be read as one.
	if err := os.Mkdir(filepath.Join(dir, "CLAUDE.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := load(t, dir); err == nil {
		t.Fatal("an unreadable instruction file should be reported")
	}
}
