package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeTree writes files under root, creating their directories.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// isolateSetupHome points the user-level setup at an empty directory of its own.
func isolateSetupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	old := setupHome
	setupHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { setupHome = old })
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	return home
}

// ownFiles are the recorded files, less any CLAUDE.md the folders above the
// test's temp directory happen to hold.
func ownFiles(s *Setup) []string {
	var out []string
	for name := range s.Files {
		if !strings.HasPrefix(name, "../") {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func claudeProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"CLAUDE.md":                     "@AGENTS.md\n\nClaude lines.\n",
		"AGENTS.md":                     "# Guide\n",
		".claude/rules/tests.md":        "Tests first.\n",
		".claude/skills/check/SKILL.md": "---\nname: check\n---\n",
		".claude/settings.json":         `{"enabledPlugins": {"review@market": true, "off@market": false}}`,
		".mcp.json":                     `{"mcpServers": {"docs": {"command": "docs-server"}, "web": {"type": "http", "url": "https://example.test"}}}`,
		"README.md":                     "not loaded\n",
	})
	return dir
}

func TestSetupDigestIsStableForTheSameSetup(t *testing.T) {
	isolateSetupHome(t)
	dir := claudeProject(t)
	task := Task{Dir: dir}
	a, b := ResolveSetup("claude", task), ResolveSetup("claude", task)
	if a.Digest == "" || a.BaseDigest == "" {
		t.Fatalf("no digest: %+v", a)
	}
	if a.Digest != b.Digest || a.BaseDigest != b.BaseDigest {
		t.Fatalf("digest moved between two reads of one setup: %s %s", a.Digest, b.Digest)
	}
	// A copy of the tree elsewhere is the same setup.
	other := claudeProject(t)
	if c := ResolveSetup("claude", Task{Dir: other}); c.Digest != a.Digest {
		t.Fatalf("the same files in another directory give another digest")
	}
}

func TestSetupRecordsWhatClaudeLoadsAsHashesOnly(t *testing.T) {
	home := isolateSetupHome(t)
	writeTree(t, filepath.Join(home, ".claude"), map[string]string{
		"CLAUDE.md":                  "Owner rules.\n",
		"rules/guard.md":             "Guard rails.\n",
		"skills/deliver/SKILL.md":    "---\nname: deliver\n---\n",
		"settings.json":              `{"enabledPlugins": {"pr@tools": true}}`,
		"skills/notaskill/README.md": "no SKILL.md here\n",
	})
	dir := claudeProject(t)
	s := ResolveSetup("claude", Task{Dir: dir})

	wantFiles := []string{".claude/rules/tests.md", ".claude/settings.json", ".claude/skills/check/SKILL.md",
		"AGENTS.md", "CLAUDE.md", "~/.claude/CLAUDE.md", "~/.claude/rules/guard.md", "~/.claude/settings.json", "~/.claude/skills/deliver/SKILL.md"}
	got := ownFiles(s)
	for _, name := range got {
		if len(s.Files[name]) != 64 {
			t.Errorf("%s: %q is not a sha256 hex digest", name, s.Files[name])
		}
	}
	if !slices.Equal(got, wantFiles) {
		t.Fatalf("files = %v, want %v", got, wantFiles)
	}
	if !slices.Equal(s.Skills, []string{"check", "deliver"}) {
		t.Errorf("skills = %v", s.Skills)
	}
	if !slices.Equal(s.Plugins, []string{"pr@tools", "review@market"}) {
		t.Errorf("plugins = %v", s.Plugins)
	}
	if !slices.Equal(s.MCP, []string{"docs", "web"}) {
		t.Errorf("mcp = %v", s.MCP)
	}
	data, _ := json.Marshal(s)
	for _, text := range []string{"Claude lines", "Owner rules", "Tests first", "docs-server"} {
		if strings.Contains(string(data), text) {
			t.Errorf("the setup record holds file contents (%q): %s", text, data)
		}
	}
}

