package agent

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/mask"
	"github.com/cristiangirlea/harness/internal/tools"
)

type refusalInput struct {
	Command   string `json:"command"`
	Path      string `json:"path"`
	Content   string `json:"content"`
	NewString string `json:"new_string"`
	normal    string
	runs      []string
}

type refusal struct {
	call, result int
	rule         string
}

// RefusalFlags highlights textual matches for review without deciding whether a run succeeded.
func RefusalFlags(events []Event) []RefusalFlag {
	pending := map[string][]int{}
	refused := map[int]bool{}
	var refusals []refusal
	inputs := make([]refusalInput, len(events))
	ordinals := make([]int, len(events))
	calls := 0
	type refusalKey struct{ rule, command string }
	seen := map[refusalKey]bool{}
	for i, e := range events {
		if e.Partial {
			continue
		}
		switch e.Kind {
		case EventToolCall:
			ordinals[i] = calls
			calls++
			pending[e.Tool] = append(pending[e.Tool], i)
			var input refusalInput
			if json.Unmarshal(e.Input, &input) == nil {
				switch e.Tool {
				case "write_file", "edit_file":
					if e.Tool == "edit_file" {
						input.Content = input.NewString
					}
					input.normal = refusalNormalize(input.Content)
				case "run_command":
					input.runs = refusalExecutions(input.Command)
				}
				inputs[i] = input
			}
		case EventToolResult:
			stack := pending[e.Tool]
			if len(stack) == 0 {
				continue
			}
			call := stack[len(stack)-1]
			pending[e.Tool] = stack[:len(stack)-1]
			if rule := refusalRule(e); rule != "" {
				refused[call] = true
				command := inputs[call].Command
				key := refusalKey{rule, command}
				if events[call].Tool == "run_command" && command != "" && !seen[key] && len(refusals) < 20 {
					seen[key] = true
					refusals = append(refusals, refusal{call, i, rule})
				}
			}
		}
	}
	if len(refusals) == 0 {
		return nil
	}
	var flags []RefusalFlag
	type matchKey struct{ rule, program, text string }
	matched := map[matchKey]bool{}
	for _, r := range refusals {
		command := inputs[r.call].Command
		normal := refusalNormalize(command)
		program := refusalProgram(command)
		var programPattern *regexp.Regexp
		if program != "" {
			programPattern = refusalProgramPattern(program)
		}
		matches := func(text string) bool {
			if text == "" {
				return false
			}
			key := matchKey{rule: r.rule, text: text}
			if r.rule == "permission" {
				if programPattern == nil || !strings.Contains(text, strings.Fields(program)[0]) {
					return false
				}
				key.program = program
			}
			if result, ok := matched[key]; ok {
				return result
			}
			result := false
			for _, line := range strings.Split(text, "\n") {
				if r.rule == "permission" {
					result = programPattern.MatchString(refusalNormalize(line))
				} else if tools.Guard(line) == r.rule {
					result = true
				}
				if result {
					break
				}
			}
			matched[key] = result
			return result
		}
		files := map[string]bool{}
		// Counting matching files by full path and base name keeps each operand one lookup.
		byPath, byBase := map[string]int{}, map[string]int{}
		setFile := func(filename string, match bool) {
			if files[filename] != match {
				delta := 1
				if !match {
					delta = -1
				}
				filename := strings.ReplaceAll(filename, `\`, "/")
				byPath[filename] += delta
				byBase[path.Base(filename)] += delta
			}
			files[filename] = match
		}
		for i, e := range events {
			if e.Partial || e.Kind != EventToolCall || refused[i] {
				continue
			}
			in := inputs[i]
			var kinds []string
			if i > r.result {
				switch e.Tool {
				case "write_file", "edit_file":
					if len(normal) >= 8 && strings.Contains(in.normal, normal) {
						kinds = append(kinds, "written")
					}
				case "run_command":
					script := matches(in.Command)
					for _, operand := range in.runs {
						if script || byPath[operand] > 0 || byBase[operand[strings.LastIndex(operand, "/")+1:]] > 0 {
							script = true
							break
						}
					}
					if script {
						kinds = append(kinds, "script")
					}
					if programPattern != nil && refusalWrapped(in.Command, programPattern) {
						kinds = append(kinds, "wrapped")
					}
				}
			}
			for _, kind := range kinds {
				flags = append(flags, RefusalFlag{Kind: kind, Rule: r.rule, Refused: refusalSummary(events[r.call]), Later: refusalSummary(e), RefusedAt: ordinals[r.call], LaterAt: ordinals[i]})
				if len(flags) == 20 {
					return flags
				}
			}
			if in.Path != "" {
				switch e.Tool {
				case "write_file":
					setFile(in.Path, matches(in.Content))
				case "edit_file":
					setFile(in.Path, files[in.Path] || matches(in.Content))
				}
			}
		}
	}
	return flags
}

func refusalRule(e Event) string {
	if !e.IsError {
		return ""
	}
	if e.Text == "Permission denied by the user's harness settings." {
		return "permission"
	}
	const prefix = "blocked by the hrn guard ("
	if !strings.HasPrefix(e.Text, prefix) {
		return ""
	}
	rule, _, ok := strings.Cut(strings.TrimPrefix(e.Text, prefix), "):")
	if ok && rule != "" && e.Text == tools.GuardError(rule).Error() {
		return rule
	}
	return ""
}

func refusalNormalize(s string) string { return strings.Join(strings.Fields(s), " ") }

func refusalProgram(command string) string {
	command = strings.TrimSpace(command)
	for {
		loc := refusalAssignment.FindStringIndex(command)
		if loc == nil {
			break
		}
		command = strings.TrimSpace(command[loc[1]:])
	}
	words := strings.Fields(command)
	if len(words) < 2 {
		return ""
	}
	return strings.Join(words[:2], " ")
}

var refusalAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=(?:[^\s"'\\]|\\.|"[^"]*"|'[^']*')*(?:\s+|$)`)
var refusalWrapper = regexp.MustCompile(`(?i)(?:^|[\s;&|("'])(?:sh\s+-c|bash\s+-c|pwsh\s+-Command|powershell\s+-Command|cmd\s+/c|python3?\s+-c|node\s+-e|perl\s+-e|ruby\s+-e)\s+`)

func refusalProgramPattern(program string) *regexp.Regexp {
	pattern := `(?:^|[^\w-])` + regexp.QuoteMeta(program) + `(?:$|[^\w-])`
	return regexp.MustCompile(pattern)
}

func refusalWrapped(command string, program *regexp.Regexp) bool {
	// Later wrappers lie inside the first one's text, so it alone decides.
	loc := refusalWrapper.FindStringIndex(command)
	return loc != nil && program.MatchString(refusalNormalize(command[loc[1]:]))
}

var refusalWords = regexp.MustCompile(`"[^"]*"|'[^']*'|[^\s"']+`)

func refusalCommandSegments(command string) []string {
	var segments []string
	start := 0
	var quote byte
	for i := 0; i < len(command); i++ {
		c := command[i]
		if (c == '\\' || c == '`') && quote != '\'' {
			i++
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == ';' || c == '|' || (c == '&' && i+1 < len(command) && command[i+1] == '&') {
			segments = append(segments, command[start:i])
			if (c == '&' || c == '|') && i+1 < len(command) && command[i+1] == c {
				i++
			}
			start = i + 1
		}
	}
	return append(segments, command[start:])
}

func refusalExecutions(command string) []string {
	var operands []string
	for _, segment := range refusalCommandSegments(command) {
		words := refusalWords.FindAllString(segment, -1)
		if len(words) == 0 {
			continue
		}
		for i := range words {
			words[i] = strings.Trim(words[i], `"'`)
		}
		operand := words[0]
		operands = append(operands, strings.ReplaceAll(operand, `\`, "/"))
		switch strings.ToLower(path.Base(strings.ReplaceAll(operand, `\`, "/"))) {
		case ".", "source", "sh", "bash", "zsh", "pwsh", "powershell", "python", "python3", "node", "perl", "ruby":
			operand = ""
			for _, word := range words[1:] {
				if word == "-c" || word == "-e" || strings.EqualFold(word, "-Command") {
					break
				}
				if strings.HasPrefix(word, "-") {
					continue
				}
				operand = word
				break
			}
		}
		if operand != "" {
			operands = append(operands, strings.ReplaceAll(operand, `\`, "/"))
		}
	}
	return operands
}

func refusalSummary(e Event) string {
	s := e.Text
	if s == "" {
		s = e.Tool + " " + string(e.Input)
	}
	// Masking before the cut keeps a secret cut in half recognisable.
	s = mask.Text(refusalNormalize(s))
	if len(s) > 200 {
		s = s[:200]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}
