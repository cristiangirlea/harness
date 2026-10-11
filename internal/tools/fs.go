package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, ".hrn": true, "dist": true, "build": true, "__pycache__": true, ".venv": true}

func str(v string) map[string]any { return map[string]any{"type": "string", "description": v} }
func num(v string) map[string]any { return map[string]any{"type": "integer", "description": v} }

func summaryOf(field string) func(json.RawMessage) string {
	return func(in json.RawMessage) string {
		var m map[string]any
		_ = json.Unmarshal(in, &m)
		if v, ok := m[field].(string); ok {
			return v
		}
		return ""
	}
}

func readFile() Tool {
	type in struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	return Tool{
		Name:        "read_file",
		Description: "Read a text file. Returns lines prefixed with their 1-based line number and a tab. Use offset/limit for large files.",
		Properties: map[string]any{
			"path":   str("File path, relative to the workspace root."),
			"offset": num("1-based line to start from (default 1)."),
			"limit":  num("Maximum lines to return (default 2000)."),
		},
		Required: []string{"path"},
		Effect:   EffectRead,
		Summary:  summaryOf("path"),
		Run: func(ctx context.Context, ws *Workspace, raw json.RawMessage) (string, error) {
			args, err := decode[in](raw)
			if err != nil {
				return "", err
			}
			p, err := ws.Resolve(args.Path)
			if err != nil {
				return "", err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return "", err
			}
			if args.Offset < 1 {
				args.Offset = 1
			}
			if args.Limit <= 0 {
				args.Limit = 2000
			}
			lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
			if args.Offset > len(lines) {
				return fmt.Sprintf("(file has %d lines)", len(lines)), nil
			}
			end := min(args.Offset-1+args.Limit, len(lines))
			var b strings.Builder
			for i := args.Offset - 1; i < end; i++ {
				fmt.Fprintf(&b, "%d\t%s\n", i+1, lines[i])
			}
			if end < len(lines) {
				fmt.Fprintf(&b, "(%d more lines; continue with offset=%d)\n", len(lines)-end, end+1)
			}
			return Truncate(b.String()), nil
		},
	}
}

