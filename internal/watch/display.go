package watch

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/cristiangirlea/harness/internal/agent"
)

// backendStart matches the info line emitted by cliBackend.Run, before any
// display shortening or masking. A live run may not have saved its program yet.
func backendStart(e agent.Event, backend string) bool {
	if e.Kind != agent.EventInfo {
		return false
	}
	if backend != "" {
		return strings.HasPrefix(e.Text, filepath.Base(backend)+" ")
	}
	for _, name := range []string{"claude", "codex", "agy", "gemini", "opencode"} {
		for _, ext := range []string{"", ".exe", ".cmd"} {
			if strings.HasPrefix(e.Text, name+ext+" ") {
				return true
			}
		}
	}
	return false
}

// shellPrefix consumes only a recognised interpreter and its command flags.
// next supplies either words from display text or already decoded argv entries.
func shellPrefix(next func() string) bool {
	program := next()
	if program == "&" {
		program = next()
	}
	program = program[strings.LastIndexAny(program, `/\`)+1:]
	program = strings.TrimSuffix(strings.ToLower(program), ".exe")
	switch program {
	case "bash", "sh", "zsh":
		flag := next()
		return flag == "-c" || flag == "-lc"
	case "powershell", "pwsh", "cmd":
		for {
			flag := strings.ToLower(next())
			if program == "cmd" {
				switch flag {
				case "/c":
					return true
				case "/d", "/s":
					continue
				}
			} else {
				switch flag {
				case "-command":
					return true
				case "-noprofile", "-noninteractive":
					continue
				case "-executionpolicy":
					if next() != "" {
						continue
					}
				}
			}
			return false
		}
	}
	return false
}

// quoteEnd finds the matching quote without interpreting the command itself.
// Backslash and PowerShell backtick escapes stay exactly as recorded.
func quoteEnd(s string) int {
	for i := 1; i < len(s); i++ {
		if s[0] == '"' && (s[i] == '\\' || s[i] == '`') && i+1 < len(s) {
			i++
			continue
		}
		if s[i] == s[0] {
			return i
		}
	}
	return -1
}

func balancedCommand(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '`':
			i++
		case '\'', '"':
			end := quoteEnd(s[i:])
			if end < 0 {
				return false
			}
			i += end
		}
	}
	return true
}

func unwrapCommand(command string) string {
	rest := strings.TrimSpace(command)
	next := func() string {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return ""
		}
		if rest[0] == '\'' || rest[0] == '"' {
			end := quoteEnd(rest)
			if end < 0 || end+1 < len(rest) && !strings.ContainsRune(" \t\r\n", rune(rest[end+1])) {
				return ""
			}
			word := rest[1:end]
			rest = rest[end+1:]
			return word
		}
		end := strings.IndexAny(rest, " \t\r\n")
		if end < 0 {
			end = len(rest)
		}
		word := rest[:end]
		rest = rest[end:]
		return word
	}
	if !shellPrefix(next) {
		return command
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return command
	}
	if rest[0] == '\'' || rest[0] == '"' {
		if quoteEnd(rest) != len(rest)-1 {
			return command
		}
		rest = rest[1 : len(rest)-1]
	}
	if !balancedCommand(rest) {
		return command
	}
	return rest
}

// unwrapInput rewrites only the command field, preserving other JSON values
// (including large integers) and leaving the source event's bytes untouched.
func unwrapInput(input json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(input, &fields) != nil {
		return input
	}
	var command, shown string
	if json.Unmarshal(fields["command"], &command) == nil {
		shown = unwrapCommand(command)
		if shown == command {
			return input
		}
	} else {
		var args []string
		if json.Unmarshal(fields["command"], &args) != nil {
			return input
		}
		i := 0
		if !shellPrefix(func() string {
			if i == len(args) {
				return ""
			}
			word := args[i]
			i++
			return word
		}) || i == len(args) {
			return input
		}
		shown = strings.Join(args[i:], " ")
		if !balancedCommand(shown) {
			return input
		}
	}
	fields["command"], _ = json.Marshal(shown)
	out, err := json.Marshal(fields)
	if err != nil {
		return input
	}
	return out
}
