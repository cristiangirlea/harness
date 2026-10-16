package watch

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/cristiangirlea/harness/internal/agent"
)

func trimProgExt(s string) string {
	if strings.HasSuffix(s, ".exe") || strings.HasSuffix(s, ".cmd") {
		return s[:len(s)-4]
	}
	return s
}

// backendStart matches the info line emitted by cliBackend.Run, before any
// display shortening or masking. A live run may not have saved its program yet.
func backendStart(e agent.Event, backend string) bool {
	if e.Kind != agent.EventInfo {
		return false
	}
	first, _, ok := strings.Cut(e.Text, " ")
	if !ok {
		return false
	}
	first = trimProgExt(first)
	if backend != "" {
		return first == trimProgExt(filepath.Base(backend))
	}
	for _, name := range []string{"claude", "codex", "agy", "gemini", "opencode"} {
		if first == name {
			return true
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
		for {
			switch next() {
			case "-c", "-lc":
				return true
			case "-l", "--login":
				continue
			}
			return false
		}
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
				case "-command", "-c":
					return true
				case "-noprofile", "-noninteractive", "-nologo":
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
	if quote := rest[0]; quote == '\'' || quote == '"' {
		if quoteEnd(rest) != len(rest)-1 {
			return command
		}
		rest = rest[1 : len(rest)-1]
		if quote == '"' {
			rest = strings.ReplaceAll(rest, `\"`, `"`)
		}
	}
	if !balancedCommand(rest) {
		return command
	}
	return rest
}

func unwrapCommandValue(raw json.RawMessage) (string, bool) {
	var command string
	if json.Unmarshal(raw, &command) == nil {
		shown := unwrapCommand(command)
		return shown, shown != command
	}
	var args []string
	if json.Unmarshal(raw, &args) != nil {
		return "", false
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
		return "", false
	}
	shown := strings.Join(args[i:], " ")
	return shown, balancedCommand(shown)
}

// unwrapInput rewrites only the command field in place, preserving field order
// and other JSON values and leaving the source event's bytes untouched.
func unwrapInput(input json.RawMessage) json.RawMessage {
	if !json.Valid(input) {
		return input
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return input
	}
	for dec.More() {
		key, _ := dec.Token()
		var raw json.RawMessage
		_ = dec.Decode(&raw)
		if key != "command" {
			continue
		}
		shown, ok := unwrapCommandValue(raw)
		if !ok {
			return input
		}
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(shown)
		encoded := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
		end := int(dec.InputOffset())
		start := end - len(raw)
		out := make(json.RawMessage, 0, len(input)-len(raw)+len(encoded))
		out = append(out, input[:start]...)
		out = append(out, encoded...)
		out = append(out, input[end:]...)
		return out
	}
	return input
}