func writeFile() Tool {
	type in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	return Tool{
		Name:        "write_file",
		Description: "Create or overwrite a file with the given content. Parent directories are created. Prefer edit_file for changes to existing files.",
		Properties: map[string]any{
			"path":    str("File path, relative to the workspace root."),
			"content": str("Full file content."),
		},
		Required: []string{"path", "content"},
		Effect:   EffectWrite,
		Summary:  summaryOf("path"),
		Run: func(ctx context.Context, ws *Workspace, raw json.RawMessage) (string, error) {
			args, err := decode[in](raw)
			if err != nil {
				return "", err
			}
			p, err := ws.Resolve(args.Path)
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(p, []byte(args.Content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %s (%d bytes)", ws.Rel(p), len(args.Content)), nil
		},
	}
}

func editFile() Tool {
	type in struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	return Tool{
		Name:        "edit_file",
		Description: "Replace an exact string in a file. old_string must match exactly (including whitespace) and be unique unless replace_all is true.",
		Properties: map[string]any{
			"path":        str("File path, relative to the workspace root."),
			"old_string":  str("Exact text to replace."),
			"new_string":  str("Replacement text."),
			"replace_all": map[string]any{"type": "boolean", "description": "Replace every occurrence."},
		},
		Required: []string{"path", "old_string", "new_string"},
		Effect:   EffectWrite,
		Summary:  summaryOf("path"),
		Run: func(ctx context.Context, ws *Workspace, raw json.RawMessage) (string, error) {
			args, err := decode[in](raw)
			if err != nil {
				return "", err
			}
			if args.OldString == "" {
				return "", errors.New("old_string must not be empty; use write_file to create files")
			}
			if args.OldString == args.NewString {
				return "", errors.New("old_string and new_string are identical")
			}
			p, err := ws.Resolve(args.Path)
			if err != nil {
				return "", err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return "", err
			}
			content := string(data)
			old := args.OldString
			// Files with CRLF endings still match LF-only old_string from the model.
			if !strings.Contains(content, old) && strings.Contains(content, "\r\n") {
				old = strings.ReplaceAll(old, "\n", "\r\n")
				args.NewString = strings.ReplaceAll(args.NewString, "\n", "\r\n")
			}
			n := strings.Count(content, old)
			switch {
			case n == 0:
				return "", errors.New("old_string not found in file")
			case n > 1 && !args.ReplaceAll:
				return "", fmt.Errorf("old_string matches %d times; add more context or set replace_all", n)
			}
			replaced := 1
			if args.ReplaceAll {
				content = strings.ReplaceAll(content, old, args.NewString)
				replaced = n
			} else {
				content = strings.Replace(content, old, args.NewString, 1)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("edited %s (%d replacement(s))", ws.Rel(p), replaced), nil
		},
	}
}

func listFiles() Tool {
	type in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	return Tool{
		Name:        "list_files",
		Description: "List files matching a glob pattern (supports ** for any depth, e.g. \"**/*.go\"). Skips .git, node_modules and similar directories. Returns at most 500 paths.",
		Properties: map[string]any{
			"pattern": str("Glob pattern, default \"**/*\"."),
			"path":    str("Directory to search from, default the workspace root."),
		},
		Effect:  EffectRead,
		Summary: summaryOf("pattern"),
		Run: func(ctx context.Context, ws *Workspace, raw json.RawMessage) (string, error) {
			args, err := decode[in](raw)
			if err != nil {
				return "", err
			}
			if args.Pattern == "" {
				args.Pattern = "**/*"
			}
			base, err := ws.Resolve(args.Path)
			if err != nil {
				return "", err
			}
			re, err := GlobRegexp(args.Pattern)
			if err != nil {
				return "", err
			}
			var out []string
			const limit = 500
			err = walk(ctx, base, func(p, rel string, _ bool) bool {
				if re.MatchString(rel) {
					out = append(out, ws.Rel(p))
				}
				return len(out) < limit
			})
			if err != nil {
				return "", err
			}
			if len(out) == 0 {
				return "(no matches)", nil
			}
			s := strings.Join(out, "\n")
			if len(out) == limit {
				s += "\n(limit reached; narrow the pattern)"
			}
			return s, nil
		},
	}
}

func grep() Tool {
	type in struct {
		Pattern         string `json:"pattern"`
		Path            string `json:"path"`
		Glob            string `json:"glob"`
		CaseInsensitive bool   `json:"case_insensitive"`
	}
	return Tool{
		Name:        "grep",
		Description: "Search file contents with a regular expression (Go RE2 syntax). Returns path:line:text, at most 200 matches.",
		Properties: map[string]any{
			"pattern":          str("Regular expression."),
			"path":             str("File or directory to search, default the workspace root."),
			"glob":             str("Only search files whose path matches this glob, e.g. \"**/*.ts\"."),
			"case_insensitive": map[string]any{"type": "boolean"},
		},
		Required: []string{"pattern"},
		Effect:   EffectRead,
		Summary:  summaryOf("pattern"),
		Run: func(ctx context.Context, ws *Workspace, raw json.RawMessage) (string, error) {
			args, err := decode[in](raw)
			if err != nil {
				return "", err
			}
			expr := args.Pattern
			if args.CaseInsensitive {
				expr = "(?i)" + expr
			}
			re, err := regexp.Compile(expr)
			if err != nil {
				return "", err
			}
			var globRe *regexp.Regexp
			if args.Glob != "" {
				if globRe, err = GlobRegexp(args.Glob); err != nil {
					return "", err
				}
			}
			base, err := ws.Resolve(args.Path)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			const limit = 200
			count := 0
			search := func(p string) {
				f, err := os.Open(p)
				if err != nil {
					return
				}
				defer f.Close()
				sc := bufio.NewScanner(f)
				sc.Buffer(make([]byte, 64*1024), 1024*1024)
				line := 0
				for sc.Scan() && count < limit {
					line++
					text := sc.Text()
					if line == 1 && strings.ContainsRune(text, 0) {
						return // binary
					}
					if re.MatchString(text) {
						if len(text) > 300 {
							text = text[:300] + "…"
						}
						fmt.Fprintf(&b, "%s:%d:%s\n", ws.Rel(p), line, text)
						count++
					}
				}
			}
			if info, err := os.Stat(base); err == nil && !info.IsDir() {
				search(base)
			} else {
				err = walk(ctx, base, func(p, rel string, regular bool) bool {
					if globRe != nil && !globRe.MatchString(rel) {
						return true
					}
					// A link out of the workspace is not followed, as Resolve
					// refuses one. A regular file needs no judging: the walk
					// goes into no link, and base is inside.
					if !regular && !ws.inside(realPath(p)) {
						return true
					}
					search(p)
					return count < limit
				})
				if err != nil {
					return "", err
				}
			}
			if count == 0 {
				return "(no matches)", nil
			}
			if count == limit {
				b.WriteString("(limit reached; narrow the search)\n")
			}
			return b.String(), nil
		},
	}
}

var errStop = errors.New("stop")

// walk visits what is not a directory under base, passing the slash-separated
// path relative to base and whether it is a regular file rather than a link,
// a junction or another irregular entry. It goes into no link or junction
// (filepath.WalkDir does not). visit returns false to stop.
func walk(ctx context.Context, base string, visit func(path, rel string, regular bool) bool) error {
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if p != base && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(base, p)
		if !visit(p, filepath.ToSlash(rel), d.Type().IsRegular()) {
			return errStop
		}
		return nil
	})
	if errors.Is(err, errStop) {
		return nil
	}
	return err
}

// GlobRegexp compiles a glob with ** support into an anchored regexp.
func GlobRegexp(glob string) (*regexp.Regexp, error) {
	glob = filepath.ToSlash(glob)
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '{':
			b.WriteString("(?:")
		case '}':
			b.WriteString(")")
		case ',':
			b.WriteString("|")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
