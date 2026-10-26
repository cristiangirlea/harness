package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/cristiangirlea/harness/internal/mcp"
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

var setupHome = os.UserHomeDir

const (
	setupVersion    = "hrn-setup 1"
	maxSetupFiles   = 500
	maxImportHops   = 5
	maxImportScan   = 256 * 1024
	fileUnreadable  = "unreadable"
	fileOutsideTree = "outside"
)

// ResolveSetup records unreadable inputs without failing.
func ResolveSetup(backend string, task Task) *Setup {
	s := &setupScan{files: map[string]string{}}
	dir, err := filepath.Abs(task.Dir)
	if err != nil {
		dir = task.Dir
	}
	project := newSetupTree(dir, "")
	home, _ := setupHome()
	user := func(rel, env, prefix string) setupTree {
		if d := os.Getenv(env); env != "" && d != "" {
			if !filepath.IsAbs(d) {
				d = filepath.Join(dir, d)
			}
			if home == "" || !samePath(d, filepath.Join(home, rel)) {
				return newSetupTree(d, "$"+env+"/")
			}
		}
		if home == "" {
			return setupTree{}
		}
		return newSetupTree(filepath.Join(home, rel), prefix)
	}
	switch backend {
	case "api":
		s.file(project, instructionFiles()...)
		s.mcpConfigs(task.MCP, false)
	case "claude":
		claudeDir := user(".claude", "CLAUDE_CONFIG_DIR", "~/.claude/")
		s.imports = true
		if home != "" {
			s.home = newSetupTree(home, "~/")
		}
		s.file(claudeDir, "CLAUDE.md", "settings.json")
		s.mdDir(claudeDir, "rules")
		s.skills(claudeDir, "skills")
		s.file(project, "CLAUDE.md", "CLAUDE.local.md", ".claude/CLAUDE.md", ".claude/settings.json", ".claude/settings.local.json")
		s.mdDir(project, ".claude/rules")
		s.skills(project, ".claude/skills")
		// Claude also loads ancestor instructions above worktrees.
		up := ""
		for d := filepath.Dir(project.real); d != filepath.Dir(d); d = filepath.Dir(d) {
			up += "../"
			s.file(newSetupTree(d, up), "CLAUDE.md", "CLAUDE.local.md")
		}
		// Later files override earlier ones, as in Claude Code.
		s.plugins(claudeDir.file("settings.json"), project.file(".claude/settings.json"), project.file(".claude/settings.local.json"))
		s.mcpConfigs(append([]string{filepath.Join(dir, ".mcp.json")}, task.MCP...), true)
		s.claudeUserMCP(claudeDir, home, dir, project.real)
	case "codex":
		s.file(project, "AGENTS.override.md", "AGENTS.md")
		s.file(user(".codex", "CODEX_HOME", "~/.codex/"), "AGENTS.override.md", "AGENTS.md")
		s.mcpConfigs(task.MCP, true)
	case "gemini":
		s.file(project, "GEMINI.md")
		s.file(user(".gemini", "", "~/.gemini/"), "GEMINI.md")
	case "opencode":
		s.file(project, "AGENTS.md", "CLAUDE.md")
		s.file(user(filepath.Join(".config", "opencode"), "", "~/.config/opencode/"), "AGENTS.md")
	case "agy":
		s.file(project, "AGENTS.md", "GEMINI.md")
		s.mdDir(project, ".agent/rules")
		s.file(user(".gemini", "", "~/.gemini/"), "GEMINI.md")
	default:
		s.file(project, instructionFiles()...)
	}
	out := s.setup(optionLines(backend, task.Options))
	out.Backend = backend
	return out
}