func TestSetupDigestChangesWithEveryInput(t *testing.T) {
	isolateSetupHome(t)
	base := ResolveSetup("claude", Task{Dir: claudeProject(t)})
	mcpCfg := filepath.Join(t.TempDir(), "extra.json")
	writeTree(t, filepath.Dir(mcpCfg), map[string]string{"extra.json": `{"mcpServers": {"extra": {"command": "x"}}}`})

	cases := []struct {
		name     string
		files    map[string]string
		opts     func(*Task)
		onlyFull bool // a harness option: the base digest stays
	}{
		{name: "an imported file", files: map[string]string{"AGENTS.md": "# Guide, changed\n"}},
		{name: "a rule", files: map[string]string{".claude/rules/new.md": "New rule.\n"}},
		{name: "a skill", files: map[string]string{".claude/skills/other/SKILL.md": "---\nname: other\n---\n"}},
		{name: "a plugin", files: map[string]string{".claude/settings.json": `{"enabledPlugins": {"review@market": true, "off@market": true}}`}},
		{name: "a project MCP server", files: map[string]string{".mcp.json": `{"mcpServers": {"docs": {"command": "docs-server"}}}`}},
		{name: "a run MCP config", opts: func(t *Task) { t.MCP = []string{mcpCfg} }},
		{name: "append_system_prompt", opts: func(t *Task) { t.AppendSystemPrompt = "Be brief." }, onlyFull: true},
		{name: "no_skills", opts: func(t *Task) { t.NoSkills = true }, onlyFull: true},
		{name: "lean off", opts: func(t *Task) { t.Lean = Bool(false) }, onlyFull: true},
		{name: "skills", opts: func(t *Task) { t.Skills = Bool(true) }, onlyFull: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := claudeProject(t)
			writeTree(t, dir, c.files)
			task := Task{Dir: dir}
			if c.opts != nil {
				c.opts(&task)
			}
			s := ResolveSetup("claude", task)
			if s.Digest == base.Digest {
				t.Fatalf("digest unchanged")
			}
			if c.onlyFull != (s.BaseDigest == base.BaseDigest) {
				t.Fatalf("base digest changed = %v, want %v", s.BaseDigest != base.BaseDigest, !c.onlyFull)
			}
		})
	}
}

func TestSetupOfEachBackendNamesItsOwnInstructionFiles(t *testing.T) {
	isolateSetupHome(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"HRN.md": "h", "AGENTS.md": "a", "CLAUDE.md": "c", "GEMINI.md": "g", "AGENTS.override.md": "o"})
	cases := map[string][]string{
		"api":      {"AGENTS.md", "CLAUDE.md", "HRN.md"},
		"codex":    {"AGENTS.md", "AGENTS.override.md"},
		"gemini":   {"GEMINI.md"},
		"opencode": {"AGENTS.md", "CLAUDE.md"},
		"agy":      {"AGENTS.md", "GEMINI.md"},
	}
	for backend, want := range cases {
		if got := ownFiles(ResolveSetup(backend, Task{Dir: dir})); !slices.Equal(got, want) {
			t.Errorf("%s: files = %v, want %v", backend, got, want)
		}
	}
}

func TestSetupOptionsCountOnlyWhereTheBackendAppliesThem(t *testing.T) {
	isolateSetupHome(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"AGENTS.md": "a"})
	plain := ResolveSetup("codex", Task{Dir: dir})
	task := Task{Dir: dir}
	task.NoSkills, task.Skills, task.NoInstructions = true, Bool(true), true
	if s := ResolveSetup("codex", task); s.Digest != plain.Digest {
		t.Fatalf("options codex ignores changed its digest")
	}
	mcpCfg := filepath.Join(t.TempDir(), "extra.json")
	writeTree(t, filepath.Dir(mcpCfg), map[string]string{"extra.json": `{"mcpServers": {"extra": {"url": "https://example.test"}}}`})
	task = Task{Dir: dir}
	task.MCP = []string{mcpCfg}
	if s := ResolveSetup("codex", task); s.Digest == plain.Digest || !slices.Contains(s.MCP, "extra") {
		t.Fatalf("a run MCP config on codex: digest changed %v, mcp %v", s.Digest != plain.Digest, s.MCP)
	}
	claude := Task{Dir: dir}
	claude.NoSkills = true
	both := claude
	both.Skills = Bool(true)
	if ResolveSetup("claude", both).Digest != ResolveSetup("claude", claude).Digest {
		t.Fatalf("skills beside no_skills starts claude the same way but changed its digest")
	}
	task = Task{Dir: dir}
	task.Lean = Bool(false)
	if s := ResolveSetup("codex", task); s.Digest == plain.Digest {
		t.Fatalf("lean off on codex left its digest unchanged")
	}
	api := ResolveSetup("api", Task{Dir: dir})
	task = Task{Dir: dir}
	task.NoInstructions = true
	if s := ResolveSetup("api", task); s.Digest == api.Digest || s.BaseDigest != api.BaseDigest {
		t.Fatalf("no_instructions on api: digest %v base %v", s.Digest != api.Digest, s.BaseDigest != api.BaseDigest)
	}
}

