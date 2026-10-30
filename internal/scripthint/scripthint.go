// Package scripthint suggests kept scripts before tool use.
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

// Script carries a kept script's matches pattern.
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

// Find returns a hint or silently ignores unrecognized input.
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
		if runsKept(command, filepath.Base(file)) {
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

// Root finds the nearest enclosing .git entry.
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

// Load reads valid script matches headers in file-name order.
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
	safeParameters = regexp.MustCompile(`^(?:none|[A-Za-z0-9_-]+:[A-Za-z0-9_-]+=[A-Za-z0-9_.,\[\]|/-]+(?:; [A-Za-z0-9_-]+:[A-Za-z0-9_-]+=[A-Za-z0-9_.,\[\]|/-]+)*)$`)
)

func runsKept(command, file string) bool {
	path := `(?:\./)?scripts/` + regexp.QuoteMeta(file) + `(?:[\s;|&'"()]|$)`
	prefix := `(?:^|[;|&\n({])\s*(?:(?:sudo|env|exec|time|nohup|\w+=\S*)\s+)*`
	run := regexp.MustCompile(prefix + `(?:bash|sh)(?:\s+-\S+)*\s+` + path + `|` + prefix + `go\s+run\s+` + path + `|` + prefix + `&\s*` + path + `|` + prefix + `(?:pwsh|powershell)(?:\.exe)?\s+-[Ff]ile\s+` + path + `|` + prefix + `\./scripts/` + regexp.QuoteMeta(file) + `(?:[\s;|&'"()]|$)`)
	return run.MatchString(command)
}

// Untrusted headers require plain filenames and bounded parameter syntax.
func hintSafe(file, parameters string) bool {
	return safeFile.MatchString(file) && len(parameters) <= 200 && safeParameters.MatchString(parameters)
}

func keptFiles(root string) []string {
	var files []string
	for _, pattern := range []string{"*.sh", "*.go"} {
		got, _ := filepath.Glob(filepath.Join(root, "scripts", pattern))
		files = append(files, got...)
	}
	sort.Slice(files, func(i, j int) bool { return filepath.Base(files[i]) < filepath.Base(files[j]) })
	return files
}

// Kept reports whether file starts with a kept-script header, as scripts/verify.sh reads it.
func Kept(file string) bool {
	header := readHeader(file)
	return header["name"] != "" && header["summary"] != ""
}

// Match scripts/verify.sh header boundaries so hints describe the verified script.
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

// Normalize collapses and trims whitespace.
func Normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

var (
	psHere      = regexp.MustCompile(`(?s)@'\r?\n(.*?)\r?\n'@|@"\r?\n(.*?)\r?\n"@`)
	psHerePipe  = regexp.MustCompile(`^[ \t]*\|[ \t]*(?:python3?|py|node|perl|bash|sh)(?:\.exe)?(?:[ \t]+-)?[ \t]*(?:[\r\n;|&]|$)`)
	heredoc     = regexp.MustCompile(`<<(-?)[ \t]*(['"]?)([A-Za-z_][A-Za-z0-9_]*)(['"]?)`)
	interpreter = regexp.MustCompile(`(?:^|[;|&\n({])\s*(?:(?:sudo|env|exec|time|nohup|\w+=\S*)\s+)*(?:\S*[/\\])?(python3?|py|node|perl|bash|sh)(?:\.exe)?\s+(-c|-e)\s+`)
	powershell  = regexp.MustCompile(`(?i)(?:^|[;|&\n({]\s*|\s)(?:\S*[/\\])?(?:pwsh|powershell)(?:\.exe)?\s(?:[^;|&\n]*?\s)?-(?:command|c)\s+`)
)

// Inline joins heredocs and interpreter scripts with newlines, or returns "".
func Inline(command string) string {
	var parts []string
	for _, m := range psHere.FindAllStringSubmatchIndex(command, -1) {
		if psHerePipe.MatchString(command[m[1]:]) {
			for i := 2; i < len(m); i += 2 {
				if m[i] >= 0 {
					parts = append(parts, command[m[i]:m[i+1]])
				}
			}
		}
	}
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
		if (name == "node" || name == "perl") != (flag == "-e") {
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

// Context supplies advisory text without making an approval decision.
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

// Record appends a shown hint, ignoring failures so the hook cannot fail.
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
