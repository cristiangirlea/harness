package watch

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// A run's text names its workspace, the user's home and the programs it ran
// by their full paths, again and again. The page shows them short: the
// workspace as ".", the home as "~", and a program in a system folder by its
// file name. Only the page: records and hrn show keep the full paths.

// pathRoot is one path the shortener looks for.
type pathRoot struct {
	elems   []string // the path's elements; a Unix path's first is ""
	windows bool     // either slash, a JSON-escaped backslash, any case
	to      string   // "." or "~"; "" for a system folder, whose programs become their file names
}

type shortener struct{ roots []pathRoot }

// newShortener looks for workspace, home and the system folders, in either
// path style; a path with a drive letter or a backslash is a Windows one. A
// root folder ("/", C:\) or an empty path is left out.
func newShortener(workspace, home string, system []string) shortener {
	var sh shortener
	sh.add(workspace, ".")
	sh.add(home, "~")
	for _, s := range system {
		sh.add(s, "")
	}
	// The longest first: a workspace inside home is the workspace.
	sort.SliceStable(sh.roots, func(i, j int) bool { return len(sh.roots[i].elems) > len(sh.roots[j].elems) })
	return sh
}

func (sh *shortener) add(path, to string) {
	windows := strings.Contains(path, `\`) || len(path) >= 2 && path[1] == ':'
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' || windows && r == '\\' })
	switch {
	case windows && (len(parts) == 0 || len(parts[0]) != 2 || parts[0][1] != ':'):
		return
	case !windows && !strings.HasPrefix(path, "/"):
		return
	case !windows:
		parts = append([]string{""}, parts...)
	}
	if len(parts) < 2 {
		return
	}
	sh.roots = append(sh.roots, pathRoot{elems: parts, windows: windows, to: to})
}

// systemFolders are where the programs an agent runs by full path live.
func systemFolders() []string {
	env := func(name, def string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	}
	return []string{env("SystemRoot", `C:\Windows`) + `\System32`, env("ProgramFiles", `C:\Program Files`),
		env("ProgramFiles(x86)", `C:\Program Files (x86)`), "/usr/bin", "/bin", "/usr/local/bin"}
}

// shorten rewrites the paths in s. A root counts only at a path boundary:
// E:\ws-old is not E:\ws, and neither is D:\copy\E:\ws.
func (sh shortener) shorten(s string) string {
	if len(sh.roots) == 0 || s == "" {
		return s
	}
	var b strings.Builder
	done := 0 // s[:done] is in b
	for i := 0; i < len(s); i++ {
		if i > 0 {
			if r, _ := utf8.DecodeLastRuneInString(s[:i]); nameRune(r) || r == '/' || r == '\\' {
				continue
			}
		}
		for _, root := range sh.roots {
			j := root.match(s, i)
			if j < 0 {
				continue
			}
			if root.to != "" {
				if j < len(s) && !boundaryAt(s, j) {
					continue
				}
				b.WriteString(s[done:i])
				b.WriteString(root.to)
				done, i = j, j-1
				break
			}
			if sepAt(s, j, root.windows) == 0 {
				continue
			}
			end, name := program(s, i, j, root.windows)
			if name == "" {
				continue
			}
			b.WriteString(s[done:i])
			b.WriteString(name)
			done, i = end, end-1
			break
		}
	}
	if done == 0 {
		return s
	}
	b.WriteString(s[done:])
	return b.String()
}

// match is where root ends in s when it starts at i, or -1.
func (r pathRoot) match(s string, i int) int {
	for k, e := range r.elems {
		if k > 0 {
			n := sepAt(s, i, r.windows)
			if n == 0 {
				return -1
			}
			i += n
		}
		if len(s)-i < len(e) {
			return -1
		}
		if got := s[i : i+len(e)]; got != e && !(r.windows && strings.EqualFold(got, e)) {
			return -1
		}
		i += len(e)
	}
	return i
}

// sepAt is the length of the path separator at s[i], 0 for none.
func sepAt(s string, i int, windows bool) int {
	switch {
	case i >= len(s):
		return 0
	case windows && strings.HasPrefix(s[i:], `\\`): // as JSON holds it
		return 2
	case s[i] == '/' || windows && s[i] == '\\':
		return 1
	}
	return 0
}

// boundaryAt: a path ends before s[i], with a separator or a character no
// file name continues with.
func boundaryAt(s string, i int) bool {
	r, _ := utf8.DecodeRuneInString(s[i:])
	return r == '/' || r == '\\' || !nameRune(r)
}

func nameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_-.~$", r)
}

// program reads the path that starts at s[i] and continues past a system
// folder at s[j], up to a quote, a space or a delimiter, and returns where it
// ends and the program's file name; "" when it does not name a program. In
// quotes a Windows folder may have spaces in its name (Microsoft VS Code) and
// punctuation (Git (Preview)), so only the matching quote, a backtick or a
// line end stops it; a Windows program has an extension, which tells it from a folder.
func program(s string, i, j int, windows bool) (int, string) {
	var quote byte
	stops := "\"'`\r\n,;|&<>()[]{}"
	if i > 0 && (s[i-1] == '"' || s[i-1] == '\'') {
		quote = s[i-1]
		stops = string(quote) + "`\r\n"
	}
	end, last := j, j // last: where the current element starts
	for ; end < len(s); end++ {
		c := s[end]
		if c == '/' || windows && c == '\\' {
			last = end + 1
			continue
		}
		if c == ' ' || c == '\t' {
			if quote != 0 && windows && !strings.Contains(s[last:end], ".") {
				continue
			}
			break
		}
		if strings.IndexByte(stops, c) >= 0 {
			break
		}
	}
	for end > j && s[end-1] == '\\' { // a JSON-escaped closing quote's backslash
		end--
	}
	name := s[j:end]
	if k := strings.LastIndexAny(name, `/\`); k >= 0 && (windows || name[k] == '/') {
		name = name[k+1:]
	}
	if name == "" || windows && !strings.Contains(name, ".") || strings.ContainsAny(name, `/\`) {
		return 0, ""
	}
	return end, name
}

// viewer is who a page is for: whether they sit at this computer, and the
// paths their pages are shortened by.
type viewer struct {
	local  bool
	home   string
	system []string
}

func (v viewer) shortener(workspace string) shortener {
	return newShortener(workspace, v.home, v.system)
}

// place is a path as a page's fact shows it: by name, with the whole path as
// the hover title for this computer only, as it names the user's account.
func (v viewer) place(label, name, full string) fact {
	f := fact{Label: label, Value: name}
	if v.local && full != name {
		f.Title = full
	}
	return f
}

// The shortening runs after secrets are blanked (cleanEvent, cleanRecord,
// cleanBatch), on what a record says, before any cut.

func (sh shortener) event(e agent.Event) agent.Event {
	e.Text = sh.shorten(e.Text)
	if len(e.Input) > 0 {
		e.Input = json.RawMessage(sh.shorten(string(e.Input)))
	}
	return e
}

// record shortens what a run's page shows from its record; Task.Dir stays
// whole, for the Workspace fact.
func (sh shortener) record(rec runlog.Record) runlog.Record {
	rec.Task.Prompt = sh.shorten(rec.Task.Prompt)
	rec.Result.Final = sh.shorten(rec.Result.Final)
	rec.Result.Error = sh.shorten(rec.Result.Error)
	return rec
}

// batch shortens what a batch's page shows: each task's text by its own
// worktree (the repository when it has none). Dir and Worktree stay whole,
// for the facts.
func (v viewer) batch(res batch.Result) batch.Result {
	tasks := make([]batch.TaskResult, len(res.Tasks))
	for i, t := range res.Tasks {
		ws := t.Worktree
		if ws == "" {
			ws = res.Dir
		}
		sh := v.shortener(ws)
		t.Prompt, t.Error = sh.shorten(t.Prompt), sh.shorten(t.Error)
		attempts := make([]batch.Attempt, len(t.Attempts))
		for j, a := range t.Attempts {
			a.Prompt, a.Error = sh.shorten(a.Prompt), sh.shorten(a.Error)
			a.Result.Final, a.Result.Error = sh.shorten(a.Result.Final), sh.shorten(a.Result.Error)
			gates := make([]batch.GateResult, len(a.Gates))
			for k, g := range a.Gates {
				g.Command, g.Output = sh.shorten(g.Command), sh.shorten(g.Output)
				gates[k] = g
			}
			a.Gates = gates
			attempts[j] = a
		}
		t.Attempts = attempts
		tasks[i] = t
	}
	res.Tasks = tasks
	return res
}
