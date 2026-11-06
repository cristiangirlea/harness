package archtest

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func fuzzTargets(root string) (map[string]bool, error) {
	targets := map[string]bool{}
	decl := regexp.MustCompile(`(?m)^func (Fuzz\w+)\s*\(`)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_") || d.Name() == "testdata" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		dir, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		for _, match := range decl.FindAllStringSubmatch(string(data), -1) {
			targets[filepath.ToSlash(dir)+":"+match[1]] = true
		}
		return nil
	})
	return targets, err
}

func TestEveryFuzzTargetRunsInCIAndTheCheckSkill(t *testing.T) {
	root := filepath.Join("..", "..")
	targets, err := fuzzTargets(root)
	if err != nil {
		t.Fatal(err)
	}
	workflows, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	sources := []struct {
		name  string
		paths []string
	}{
		{"workflows", workflows},
		{"check skill", []string{filepath.Join(root, ".claude", "skills", "check", "SKILL.md")}},
	}
	invocation := regexp.MustCompile(`-fuzz=(Fuzz\w+)\b[^\r\n]*?[ \t]+\./([\w./-]+)`)
	for _, source := range sources {
		run := map[string]bool{}
		for _, path := range source.paths {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, match := range invocation.FindAllStringSubmatch(string(data), -1) {
				dir := filepath.ToSlash(filepath.Clean(filepath.FromSlash(match[2])))
				run[dir+":"+match[1]] = true
			}
		}
		if differences := fuzzTargetDifferences(targets, run); len(differences) != 0 {
			t.Errorf("%s:\n%s", source.name, strings.Join(differences, "\n"))
		}
	}
}

func TestFuzzTargetWalkSkipsDirectoriesIgnoredByGo(t *testing.T) {
	root := filepath.Join(t.TempDir(), "_root")
	for _, dir := range []string{".claude/worktrees/example", ".cache", "_scratch", "testdata", "vendor", "internal/example"} {
		path := filepath.Join(root, filepath.FromSlash(dir), "fuzz_test.go")
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package example\nfunc FuzzExample(f *testing.F) {}\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	targets, err := fuzzTargets(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || !targets["internal/example:FuzzExample"] {
		t.Fatalf("got targets %v, want only internal/example:FuzzExample", targets)
	}
}

func fuzzTargetDifferences(targets, run map[string]bool) []string {
	var differences []string
	for target := range targets {
		if !run[target] {
			differences = append(differences, "missing invocation: "+target)
		}
	}
	for target := range run {
		if !targets[target] {
			differences = append(differences, "unknown target: "+target)
		}
	}
	slices.Sort(differences)
	return differences
}

func TestFuzzTargetDifferencesListBothDirections(t *testing.T) {
	cases := []struct {
		name string
		run  map[string]bool
		want []string
	}{
		{"matching", map[string]bool{"internal/example:FuzzOne": true}, nil},
		{"missing", map[string]bool{}, []string{"missing invocation: internal/example:FuzzOne"}},
		{"extra", map[string]bool{"internal/example:FuzzOne": true, "internal/other:FuzzOne": true}, []string{"unknown target: internal/other:FuzzOne"}},
		{"renamed", map[string]bool{"internal/example:FuzzTwo": true}, []string{"missing invocation: internal/example:FuzzOne", "unknown target: internal/example:FuzzTwo"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fuzzTargetDifferences(map[string]bool{"internal/example:FuzzOne": true}, tc.run)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWorkflowActionsArePinnedToCommits(t *testing.T) {
	root := filepath.Join("..", "..")
	paths, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	usesCount := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			uses, pinned := workflowActionPin(line)
			if !uses {
				continue
			}
			usesCount++
			if !pinned {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					t.Fatal(err)
				}
				offenders = append(offenders, fmt.Sprintf("%s:%d: action needs a commit hash and version comment", filepath.ToSlash(rel), i+1))
			}
		}
	}
	if usesCount == 0 {
		t.Error("workflows contain no uses: lines")
	}
	if len(offenders) != 0 {
		t.Fatal(strings.Join(offenders, "\n"))
	}
}

func workflowActionPin(line string) (uses, pinned bool) {
	match := regexp.MustCompile(`^\s*(?:-\s+)?uses:\s*(.*?)\s*$`).FindStringSubmatch(line)
	if match == nil {
		return false, false
	}
	action, version, _ := strings.Cut(match[1], " # ")
	value := strings.Trim(action, `"'`)
	if strings.HasPrefix(value, "./") || strings.HasPrefix(value, "docker://") {
		return true, true
	}
	pin := regexp.MustCompile(`^[^\s@]+/[^\s@]+@[0-9a-f]{40} # v[0-9]+\.[0-9]+\.[0-9]+$`)
	return true, pin.MatchString(value + " # " + version)
}

func TestWorkflowActionLinesRequireACommitAndVersion(t *testing.T) {
	action := "actions/checkout@" + strings.Repeat("a", 40)
	cases := []struct {
		name   string
		line   string
		uses   bool
		pinned bool
	}{
		{"tag only", "uses: actions/checkout@v4", true, false},
		{"quoted value", "uses: \"" + action + "\" # v4.4.0", true, true},
		{"single quoted value", "uses: '" + action + "' # v4.4.0", true, true},
		{"list item", "  - uses: " + action + " # v4.4.0", true, true},
		{"name on its own line", "  - name: checkout", false, false},
		{"uses under name", "    uses: " + action + " # v4.4.0", true, true},
		{"local action", "uses: ./local/action", true, true},
		{"docker action", "uses: docker://alpine:3", true, true},
		{"short sha", "uses: actions/checkout@abcdef0 # v4.4.0", true, false},
		{"missing version", "uses: " + action, true, false},
		{"malformed version", "uses: " + action + " # v4.4", true, false},
		{"correct pin", "uses: " + action + " # v4.4.0", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uses, pinned := workflowActionPin(tc.line)
			if uses != tc.uses || pinned != tc.pinned {
				t.Fatalf("got uses=%t pinned=%t, want uses=%t pinned=%t", uses, pinned, tc.uses, tc.pinned)
			}
		})
	}
}