// Ignored options must not distinguish equivalent setups.
func optionLines(backend string, o Options) []string {
	var out []string
	if o.AppendSystemPrompt != "" && (backend == "api" || backend == "claude") {
		out = append(out, "option append_system_prompt "+textHash(o.AppendSystemPrompt))
	}
	if o.Retrieval != "" && o.Retrieval != "off" {
		out = append(out, "option retrieval "+o.Retrieval+" "+o.RetrievalVersion)
	}
	if backend == "api" && o.NoInstructions {
		out = append(out, "option no_instructions")
	}
	if backend == "api" && o.ShapeTools {
		out = append(out, "option shape_tools")
	}
	if backend == "claude" && o.NoSkills {
		out = append(out, "option no_skills")
	}
	if (backend == "claude" || backend == "codex") && o.IsLean() {
		out = append(out, "option lean")
	}
	if backend == "claude" && o.IsLean() && o.Skills != nil && *o.Skills && !o.NoSkills {
		out = append(out, "option skills")
	}
	return out
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

// PromptPrefixOption hashes a brief for WithOptions.
func PromptPrefixOption(prefix string) string { return "option prompt_prefix " + textHash(prefix) }

func textHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

type setupTree struct{ root, real, prefix string }

func (t setupTree) file(rel string) string {
	if t.root == "" {
		return ""
	}
	return filepath.Join(t.root, filepath.FromSlash(rel))
}

func newSetupTree(root, prefix string) setupTree {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		real = root
	}
	return setupTree{root: root, real: real, prefix: prefix}
}

type setupScan struct {
	files                             map[string]string
	mcpNames, pluginNames, skillNames []string
	imports, truncated                bool
	// Home imports resolve outside the project tree.
	home setupTree
}

func (s *setupScan) file(t setupTree, names ...string) {
	for _, name := range names {
		s.add(t, name, 0)
	}
}

func (s *setupScan) add(t setupTree, rel string, hop int) {
	if t.root == "" {
		return
	}
	rel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	key := t.prefix + rel
	if _, seen := s.files[key]; seen {
		return
	}
	real, err := filepath.EvalSymlinks(filepath.Join(t.root, filepath.FromSlash(rel)))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if len(s.files) >= maxSetupFiles {
		s.truncated = true
		return
	}
	if err != nil {
		s.files[key] = fileUnreadable
		return
	}
	// Hash external links only for matching Markdown/JSON files to avoid confirming guessed secrets.
	if !within(t.real, real) && (filepath.Ext(real) != filepath.Ext(rel) || !slices.Contains([]string{".md", ".json"}, filepath.Ext(rel))) {
		s.files[key] = fileOutsideTree
		return
	}
	if info, err := os.Stat(real); err != nil || !info.Mode().IsRegular() {
		return
	}
	// Claude Code expands imports in instruction and rule files, not in skills.
	sum, head, err := hashFile(real, s.imports && strings.HasSuffix(rel, ".md") && filepath.Base(rel) != "SKILL.md")
	if err != nil {
		s.files[key] = fileUnreadable
		return
	}
	s.files[key] = sum
	if hop >= maxImportHops {
		return
	}
	for _, imp := range importPaths(head) {
		if home, ok := strings.CutPrefix(imp, "~/"); ok {
			home = filepath.ToSlash(filepath.Clean(filepath.FromSlash(home)))
			if strings.HasSuffix(home, ".md") && home != ".." && !strings.HasPrefix(home, "../") {
				s.add(s.home, home, hop+1)
			}
			continue
		}
		target := filepath.ToSlash(filepath.Join(filepath.Dir(filepath.FromSlash(rel)), filepath.FromSlash(imp)))
		if target == ".." || strings.HasPrefix(target, "../") || filepath.IsAbs(imp) || strings.HasPrefix(target, "~") {
			continue
		}
		s.add(t, target, hop+1)
	}
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(filepath.FromSlash(a)), filepath.Clean(filepath.FromSlash(b))
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func hashFile(path string, keepHead bool) (sum, head string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	h := sha256.New()
	var r io.Reader = f
	var buf strings.Builder
	if keepHead {
		if _, err := io.Copy(io.MultiWriter(h, &buf), io.LimitReader(f, maxImportScan)); err != nil {
			return "", "", err
		}
	}
	if _, err := io.Copy(h, r); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(h.Sum(nil)), buf.String(), nil
}

func importPaths(text string) []string {
	var out []string
	fenced := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		for _, word := range strings.Fields(line) {
			if p, ok := strings.CutPrefix(word, "@"); ok && p != "" && !strings.Contains(p, "`") {
				out = append(out, p)
			}
		}
	}
	return out
}

