package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// spawnExempt names the functions whose one child keeps its window, or never runs on Windows.
var spawnExempt = []string{
	"cmd/hrn/bot.go:openWindow",
	"internal/update/process_darwin.go:systemProcesses",
}

// unhiddenSpawns lists uses of exec.Command in the hrn binary's code that are not followed by nowindow.Set.
func unhiddenSpawns(root string, exempt []string) ([]string, error) {
	used := map[string][]string{}
	var problems []string
	for _, top := range []string{"cmd/hrn", "internal"} {
		dir := filepath.Join(root, filepath.FromSlash(top))
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_") || d.Name() == "testdata" || d.Name() == "vendor" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			found, err := fileSpawns(path, filepath.ToSlash(rel), exempt, used)
			problems = append(problems, found...)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	for _, e := range exempt {
		switch lines := used[e]; {
		case len(lines) == 0:
			problems = append(problems, "stale exemption: "+e)
		case len(lines) > 1:
			problems = append(problems, fmt.Sprintf("exemption %s covers %d spawn sites, want one: lines %s", e, len(lines), strings.Join(lines, ", ")))
		}
	}
	sort.Strings(problems)
	return problems, nil
}

func fileSpawns(path, rel string, exempt []string, used map[string][]string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	execName, hideName := "", ""
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, err
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch {
		case p == "os/exec" && name == ".":
			return []string{fmt.Sprintf("%s:%d: os/exec is dot-imported, which hides its spawn sites from this check", rel, fset.Position(imp.Pos()).Line)}, nil
		case p == "os/exec":
			execName = name
		case strings.HasSuffix(p, "/internal/nowindow"):
			hideName = name
		}
	}
	if execName == "" || execName == "_" {
		return nil, nil
	}
	var problems []string
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		if !isSelector(n, execName, "Command", "CommandContext") {
			return true
		}
		decl := ""
		for _, node := range stack {
			if fn, ok := node.(*ast.FuncDecl); ok {
				decl = fn.Name.Name
			}
		}
		if key := rel + ":" + decl; slices.Contains(exempt, key) {
			used[key] = append(used[key], strconv.Itoa(fset.Position(n.Pos()).Line))
			return true
		}
		if hideName == "" || !hidden(stack, hideName) {
			problems = append(problems, fmt.Sprintf("%s:%d: the command created in %s is not passed to nowindow.Set before it is used", rel, fset.Position(n.Pos()).Line, decl))
		}
		return true
	})
	return problems, nil
}

// hidden accepts one shape only: `x := exec.Command(...)` as a statement, then `nowindow.Set(x)` in the same list before x is used.
func hidden(stack []ast.Node, hideName string) bool {
	if len(stack) < 4 {
		return false
	}
	call, ok := stack[len(stack)-2].(*ast.CallExpr)
	if !ok || call.Fun != stack[len(stack)-1] {
		return false
	}
	assign, ok := stack[len(stack)-3].(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return false
	}
	id, ok := assign.Lhs[0].(*ast.Ident)
	if !ok {
		return false
	}
	var list []ast.Stmt
	switch parent := stack[len(stack)-4].(type) {
	case *ast.BlockStmt:
		list = parent.List
	case *ast.CaseClause:
		list = parent.Body
	case *ast.CommClause:
		list = parent.Body
	}
	at := slices.Index(list, ast.Stmt(assign))
	if at < 0 {
		return false
	}
	for _, stmt := range list[at+1:] {
		if expr, ok := stmt.(*ast.ExprStmt); ok {
			if set, ok := expr.X.(*ast.CallExpr); ok && isSelector(set.Fun, hideName, "Set") && len(set.Args) == 1 && isIdent(set.Args[0], id.Name) {
				return true
			}
		}
		if uses(stmt, id.Name) {
			return false
		}
	}
	return false
}

// uses reports a statement that names the command other than to reach one of its fields or non-starting methods.
func uses(stmt ast.Node, name string) bool {
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if isIdent(n.X, name) {
				found = found || slices.Contains([]string{"Run", "Start", "Output", "CombinedOutput"}, n.Sel.Name)
			} else {
				found = found || uses(n.X, name)
			}
			return false
		case *ast.Ident:
			found = found || n.Name == name
		}
		return !found
	})
	return found
}

func isSelector(n ast.Node, pkg string, names ...string) bool {
	sel, ok := n.(*ast.SelectorExpr)
	return ok && isIdent(sel.X, pkg) && slices.Contains(names, sel.Sel.Name)
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}
