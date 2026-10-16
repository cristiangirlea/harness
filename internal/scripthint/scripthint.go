// Package scripthint names the kept script that covers an inline script an
// agent is about to run, for an advisory Claude Code PreToolUse hook.
package scripthint

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Script is a kept script whose header carries a matches: pattern.
type Script struct {
	File       string
	Parameters string
	Matches    *regexp.Regexp
}

// Hint names the kept script that covers a command's inline script.
type Hint struct {
	Script     string
	Parameters string
	Repo       string
}

type payload struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
	Cwd string `json:"cwd"`
}

// Find reads one PreToolUse payload and returns the hint to show, if any.
// Anything it does not understand yields no hint, never an error.
func Find(data []byte) (Hint, bool) {
	var p payload
	if json.Unmarshal(data, &p) != nil || p.Cwd == "" {
		return Hint{}, false
	}
	if p.ToolName != "Bash" && p.ToolName != "PowerShell" {
		return Hint{}, false
	}
	text := Normalize(Inline(p.ToolInput.Command))
	if text == "" {
		return Hint{}, false
	}
	root, ok := Root(p.Cwd)
	if !ok {
		return Hint{}, false
	}
	command := strings.ReplaceAll(p.ToolInput.Command, `\`, "/")
	for _, file := range keptFiles(root) {
		if strings.Contains(command, "scripts/"+filepath.Base(file)) {
			return Hint{}, false
		}
	}
	for _, s := range Load(root) {
		if s.Matches.MatchString(text) {
			return Hint{Script: "scripts/" + s.File, Parameters: s.Parameters, Repo: root}, true
		}
	}
	return Hint{}, false
}

// Root walks up from dir to the first directory holding a .git file or directory.
func Root(dir string) (string, bool) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// Load reads the headers of root/scripts/*.sh and *.go in file-name order and
// keeps those with a valid matches: pattern.
func Load(root string) []Script {
	var scripts []Script
	for _, file := range keptFiles(root) {
		header := readHeader(file)
		re, err := regexp.Compile(header["matches"])
		if header["matches"] == "" || err != nil || !hintSafe(filepath.Base(file), header["parameters"]) {
			continue
		}
		scripts = append(scripts, Script{File: filepath.Base(file), Parameters: header["parameters"], Matches: re})
	}
	return scripts
}

var (
	safeFile       = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	safeParameters = regexp.MustCompile(`^[A-Za-z0-9_:=;.,\[\]|/ -]*$`)
)

// hintSafe reports whether a header's text may reach the agent: any repository
// can commit scripts, so only a plain file name and a short parameters grammar pass.
func hintSafe(file, parameters string) bool {
	return safeFile.MatchString(file) && len(parameters) <= 200 && safeParameters.MatchString(parameters)
}

// keptFiles lists root/scripts/*.sh and *.go together, sorted by file name.
func keptFiles(root string) []string {
	var files []string
	for _, pattern := range []string{"*.sh", "*.go"} {
		got, _ := filepath.Glob(filepath.Join(root, "scripts", pattern))
		files = append(files, got...)
	}
	sort.Slice(files, func(i, j int) bool { return filepath.Base(files[i]) < filepath.Base(files[j]) })
	return files
}

// readHeader parses the leading comment block the way scripts/verify.sh does:
// shebangs skipped, "# " or "// " lines read, anything else ends it.
func readHeader(file string) map[string]string {
	header := map[string]string{}
	f, err := os.Open(file)
	if err != nil {
		return header
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if strings.HasPrefix(line, "#!") {
			continue
		}
		var ok bool
		if line, ok = strings.CutPrefix(line, "# "); !ok {
			if line, ok = strings.CutPrefix(line, "// "); !ok {
				break
			}
		}
		if key, value, found := strings.Cut(line, ": "); found {
			header[key] = value
		}
	}
	return header
}

// Normalize trims text and collapses each run of whitespace to one space.
func Normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

var (
	heredoc     = regexp.MustCompile(`<<(-?)[ \t]*(['"]?)([A-Za-z_][A-Za-z0-9_]*)(['"]?)`)
	interpreter = regexp.MustCompile(`(?:^|[;|&\n({]\s*|\b(?:sudo|env|exec)\s+)(?:\S*[/\\])?(python3?|py|node|bash|sh)(?:\.exe)?\s+(-c|-e)\s+`)
	powershell  = regexp.MustCompile(`(?i)(?:^|[;|&\n({]\s*|\s)(?:\S*[/\\])?(?:pwsh|powershell)(?:\.exe)?\s(?:[^;|&\n]*?\s)?-(?:command|c)\s+`)
)

// Inline returns the inline scripts in a command (heredoc bodies and the
// arguments of interpreter flags), joined by newlines, or "" when there are none.
func Inline(command string) string {
	var parts []string
	for _, m := range heredoc.FindAllStringSubmatchIndex(command, -1) {
		start := m[0]
		if (start > 0 && command[start-1] == '<') || (m[1] < len(command) && command[m[1]] == '<') {
			continue
		}
		open, close := command[m[4]:m[5]], command[m[8]:m[9]]
		if open != close {
			continue
		}
		if body := heredocBody(command[m[1]:], command[m[6]:m[7]], m[3] > m[2]); body != "" {
			parts = append(parts, body)
		}
	}
	for _, m := range interpreter.FindAllStringSubmatchIndex(command, -1) {
		name, flag := command[m[2]:m[3]], command[m[4]:m[5]]
		if (name == "node") != (flag == "-e") {
			continue
		}
		if word := shellWord(command[m[1]:]); word != "" {
			parts = append(parts, word)
		}
	}
	for _, m := range powershell.FindAllStringIndex(command, -1) {
		if script := powershellScript(command[m[1]:]); script != "" {
			parts = append(parts, script)
		}
	}
	return strings.Join(parts, "\n")
}

// heredocBody returns the lines after the heredoc's own line up to its
// delimiter, or to the end when the delimiter never comes.
func heredocBody(rest, delimiter string, dash bool) string {
	_, after, ok := strings.Cut(rest, "\n")
	if !ok {
		return ""
	}
	var body []string
	for line := range strings.SplitSeq(after, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if dash {
			line = strings.TrimLeft(line, "\t")
		}
		if line == delimiter {
			break
		}
		body = append(body, line)
	}
	return strings.Join(body, "\n")
}

// shellWord reads one POSIX shell word: quoted and bare pieces concatenated.
func shellWord(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		switch c := s[i]; {
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return b.String() + s[i+1:]
			}
			b.WriteString(s[i+1 : i+1+end])
			i += end + 2
		case c == '"':
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
					i++
				}
				b.WriteByte(s[i])
				i++
			}
			i++
		case strings.IndexByte(" \t\n;|&<>()", c) >= 0:
			return b.String()
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// powershellScript reads a -Command argument: a script block, or a quoted
// string that holds more than one statement.
func powershellScript(s string) string {
	if s == "" {
		return ""
	}
	switch s[0] {
	case '{':
		depth := 0
		for i := 0; i < len(s); i++ {
			switch s[i] {
			case '{':
				depth++
			case '}':
				if depth--; depth == 0 {
					return s[1:i]
				}
			}
		}
		return s[1:]
	case '\'', '"':
		text := powershellString(s)
		if strings.ContainsAny(text, ";\n") {
			return text
		}
	}
	return ""
}

// powershellString reads a quoted PowerShell string: a doubled quote stands for
// one inside single quotes, and a backtick or backslash escapes inside double quotes.
func powershellString(s string) string {
	q := s[0]
	var b bytes.Buffer
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == q && q == '\'' && i+1 < len(s) && s[i+1] == '\'':
			b.WriteByte(c)
			i++
		case c == q:
			return b.String()
		case q == '"' && (c == '`' || c == '\\') && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Context is the hook's stdout line: added context only, never a decision.
func Context(h Hint) []byte {
	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	out.HookSpecificOutput.AdditionalContext = h.Script + " covers this inline script; parameters: " + h.Parameters + "."
	return encode(out)
}

// Record appends one line for a shown hint; a failure is ignored so the hook never fails.
func Record(path string, h Hint, now time.Time) {
	line := encode(struct {
		Time   string `json:"time"`
		Script string `json:"script"`
		Repo   string `json:"repo"`
	}{now.UTC().Format(time.RFC3339), h.Script, h.Repo})
	os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	f.Write(line)
	f.Close()
}

func encode(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return b.Bytes()
}