func TestSetupImportsStayInsideTheWorkspace(t *testing.T) {
	isolateSetupHome(t)
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"secret.md": "x"})
	dir := t.TempDir()
	rel, err := filepath.Rel(dir, filepath.Join(outside, "secret.md"))
	if err != nil {
		t.Skip("no relative path between temp dirs")
	}
	writeTree(t, dir, map[string]string{
		"CLAUDE.md":  "@" + filepath.ToSlash(rel) + "\n@docs/a.md\n```\n@docs/b.md\n```\nmail me@example.test\n",
		"docs/a.md":  "@a2.md\n",
		"docs/a2.md": "nested\n",
		"docs/b.md":  "in a code block\n",
		"me":         "not an import: the @ is inside a word\n",
	})
	want := []string{"CLAUDE.md", "docs/a.md", "docs/a2.md"}
	if got := ownFiles(ResolveSetup("claude", Task{Dir: dir})); !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
}

func TestSetupOfAnEmptyWorkspaceStillHasADigest(t *testing.T) {
	isolateSetupHome(t)
	s := ResolveSetup("claude", Task{Dir: t.TempDir()})
	if s == nil || s.Digest == "" || len(ownFiles(s)) != 0 || s.Backend != "claude" {
		t.Fatalf("setup = %+v", s)
	}
}

func TestSetupOfClaudeIncludesTheCLAUDEFilesInTheFoldersAbove(t *testing.T) {
	isolateSetupHome(t)
	repo := t.TempDir()
	ws := filepath.Join(repo, ".hrn", "worktrees", "t1")
	writeTree(t, repo, map[string]string{"CLAUDE.md": "main checkout\n", "CLAUDE.local.md": "owner notes\n", ".hrn/worktrees/t1/CLAUDE.md": "tracked\n"})
	s := ResolveSetup("claude", Task{Dir: ws})
	for _, key := range []string{"CLAUDE.md", "../../../CLAUDE.md", "../../../CLAUDE.local.md"} {
		if s.Files[key] == "" {
			t.Errorf("%s not recorded: %v", key, s.Files)
		}
	}
	writeTree(t, repo, map[string]string{"CLAUDE.local.md": "owner notes, edited\n"})
	if again := ResolveSetup("claude", Task{Dir: ws}); again.BaseDigest == s.BaseDigest {
		t.Fatalf("an edit to a CLAUDE.local.md above the workspace left the digest as it was")
	}
}

func TestSetupReadsTheConfigDirsTheEnvironmentNames(t *testing.T) {
	isolateSetupHome(t)
	claudeDir, codexDir := t.TempDir(), t.TempDir()
	writeTree(t, claudeDir, map[string]string{"CLAUDE.md": "c"})
	writeTree(t, codexDir, map[string]string{"AGENTS.md": "x"})
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Setenv("CODEX_HOME", codexDir)
	if s := ResolveSetup("claude", Task{Dir: t.TempDir()}); s.Files["$CLAUDE_CONFIG_DIR/CLAUDE.md"] == "" {
		t.Errorf("claude: %v", s.Files)
	}
	if s := ResolveSetup("codex", Task{Dir: t.TempDir()}); s.Files["$CODEX_HOME/AGENTS.md"] == "" {
		t.Errorf("codex: %v", s.Files)
	}
}

