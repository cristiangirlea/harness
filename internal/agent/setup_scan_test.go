package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestSetupDigestCountsTheRunOptionsAnEvalVariantSets(t *testing.T) {
	isolateSetupHome(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"AGENTS.md": "a"})
	base := ResolveSetup("api", Task{Dir: dir})
	for name, set := range map[string]func(*Options){
		"shape_tools": func(o *Options) { o.ShapeTools = true },
		"retrieval":   func(o *Options) { o.Retrieval = "optional" },
	} {
		task := Task{Dir: dir}
		set(&task.Options)
		s := ResolveSetup("api", task)
		if s.Digest == base.Digest || s.BaseDigest != base.BaseDigest {
			t.Errorf("%s: digest changed %v, base changed %v", name, s.Digest != base.Digest, s.BaseDigest != base.BaseDigest)
		}
	}
	task := Task{Dir: dir}
	task.Retrieval = "off"
	if s := ResolveSetup("api", task); s.Digest != base.Digest {
		t.Errorf("retrieval off changed the digest")
	}
}

func TestSetupWithAPromptPrefixChangesOnlyTheFullDigest(t *testing.T) {
	s := &Setup{Digest: "d", BaseDigest: "b"}
	a, b := s.WithOptions(PromptPrefixOption("Read the test first.")), s.WithOptions(PromptPrefixOption("Read the docs first."))
	if a.Digest == s.Digest || a.Digest == b.Digest || a.BaseDigest != "b" || s.Digest != "d" {
		t.Fatalf("with prefix: %+v, %+v, original %+v", a, b, s)
	}
	if s.WithOptions() != s || (*Setup)(nil).WithOptions("x") != nil {
		t.Fatalf("no lines, or no setup, should come back as they are")
	}
}

