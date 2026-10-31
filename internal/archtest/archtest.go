package archtest

import (
	"bufio"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func display(p string) string {
	if strings.HasPrefix(p, "internal/") {
		return strings.TrimPrefix(p, "internal/")
	}
	return p
}
func check(root string, baseline io.Reader) error {
	modules := map[string]string{}
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_") || d.Name() == "testdata" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if d.Name() == "go.mod" {
			b, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			for _, line := range strings.Split(string(b), "\n") {
				f := strings.Fields(line)
				if len(f) == 2 && f[0] == "module" {
					dir := filepath.ToSlash(filepath.Dir(rel))
					if dir == "." {
						dir = ""
					}
					modules[dir] = f[1]
					break
				}
			}
			if modules[filepath.ToSlash(filepath.Dir(rel))] == "" && rel != "go.mod" {
				return fmt.Errorf("invalid module: %s", rel)
			}
		}
		if strings.HasSuffix(d.Name(), ".go") {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	rootModule := modules[""]
	if rootModule == "" {
		return fmt.Errorf("missing root module")
	}
	edges := map[string]bool{}
	for _, file := range files {
		dir := filepath.ToSlash(filepath.Dir(file))
		if dir == "." {
			dir = ""
		}
		if _, ok := layer(dir); !ok {
			return fmt.Errorf("unclassified package: %s", dir)
		}
		f, e := parser.ParseFile(token.NewFileSet(), filepath.Join(root, filepath.FromSlash(file)), nil, parser.ImportsOnly)
		if e != nil {
			return e
		}
		for _, imp := range f.Imports {
			path, e := strconv.Unquote(imp.Path.Value)
			if e != nil {
				return e
			}
			to := ""
			best := -1
			for moddir, mod := range modules {
				if (path == mod || strings.HasPrefix(path, mod+"/")) && len(mod) > best {
					suffix := strings.TrimPrefix(strings.TrimPrefix(path, mod), "/")
					to = strings.Trim(strings.Join([]string{moddir, suffix}, "/"), "/")
					best = len(mod)
				}
			}
			if best < 0 {
				continue
			}
			if _, ok := layer(to); !ok {
				return fmt.Errorf("unclassified import: %s", to)
			}
			if allowed(dir, to) {
				continue
			}
			target := display(to)
			if strings.HasPrefix(dir, "server/") && !strings.HasPrefix(to, "server/") {
				target = to
			}
			edge := display(dir) + " -> " + target
			if strings.HasSuffix(file, "_test.go") {
				edge += " (test)"
			}
			edges[edge] = true
		}
	}
	expected := map[string]bool{}
	s := bufio.NewScanner(baseline)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		if expected[line] {
			return fmt.Errorf("duplicate baseline: %s", line)
		}
		expected[line] = true
	}
	if err := s.Err(); err != nil {
		return err
	}
	var problems []string
	for e := range edges {
		if !expected[e] {
			problems = append(problems, "missing baseline: "+e)
		}
	}
	for e := range expected {
		if !edges[e] {
			problems = append(problems, "stale baseline: "+e)
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "\n"))
	}
	return nil
}