func TestSetupDigestChangesWithAUserLevelFile(t *testing.T) {
	home := isolateSetupHome(t)
	dir := claudeProject(t)
	before := ResolveSetup("claude", Task{Dir: dir})
	for _, f := range []struct{ name, body string }{
		{"CLAUDE.md", "rules"}, {"rules/r.md", "rule"}, {"skills/s/SKILL.md", "skill"}, {"settings.json", `{"hooks": {}}`},
	} {
		writeTree(t, filepath.Join(home, ".claude"), map[string]string{f.name: f.body})
		after := ResolveSetup("claude", Task{Dir: dir})
		if after.BaseDigest == before.BaseDigest {
			t.Errorf("~/.claude/%s left the base digest as it was", f.name)
		}
		before = after
	}
}

func TestSetupOfApiNamesOnlyTheStdioServersItStarts(t *testing.T) {
	isolateSetupHome(t)
	cfg := filepath.Join(t.TempDir(), "mcp.json")
	writeTree(t, filepath.Dir(cfg), map[string]string{"mcp.json": `{"mcpServers": {"docs": {"command": "d"}, "web": {"type": "http", "url": "https://example.test"}}}`})
	task := Task{Dir: t.TempDir()}
	task.MCP = []string{cfg}
	if s := ResolveSetup("api", task); !slices.Equal(s.MCP, []string{"docs"}) {
		t.Errorf("api mcp = %v", s.MCP)
	}
	if s := ResolveSetup("claude", task); !slices.Equal(s.MCP, []string{"docs", "web"}) {
		t.Errorf("claude mcp = %v", s.MCP)
	}
}

func TestSetupDoesNotHashALinkOutOfTheWorkspace(t *testing.T) {
	isolateSetupHome(t)
	secret := filepath.Join(t.TempDir(), "id_rsa")
	writeTree(t, filepath.Dir(secret), map[string]string{"id_rsa": "key"})
	dir := t.TempDir()
	if err := os.Symlink(secret, filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if got := ResolveSetup("api", Task{Dir: dir}).Files["AGENTS.md"]; got != fileOutsideTree {
		t.Fatalf("AGENTS.md = %q, want %q", got, fileOutsideTree)
	}
}

func TestSetupFollowsImportCyclesOnceAndStopsAtTheHopLimit(t *testing.T) {
	isolateSetupHome(t)
	dir := t.TempDir()
	files := map[string]string{"CLAUDE.md": "@a.md\n", "a.md": "@b.md\n", "b.md": "@a.md\n@c1.md\n"}
	for i := 1; i <= maxImportHops+2; i++ {
		files[fmt.Sprintf("c%d.md", i)] = fmt.Sprintf("@c%d.md\n", i+1)
	}
	writeTree(t, dir, files)
	s := ResolveSetup("claude", Task{Dir: dir})
	if s.Files["a.md"] == "" || s.Files["b.md"] == "" {
		t.Fatalf("cycle not followed: %v", s.Files)
	}
	// CLAUDE.md is hop 0, a.md 1, b.md 2, c1.md 3: c2.md is hop 4, c3.md hop 5, c4.md beyond.
	if s.Files["c3.md"] == "" || s.Files["c4.md"] != "" {
		t.Fatalf("hop limit: %v", ownFiles(s))
	}
}

func TestSetupSaysWhenTheFileCapCutItShort(t *testing.T) {
	isolateSetupHome(t)
	dir := t.TempDir()
	files := map[string]string{}
	for i := range maxSetupFiles + 1 {
		files[fmt.Sprintf(".agent/rules/r%03d.md", i)] = "r"
	}
	writeTree(t, dir, files)
	s := ResolveSetup("agy", Task{Dir: dir})
	if !s.Truncated || len(s.Files) != maxSetupFiles {
		t.Fatalf("truncated = %v with %d files", s.Truncated, len(s.Files))
	}
}
