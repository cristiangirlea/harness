package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, ".hrn": true, "dist": true, "build": true, "__pycache__": true, ".venv": true}

func str(v string) map[string]any {
	if v == "" {
		return map[string]any{"type": "string"}
	}
	return map[string]any{"type": "string", "description": v}
}
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
		Description: "Read a workspace text file; lines have 1-based numbers and tabs. Secret files may be refused.",
		Properties: map[string]any{
			"path":   str(""),
			"offset": num("1-based start line (default 1)."),
			"limit":  num("Maximum lines (default 2000)."),
		},
		Required: []string{"path"},
		Effect:   EffectRead,
		Summary:  summaryOf("path"),
		Refuse:   refuseSecretFile,
		Run: func(ctx context.Context, ws *Workspace, raw json.RawMessage) (string, error) {
			args, err := decode[in](raw)
			if err != nil {
				return "", err
			}
			if rule := refuseSecretFile(ws, raw); rule != "" {
				return "", GuardError(rule)
			}
			p, err := ws.Resolve(args.Path)
			if err != nil {
				return "", err
			}
			info, err := os.Stat(p)
			if err != nil {
				return "", err
			}
			if !info.Mode().IsRegular() {
				return "", fmt.Errorf("%q is not a regular file", ws.Rel(p))
			}
			f, err := os.OpenFile(p, os.O_RDONLY|readOpenFlags, 0)
			if err != nil {
				return "", err
			}
			defer f.Close()
			info, err = f.Stat()
			if err != nil {
				return "", err
			}
			if !info.Mode().IsRegular() {
				return "", fmt.Errorf("%q is not a regular file", ws.Rel(p))
			}
			if args.Offset < 1 {
				args.Offset = 1
			}
			if args.Limit <= 0 {
				args.Limit = 2000
			}
			b := &boundedBuffer{keepHead: maxOutput * 2 / 3, keepTail: maxOutput / 3}
			r := bufio.NewReader(f)
			lineStart, pendingCR := true, false
			count, end := 0, 0
			for {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				part, err := r.ReadSlice('\n')
				if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
					return "", err
				}
				selected := count >= args.Offset-1 && count-(args.Offset-1) < args.Limit
				if selected {
					if lineStart {
						fmt.Fprintf(b, "%d\t", count+1)
					}
					if pendingCR && (len(part) == 0 || part[0] != '\n') {
						b.Write([]byte{'\r'})
					}
					pendingCR = false
					if err == nil {
						part = part[:len(part)-1]
						if len(part) > 0 && part[len(part)-1] == '\r' {
							part = part[:len(part)-1]
						}
					} else if err == bufio.ErrBufferFull && part[len(part)-1] == '\r' {
						pendingCR = true
						part = part[:len(part)-1]
					}
					b.Write(part)
				}
				if err == bufio.ErrBufferFull {
					lineStart = false
					continue
				}
				lineStart = true
				count++
				if selected {
					b.Write([]byte{'\n'})
					end = count
				}
				if err == io.EOF {
					break
				}
			}
			if args.Offset > count {
				return fmt.Sprintf("(file has %d lines)", count), nil
			}
			if end < count {
				fmt.Fprintf(b, "(%d more lines; continue with offset=%d)\n", count-end, end+1)
			}
			return b.String(), nil
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
		Description: "Create or overwrite a workspace file, creating parents; secret files may be refused. Prefer edit_file for existing files.",
		Properties: map[string]any{
			"path":    str(""),
			"content": str(""),
		},
		Required: []string{"path", "content"},
		Effect:   EffectWrite,
		Summary:  summaryOf("path"),
		Refuse:   refuseSecretFile,
		Run: func(ctx context.Context, ws *Workspace, raw json.RawMessage) (string, error) {
			args, err := decode[in](raw)
			if err != nil {
				return "", err
			}
			if rule := refuseSecretFile(ws, raw); rule != "" {
				return "", GuardError(rule)
			}
			p, err := ws.Resolve(args.Path)
			if err != nil {
				return "", err
			}
			if err := ws.checkParse(p, []byte(args.Content)); err != nil {
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
		Description: "Replace old_string with new_string in a workspace file; old_string must be nonempty and match uniquely, including whitespace, unless replace_all is true. Secret files may be refused.",
		Properties: map[string]any{
			"path":        str(""),
			"old_string":  str(""),
			"new_string":  str(""),
			"replace_all": map[string]any{"type": "boolean"},
		},
		Required: []string{"path", "old_string", "new_string"},
		Effect:   EffectWrite,
		Summary:  summaryOf("path"),
		Refuse:   refuseSecretFile,
		Run: func(ctx context.Context, ws *Workspace, raw json.RawMessage) (string, error) {
			args, err := decode[in](raw)
			if err != nil {
				return "", err
			}
			if rule := refuseSecretFile(ws, raw); rule != "" {
				return "", GuardError(rule)
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
			// Match model LF text against CRLF files.
			if !strings.Contains(content, old) && strings.Contains(content, "\r\n") {
				old = strings.ReplaceAll(old, "\n", "\r\n")
			}
			if strings.Contains(content, "\r\n") && (old != args.OldString || !strings.Contains(strings.ReplaceAll(content, "\r\n", ""), "\n")) {
				args.NewString = strings.ReplaceAll(strings.ReplaceAll(args.NewString, "\r\n", "\n"), "\n", "\r\n")
			}
			n := strings.Count(content, old)
			switch {
			case n == 0:
				return "", errors.New(closestLinesForMatch(content, args.OldString, old))
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
			if err := ws.checkParse(p, []byte(content)); err != nil {
				return "", err
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
		Description: "List up to 500 paths; ** matches any depth, skipping .git, node_modules and similar directories.",
		Properties: map[string]any{
			"pattern": str("Glob (default \"**/*\")."),
			"path":    str("Search directory (default workspace root)."),
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
			err = walk(ctx, ws, base, func(p, rel string, _ bool) bool {
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
		Description: "Search file contents with Go RE2; returns up to 200 path:line:text matches and reports read errors or overlong lines. Secret files may be excluded.",
		Properties: map[string]any{
			"pattern":          str(""),
			"path":             str("Search path (default workspace root)."),
			"glob":             str("Filter paths by glob."),
			"case_insensitive": map[string]any{"type": "boolean"},
		},
		Required: []string{"pattern"},
		Effect:   EffectRead,
		Summary:  summaryOf("pattern"),
		Refuse:   refuseSecretFile,
		Run: func(ctx context.Context, ws *Workspace, raw json.RawMessage) (string, error) {
			args, err := decode[in](raw)
			if err != nil {
				return "", err
			}
			if rule := refuseSecretFile(ws, raw); rule != "" {
				return "", GuardError(rule)
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
				for count < limit && sc.Scan() {
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
				if err := sc.Err(); err != nil {
					fmt.Fprintf(&b, "(%s: search cut short: %v)\n", ws.Rel(p), err)
				}
			}
			if info, err := os.Stat(base); err == nil && !info.IsDir() {
				search(base)
			} else {
				// Link-free walks reuse realBase to expose 8.3 secrets.
				realBase := realPath(base)
				if realBase == "" {
					// Resolve accepted it a moment ago, so it changed since.
					return "", fmt.Errorf("path %q is refused: hrn cannot tell where it leads", args.Path)
				}
				err = walk(ctx, ws, base, func(p, rel string, regular bool) bool {
					if globRe != nil && !globRe.MatchString(rel) {
						return true
					}
					// Skip escapes/secrets, not the whole search.
					real := filepath.Join(realBase, filepath.FromSlash(rel))
					if !regular {
						real = realPath(p)
						if !ws.inside(real) {
							return true
						}
					}
					if !ws.AllowDangerous && (SecretFile(ws.Rel(p)) || ws.secretReal(real)) {
						return true
					}
					search(p)
					return count < limit
				})
				if err != nil {
					return "", err
				}
			}
			if count == 0 && b.Len() == 0 {
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

// WalkDir skips links/junctions; visit Root's original spelling.
func walk(ctx context.Context, ws *Workspace, base string, visit func(path, rel string, regular bool) bool) error {
	walkBase := base
	if base == ws.Root {
		walkBase = ws.real
	}
	err := filepath.WalkDir(walkBase, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if p != walkBase && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(walkBase, p)
		if !visit(filepath.Join(base, rel), filepath.ToSlash(rel), d.Type().IsRegular()) {
			return errStop
		}
		return nil
	})
	if errors.Is(err, errStop) {
		return nil
	}
	return err
}

// GlobRegexp compiles anchored globs supporting **.
func GlobRegexp(glob string) (*regexp.Regexp, error) {
	glob = filepath.ToSlash(glob)
	matched := make(map[int]bool)
	var stack []int
	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '{':
			stack = append(stack, i)
		case '}':
			if len(stack) > 0 {
				matched[stack[len(stack)-1]] = true
				matched[i] = true
				stack = stack[:len(stack)-1]
			}
		}
	}
	var b strings.Builder
	b.WriteString("^")
	depth := 0
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
			if matched[i] {
				b.WriteString("(?:")
				depth++
			} else {
				b.WriteString(`\{`)
			}
		case '}':
			if matched[i] {
				b.WriteString(")")
				depth--
			} else {
				b.WriteString(`\}`)
			}
		case ',':
			if depth > 0 {
				b.WriteString("|")
			} else {
				b.WriteString(",")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
