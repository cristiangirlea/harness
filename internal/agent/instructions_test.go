package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadInstructions(t *testing.T) {
	dir := t.TempDir()
	if text, names, err := loadInstructions(dir); err != nil || text != "" || names != nil {
		t.Fatalf("empty workspace: text=%q names=%v err=%v", text, names, err)
	}

	// Files are read in a fixed order, whatever order they were created in.
	writeFile(t, dir, "CLAUDE.md", "Run gofmt.\r\n")
	writeFile(t, dir, "AGENTS.md", "  Use table tests.  \n")
	writeFile(t, dir, "README.md", "not an instruction file")
	text, names, err := loadInstructions(dir)
	if err != nil {
		t.Fatal(err)
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
	writeFile(t, dir, "HX.md", "hx first")
	writeFile(t, dir, "AGENTS.md", "   \n\n")
	text, names, err := loadInstructions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "HX.md" {
		t.Errorf("an empty file should be skipped: %v", names)
	}
	if !strings.HasPrefix(text, "# Project instructions\n\n## HX.md\n\nhx first") {
		t.Errorf("text = %q", text)
	}
}

func TestLoadInstructionsTruncates(t *testing.T) {
	dir := t.TempDir()
	// Multi-byte runes so a naive cut would leave invalid UTF-8.
	writeFile(t, dir, "AGENTS.md", strings.Repeat("é", 40*1024))
	writeFile(t, dir, "CLAUDE.md", "never reached")
	text, names, err := loadInstructions(dir)
	if err != nil {
		t.Fatal(err)
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
	if strings.ContainsRune(text, '�') || !utf8Valid(text) {
		t.Error("truncation must not split a rune")
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestLoadInstructionsReadError(t *testing.T) {
	dir := t.TempDir()
	// A directory named like an instruction file cannot be read as one.
	if err := os.Mkdir(filepath.Join(dir, "CLAUDE.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadInstructions(dir); err == nil {
		t.Fatal("an unreadable instruction file should be reported")
	}
}