func TestSetupOfClaudeNamesTheServersInClaudeJSON(t *testing.T) {
	home := isolateSetupHome(t)
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{"numStartups": 9, "history": ["private"], "mcpServers": {"user-wide": {"command": "u"}},
		"projects": {%q: {"mcpServers": {"this-project": {"command": "p"}}}, "/elsewhere": {"mcpServers": {"other": {"command": "o"}}}}}`,
		filepath.ToSlash(real))
	writeTree(t, home, map[string]string{".claude.json": cfg})
	s := ResolveSetup("claude", Task{Dir: dir})
	if !slices.Equal(s.MCP, []string{"this-project", "user-wide"}) {
		t.Fatalf("mcp = %v", s.MCP)
	}
	for name := range s.Files {
		if strings.Contains(name, ".claude.json") {
			t.Fatalf(".claude.json holds history; it must not be recorded: %v", s.Files)
		}
	}
	if s := ResolveSetup("codex", Task{Dir: dir}); len(s.MCP) != 0 {
		t.Errorf("codex does not read .claude.json: %v", s.MCP)
	}
}

func TestSetupPluginsFollowTheSettingsPrecedence(t *testing.T) {
	home := isolateSetupHome(t)
	writeTree(t, filepath.Join(home, ".claude"), map[string]string{"settings.json": `{"enabledPlugins": {"a@m": true, "b@m": true}}`})
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".claude/settings.json":       `{"enabledPlugins": {"b@m": false, "c@m": true}}`,
		".claude/settings.local.json": `{"enabledPlugins": {"c@m": false, "d@m": true}}`,
	})
	if got := ResolveSetup("claude", Task{Dir: dir}).Plugins; !slices.Equal(got, []string{"a@m", "d@m"}) {
		t.Fatalf("plugins = %v", got)
	}
}

func TestSetupAtExactlyTheFileCapIsNotTruncated(t *testing.T) {
	isolateSetupHome(t)
	dir := t.TempDir()
	files := map[string]string{}
	for i := range maxSetupFiles {
		files[fmt.Sprintf(".agent/rules/r%03d.md", i)] = "r"
	}
	writeTree(t, dir, files)
	full := ResolveSetup("agy", Task{Dir: dir})
	if full.Truncated || len(full.Files) != maxSetupFiles {
		t.Fatalf("truncated = %v with %d files", full.Truncated, len(full.Files))
	}
	writeTree(t, dir, map[string]string{".agent/rules/zzz.md": "one more"})
	if over := ResolveSetup("agy", Task{Dir: dir}); !over.Truncated || over.BaseDigest == full.BaseDigest {
		t.Fatalf("one file past the cap: truncated %v, base changed %v", over.Truncated, over.BaseDigest != full.BaseDigest)
	}
}

func TestSetupProjectImportCannotTakeAUserFilesName(t *testing.T) {
	home := isolateSetupHome(t)
	writeTree(t, filepath.Join(home, ".claude"), map[string]string{"CLAUDE.md": "the real one"})
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"CLAUDE.md": "@./~/.claude/CLAUDE.md\n", "~/.claude/CLAUDE.md": "a decoy"})
	s := ResolveSetup("claude", Task{Dir: dir})
	if want := textHash("the real one"); s.Files["~/.claude/CLAUDE.md"] != want {
		t.Fatalf("~/.claude/CLAUDE.md = %s, want the user file's hash %s", s.Files["~/.claude/CLAUDE.md"], want)
	}
}

func TestSetupWalksTheFoldersAboveOnlyForClaude(t *testing.T) {
	isolateSetupHome(t)
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{"CLAUDE.md": "above", "ws/AGENTS.md": "a"})
	for _, backend := range []string{"api", "codex", "gemini", "opencode", "agy"} {
		for name := range ResolveSetup(backend, Task{Dir: filepath.Join(repo, "ws")}).Files {
			if strings.HasPrefix(name, "../") {
				t.Errorf("%s recorded %s", backend, name)
			}
		}
	}
}

func TestSetupHashesALargeFileWhole(t *testing.T) {
	isolateSetupHome(t)
	dir := t.TempDir()
	body := strings.Repeat("x", maxImportScan) + "\n@late.md\n"
	writeTree(t, dir, map[string]string{"CLAUDE.md": body, "late.md": "past the scanned part"})
	s := ResolveSetup("claude", Task{Dir: dir})
	sum := sha256.Sum256([]byte(body))
	if s.Files["CLAUDE.md"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("CLAUDE.md hash is not the sha256 of the whole file")
	}
	if _, ok := s.Files["late.md"]; ok {
		t.Errorf("an import past the scanned part was followed")
	}
}

func TestSetupWalksALinkedRulesFolder(t *testing.T) {
	isolateSetupHome(t)
	shared := t.TempDir()
	writeTree(t, shared, map[string]string{"style.md": "shared rule"})
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(dir, ".claude", "rules")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if got := ResolveSetup("claude", Task{Dir: dir}).Files[".claude/rules/style.md"]; got != textHash("shared rule") {
		t.Fatalf(".claude/rules/style.md = %q, want the shared rule's hash", got)
	}
}

func TestSetupHashesALinkedDotfileOfTheSameKind(t *testing.T) {
	home := isolateSetupHome(t)
	dotfiles := t.TempDir()
	writeTree(t, dotfiles, map[string]string{"claude.md": "from dotfiles"})
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dotfiles, "claude.md"), filepath.Join(home, ".claude", "CLAUDE.md")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if got := ResolveSetup("claude", Task{Dir: t.TempDir()}).Files["~/.claude/CLAUDE.md"]; got != textHash("from dotfiles") {
		t.Fatalf("~/.claude/CLAUDE.md = %q", got)
	}
}

func TestSetupMarksAFileItCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not deny reads on Windows")
	}
	isolateSetupHome(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"AGENTS.md": "a"})
	path := filepath.Join(dir, "AGENTS.md")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("running as a user who reads anything")
	}
	if got := ResolveSetup("api", Task{Dir: dir}).Files["AGENTS.md"]; got != fileUnreadable {
		t.Fatalf("AGENTS.md = %q, want %q", got, fileUnreadable)
	}
}

func TestSetupDigestCountsTheRetrievalVersion(t *testing.T) {
	isolateSetupHome(t)
	dir := t.TempDir()
	a, b := Task{Dir: dir}, Task{Dir: dir}
	a.Retrieval, b.Retrieval, b.RetrievalVersion = "optional", "optional", "v2"
	if ResolveSetup("api", a).Digest == ResolveSetup("api", b).Digest {
		t.Fatalf("the retrieval version left the digest as it was")
	}
}

func TestSetupFollowsAnImportFromTheHomeFolder(t *testing.T) {
	home := isolateSetupHome(t)
	writeTree(t, home, map[string]string{".claude/extra.md": "personal", "notes.txt": "not markdown"})
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"CLAUDE.md": "@~/.claude/extra.md\n@~/notes.txt\n"})
	before := ResolveSetup("claude", Task{Dir: dir})
	if before.Files["~/.claude/extra.md"] != textHash("personal") {
		t.Fatalf("files = %v", before.Files)
	}
	if _, ok := before.Files["~/notes.txt"]; ok {
		t.Errorf("a home import that is not Markdown was recorded")
	}
	writeTree(t, home, map[string]string{".claude/extra.md": "personal, edited"})
	if after := ResolveSetup("claude", Task{Dir: dir}); after.BaseDigest == before.BaseDigest {
		t.Fatalf("editing an imported home file left the digest as it was")
	}
}

func TestSetupReadsClaudeJSONFromTheConfigDir(t *testing.T) {
	home := isolateSetupHome(t)
	writeTree(t, home, map[string]string{".claude.json": `{"mcpServers": {"from-home": {"command": "h"}}}`})
	cfg := t.TempDir()
	writeTree(t, cfg, map[string]string{".claude.json": `{"mcpServers": {"from-config-dir": {"command": "c"}}}`})
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if got := ResolveSetup("claude", Task{Dir: t.TempDir()}).MCP; !slices.Equal(got, []string{"from-config-dir"}) {
		t.Fatalf("mcp = %v", got)
	}
}

func TestSetupMatchesTheClaudeJSONProjectByTheFolderAsGiven(t *testing.T) {
	home := isolateSetupHome(t)
	dir := t.TempDir()
	key := filepath.ToSlash(dir)
	if runtime.GOOS == "windows" {
		key = strings.ToUpper(key)
	}
	writeTree(t, home, map[string]string{".claude.json": fmt.Sprintf(`{"projects": {%q: {"mcpServers": {"p": {"command": "p"}}}}}`, key)})
	if got := ResolveSetup("claude", Task{Dir: dir}).MCP; !slices.Equal(got, []string{"p"}) {
		t.Fatalf("mcp = %v", got)
	}
}

func TestSetupWalksALinkedSubfolderOfRulesOnce(t *testing.T) {
	isolateSetupHome(t)
	shared := t.TempDir()
	writeTree(t, shared, map[string]string{"style.md": "shared"})
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".claude/rules/own.md": "own"})
	if err := os.Symlink(shared, filepath.Join(dir, ".claude", "rules", "team")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	// A link back up would loop without the visited set.
	if err := os.Symlink(filepath.Join(dir, ".claude", "rules"), filepath.Join(shared, "loop")); err != nil {
		t.Fatal(err)
	}
	s := ResolveSetup("claude", Task{Dir: dir})
	want := []string{".claude/rules/own.md", ".claude/rules/team/style.md"}
	if got := ownFiles(s); s.Truncated || !slices.Equal(got, want) {
		t.Fatalf("truncated %v, files = %v, want %v", s.Truncated, got, want)
	}
}

func TestSetupWalksTheFoldersAboveWhereALinkedWorkspaceLeads(t *testing.T) {
	isolateSetupHome(t)
	target := t.TempDir()
	writeTree(t, target, map[string]string{"CLAUDE.md": "above the target", "ws/AGENTS.md": "a"})
	links := t.TempDir()
	writeTree(t, links, map[string]string{"CLAUDE.md": "above the link"})
	link := filepath.Join(links, "ws")
	if err := os.Symlink(filepath.Join(target, "ws"), link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if got := ResolveSetup("claude", Task{Dir: link}).Files["../CLAUDE.md"]; got != textHash("above the target") {
		t.Fatalf("../CLAUDE.md = %q, want the hash of the file above the target", got)
	}
}

func TestSetupMatchesTheClaudeJSONProjectByALinkedFolder(t *testing.T) {
	home := isolateSetupHome(t)
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	writeTree(t, home, map[string]string{".claude.json": fmt.Sprintf(`{"projects": {%q: {"mcpServers": {"p": {"command": "p"}}}}}`, filepath.ToSlash(link))})
	if got := ResolveSetup("claude", Task{Dir: link}).MCP; !slices.Equal(got, []string{"p"}) {
		t.Fatalf("mcp = %v", got)
	}
}

func TestSetupHomeImportCannotLeaveTheHomeFolder(t *testing.T) {
	home := isolateSetupHome(t)
	writeTree(t, filepath.Dir(home), map[string]string{"beside.md": "next to home"})
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"CLAUDE.md": "@~/../beside.md\n"})
	for name := range ResolveSetup("claude", Task{Dir: dir}).Files {
		if strings.Contains(name, "beside") {
			t.Fatalf("an import out of the home folder was recorded as %s", name)
		}
	}
}

func TestSetupKeepsTheConfigDirAndAHomeImportApart(t *testing.T) {
	home := isolateSetupHome(t)
	writeTree(t, home, map[string]string{".claude/CLAUDE.md": "home copy"})
	cfg := t.TempDir()
	writeTree(t, cfg, map[string]string{"CLAUDE.md": "loaded"})
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"CLAUDE.md": "@~/.claude/CLAUDE.md\n"})
	before := ResolveSetup("claude", Task{Dir: dir})
	if before.Files["$CLAUDE_CONFIG_DIR/CLAUDE.md"] != textHash("loaded") || before.Files["~/.claude/CLAUDE.md"] != textHash("home copy") {
		t.Fatalf("files = %v", ownFiles(before))
	}
	for _, edit := range []struct{ dir, body string }{{filepath.Join(home, ".claude"), "home, edited"}, {cfg, "loaded, edited"}} {
		writeTree(t, edit.dir, map[string]string{"CLAUDE.md": edit.body})
		after := ResolveSetup("claude", Task{Dir: dir})
		if after.BaseDigest == before.BaseDigest {
			t.Errorf("editing %s left the digest as it was", filepath.Join(edit.dir, "CLAUDE.md"))
		}
		before = after
	}
}

func TestSetupNamingTheDefaultConfigDirChangesNothing(t *testing.T) {
	home := isolateSetupHome(t)
	writeTree(t, home, map[string]string{".claude/CLAUDE.md": "rules", ".codex/AGENTS.md": "a"})
	dir := t.TempDir()
	plain := map[string]string{"claude": ResolveSetup("claude", Task{Dir: dir}).Digest, "codex": ResolveSetup("codex", Task{Dir: dir}).Digest}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	for backend, want := range plain {
		if got := ResolveSetup(backend, Task{Dir: dir}).Digest; got != want {
			t.Errorf("%s: naming the default folder changed the digest", backend)
		}
	}
}

func TestSetupReadsARelativeConfigDirFromTheWorkspace(t *testing.T) {
	isolateSetupHome(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"cfg/CLAUDE.md": "c"})
	t.Setenv("CLAUDE_CONFIG_DIR", "cfg")
	if got := ResolveSetup("claude", Task{Dir: dir}).Files["$CLAUDE_CONFIG_DIR/CLAUDE.md"]; got != textHash("c") {
		t.Fatalf("$CLAUDE_CONFIG_DIR/CLAUDE.md = %q", got)
	}
}