func (s *setupScan) mdDir(t setupTree, dir string) {
	if t.root == "" {
		return
	}
	visited := map[string]bool{}
	var walk func(real, rel string)
	walk = func(real, rel string) {
		if visited[real] {
			return
		}
		visited[real] = true
		entries, err := os.ReadDir(real)
		if err != nil {
			return
		}
		for _, e := range entries {
			if s.truncated {
				return
			}
			child := filepath.Join(real, e.Name())
			info, err := os.Stat(child)
			switch {
			case err != nil:
			case info.IsDir():
				if next, err := filepath.EvalSymlinks(child); err == nil {
					walk(next, rel+"/"+e.Name())
				}
			case strings.HasSuffix(e.Name(), ".md"):
				s.add(t, rel+"/"+e.Name(), 0)
			}
		}
	}
	if base, err := filepath.EvalSymlinks(filepath.Join(t.root, filepath.FromSlash(dir))); err == nil {
		walk(base, dir)
	}
}

func (s *setupScan) skills(t setupTree, dir string) {
	if t.root == "" {
		return
	}
	entries, err := os.ReadDir(filepath.Join(t.root, filepath.FromSlash(dir)))
	if err != nil {
		return
	}
	for _, e := range entries {
		rel := dir + "/" + e.Name() + "/SKILL.md"
		if info, err := os.Stat(filepath.Join(t.root, filepath.FromSlash(rel))); err == nil && info.Mode().IsRegular() {
			s.skillNames = append(s.skillNames, e.Name())
			s.add(t, rel, 0)
		}
	}
}

func (s *setupScan) plugins(paths ...string) {
	on := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var settings struct {
			EnabledPlugins map[string]bool `json:"enabledPlugins"`
		}
		if json.Unmarshal(data, &settings) != nil {
			continue
		}
		maps.Copy(on, settings.EnabledPlugins)
	}
	for name, enabled := range on {
		if enabled {
			s.pluginNames = append(s.pluginNames, name)
		}
	}
}

// claudeUserMCP reads only server names: entries may hold secrets.
func (s *setupScan) claudeUserMCP(claudeDir setupTree, home string, projects ...string) {
	path := ""
	switch {
	case os.Getenv("CLAUDE_CONFIG_DIR") != "":
		path = claudeDir.file(".claude.json")
	case home != "":
		path = filepath.Join(home, ".claude.json")
	}
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	type servers struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	var cfg struct {
		servers
		Projects map[string]servers `json:"projects"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return
	}
	lists := []map[string]json.RawMessage{cfg.MCPServers}
	for dir, p := range cfg.Projects {
		if slices.ContainsFunc(projects, func(project string) bool { return samePath(dir, project) }) {
			lists = append(lists, p.MCPServers)
		}
	}
	for _, list := range lists {
		for name := range list {
			s.mcpNames = append(s.mcpNames, name)
		}
	}
}

func (s *setupScan) mcpConfigs(paths []string, every bool) {
	for _, p := range paths {
		servers, skipped, err := mcp.LoadConfig(p)
		if err != nil {
			continue
		}
		for _, srv := range servers {
			s.mcpNames = append(s.mcpNames, srv.Name)
		}
		if every {
			s.mcpNames = append(s.mcpNames, skipped...)
		}
	}
}

func (s *setupScan) setup(options []string) *Setup {
	out := &Setup{Files: s.files, MCP: sortedSet(s.mcpNames), Plugins: sortedSet(s.pluginNames), Skills: sortedSet(s.skillNames), Truncated: s.truncated}
	if len(out.Files) == 0 {
		out.Files = nil
	}
	lines := []string{setupVersion}
	for _, name := range slices.Sorted(maps.Keys(s.files)) {
		lines = append(lines, "file "+name+" "+s.files[name])
	}
	for _, list := range []struct {
		kind  string
		names []string
	}{{"mcp", out.MCP}, {"plugin", out.Plugins}, {"skill", out.Skills}} {
		for _, n := range list.names {
			lines = append(lines, list.kind+" "+n)
		}
	}
	if s.truncated {
		lines = append(lines, "truncated")
	}
	out.BaseDigest = digestLines(lines)
	out.Digest = digestLines(append(lines, options...))
	return out
}

func digestLines(lines []string) string {
	h := sha256.New()
	for _, l := range lines {
		_, _ = io.WriteString(h, l+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sortedSet(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	out := slices.Clone(names)
	slices.Sort(out)
	return slices.Compact(out)
}
