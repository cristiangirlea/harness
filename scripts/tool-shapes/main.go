// name: tool-shapes-main
// summary: Measure recorded tool output and repeated inline scripts without exposing text.
// effects: read, exec
// parameters: --home:path=HRN_HOME or ~/.hrn; --days:int=30; --json:bool=true
// example: go test -count=1 ./scripts/tool-shapes
// hash: e802a27b13c10af11ff2052afc358f95051799d4a165718df7bc8f1fd9382ddb
package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var shapeNames = []string{"test", "log", "json", "table", "terminal", "other"}
var testLine = regexp.MustCompile(`(?m)^(?:go test\b|ok\s|FAIL\b|--- )`)
var logLine = regexp.MustCompile(`(?mi)^(?:\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}|\d{2}:\d{2}:\d{2}|\[?(?:trace|debug|info|warn|warning|error|fatal)\b)`)

func shellTool(tool string) bool {
	switch strings.ToLower(tool) {
	case "shell", "bash", "exec", "exec_command", "run_shell_command", "shell_command", "terminal":
		return true
	}
	return false
}

func classify(text, tool string) string {
	text = strings.TrimSpace(text)
	if testLine.MatchString(text) {
		return "test"
	}
	if logLine.MatchString(text) {
		return "log"
	}
	if json.Valid([]byte(text)) {
		return "json"
	}
	for _, delimiter := range []rune{'\t', ','} {
		r := csv.NewReader(strings.NewReader(text))
		r.Comma = delimiter
		rows, err := r.ReadAll()
		if err == nil && len(rows) >= 2 && len(rows[0]) >= 2 {
			return "table"
		}
	}
	if shellTool(tool) {
		return "terminal"
	}
	return "other"
}

var interpreter = regexp.MustCompile(`(?:^\s*|[;|&\n]\s*|\b(?:sudo|env)\s+)(python3?|node|perl)\s+(-c|-e)(?:\s|$)`)
var goTemp = regexp.MustCompile(`(?i)(?:^\s*|[;|&\n]\s*)go\s+run\s+["']?(?:[^\s"']*[/\\](?:tmp|temp)[/\\]|\$\{?TMPDIR\}?[/\\]|\$\{?TEMP\}?[/\\]|%TEMP%[/\\])[^\s"']*\.go\b`)

func inlineShape(command string) string {
	if strings.Contains(command, "<<") {
		return "heredoc"
	}
	if m := interpreter.FindStringSubmatch(command); m != nil {
		if strings.HasPrefix(m[1], "python") && m[2] == "-c" {
			return "python"
		}
		if (m[1] == "node" || m[1] == "perl") && m[2] == "-e" {
			return m[1]
		}
	}
	if goTemp.MatchString(command) {
		return "go"
	}
	return ""
}

var quoted = regexp.MustCompile(`"(?:\\.|[^"\\])*"|'[^']*'`)
var paths = regexp.MustCompile(`(?:[A-Za-z]:[/\\]|/|\.\.?[/\\]|~[/\\]|\$\{?[A-Za-z_][A-Za-z_0-9]*\}?[/\\])[^\s;|&"']+`)
var numbers = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
var identifiers = regexp.MustCompile(`\b[A-Za-z_][A-Za-z_0-9]{12,}\b`)

func normalize(command string) string {
	command = quoted.ReplaceAllString(command, "<literal>")
	command = paths.ReplaceAllString(command, "<path>")
	command = numbers.ReplaceAllString(command, "<number>")
	command = identifiers.ReplaceAllString(command, "<identifier>")
	return strings.Join(strings.Fields(command), " ")
}

type shape struct {
	Shape       string   `json:"shape"`
	Bytes       int64    `json:"bytes"`
	ResultShare float64  `json:"tool_result_share"`
	InputShare  *float64 `json:"input_token_share_estimate"`
	tokenBytes  int64
}
type group struct {
	Shape      string `json:"shape"`
	Count      int    `json:"count"`
	Runs       int    `json:"runs"`
	Bytes      int64  `json:"command_bytes"`
	tokenBytes int64
}
type stats struct {
	Shapes             []shape  `json:"shapes"`
	InlineScripts      []group  `json:"inline_scripts"`
	ToolResultBytes    int64    `json:"tool_result_bytes"`
	InputTokens        int64    `json:"input_plus_cache_read_tokens"`
	InlineBytes        int64    `json:"inline_script_bytes"`
	InlineShare        *float64 `json:"inline_script_input_share_estimate"`
	RepeatedBytes      int64    `json:"repeated_inline_script_bytes"`
	RepeatedShare      *float64 `json:"repeated_inline_script_input_share_estimate"`
	groups             map[string]*group
	inlineTokenBytes   int64
	repeatedTokenBytes int64
}
type report struct {
	*stats
	Claude *stats `json:"claude"`
	Runs   struct {
		Read       int `json:"read"`
		WithUsage  int `json:"with_usage"`
		WithTokens int `json:"with_tokens"`
		Unreadable int `json:"skipped_unreadable"`
	} `json:"runs"`
}
type event struct {
	Kind  string          `json:"kind"`
	Tool  string          `json:"tool"`
	Text  string          `json:"text"`
	Input json.RawMessage `json:"input"`
	Usage *struct {
		Input int64 `json:"input_tokens"`
		Cache int64 `json:"cache_read_tokens"`
	} `json:"usage"`
}

func newStats() *stats {
	s := &stats{InlineScripts: []group{}, groups: map[string]*group{}}
	for _, name := range shapeNames {
		s.Shapes = append(s.Shapes, shape{Shape: name})
	}
	return s
}
func fraction(bytes, tokens int64) *float64 {
	if tokens <= 0 {
		return nil
	}
	v := float64(bytes) / 4 / float64(tokens)
	return &v
}
func (s *stats) add(events []event, tokens *int64) {
	if tokens != nil {
		s.InputTokens += *tokens
	}
	seen := map[string]bool{}
	var pending []string
	ambiguous := false
	for _, e := range events {
		switch e.Kind {
		case "tool_result":
			tool := e.Tool
			if tool == "" && len(pending) > 0 {
				allShell := true
				for _, name := range pending {
					allShell = allShell && shellTool(name)
				}
				if allShell && !ambiguous {
					tool = "shell"
				} else if len(pending) > 1 {
					ambiguous = true
				}
			}
			if len(pending) > 0 {
				index := 0
				if e.Tool != "" {
					found := false
					for i, name := range pending {
						if name == e.Tool {
							index = i
							found = true
							break
						}
					}
					if !found {
						ambiguous = true
					}
				}
				pending = append(pending[:index], pending[index+1:]...)
			}
			if len(pending) == 0 {
				ambiguous = false
			}
			n := int64(len(e.Text))
			s.ToolResultBytes += n
			label := classify(e.Text, tool)
			for i := range s.Shapes {
				if s.Shapes[i].Shape == label {
					s.Shapes[i].Bytes += n
					if tokens != nil {
						s.Shapes[i].tokenBytes += n
					}
				}
			}
		case "tool_call":
			pending = append(pending, e.Tool)
			var input struct {
				Command string `json:"command"`
				Cmd     string `json:"cmd"`
			}
			if json.Unmarshal(e.Input, &input) != nil {
				continue
			}
			command := input.Command
			if command == "" {
				command = input.Cmd
			}
			label := inlineShape(command)
			if label == "" {
				continue
			}
			key := label + ":" + normalize(command)
			g := s.groups[key]
			if g == nil {
				g = &group{Shape: label}
				s.groups[key] = g
			}
			g.Count++
			g.Bytes += int64(len(command))
			s.InlineBytes += int64(len(command))
			if tokens != nil {
				g.tokenBytes += int64(len(command))
				s.inlineTokenBytes += int64(len(command))
			}
			if !seen[key] {
				g.Runs++
				seen[key] = true
			}
		}
	}
}
func (s *stats) finish() {
	for i := range s.Shapes {
		v := &s.Shapes[i]
		if s.ToolResultBytes > 0 {
			v.ResultShare = float64(v.Bytes) / float64(s.ToolResultBytes)
		}
		v.InputShare = fraction(v.tokenBytes, s.InputTokens)
	}
	keys := make([]string, 0, len(s.groups))
	for k, g := range s.groups {
		keys = append(keys, k)
		if g.Count > 1 {
			s.RepeatedBytes += g.Bytes
			s.repeatedTokenBytes += g.tokenBytes
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := s.groups[keys[i]], s.groups[keys[j]]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return keys[i] < keys[j]
	})
	for _, key := range keys[:min(20, len(keys))] {
		s.InlineScripts = append(s.InlineScripts, *s.groups[key])
	}
	s.InlineShare = fraction(s.inlineTokenBytes, s.InputTokens)
	s.RepeatedShare = fraction(s.repeatedTokenBytes, s.InputTokens)
}
func readJSON(path string, value any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	if err = d.Decode(value); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func readEvents(path string) ([]event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	var events []event
	for {
		var e event
		err = d.Decode(&e)
		if err == io.EOF {
			return events, nil
		}
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
}
func measure(home string, days int, now time.Time) (*report, error) {
	entries, err := os.ReadDir(filepath.Join(home, "runs"))
	if err != nil {
		return nil, errors.New("cannot read runs directory")
	}
	r := &report{stats: newStats(), Claude: newStats()}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if len(name) < 16 {
			continue
		}
		started, err := time.ParseInLocation("20060102-150405", name[:15], time.Local)
		if err != nil {
			continue
		}
		dir := filepath.Join(home, "runs", name)
		type totals struct {
			Status  string `json:"status"`
			Backend string `json:"backend"`
			Input   *int64 `json:"input_tokens"`
			Cache   int64  `json:"cache_read_tokens"`
		}
		var result struct {
			totals
			Started time.Time `json:"started"`
			Result  *totals   `json:"result"`
		}
		err = readJSON(filepath.Join(dir, "result.json"), &result)
		if os.IsNotExist(err) {
			continue
		}
		if !result.Started.IsZero() {
			started = result.Started
		}
		if started.Before(cutoff) || started.After(now) {
			continue
		}
		if err != nil {
			r.Runs.Unreadable++
			continue
		}
		if result.Result != nil {
			backend := result.Backend
			result.totals = *result.Result
			if result.Backend == "" {
				result.Backend = backend
			}
		}
		if result.Status == "" {
			continue
		}
		var task struct {
			Backend string `json:"backend"`
		}
		if readJSON(filepath.Join(dir, "task.json"), &task) != nil {
			r.Runs.Unreadable++
			continue
		}
		events, err := readEvents(filepath.Join(dir, "events.jsonl"))
		if err != nil {
			r.Runs.Unreadable++
			continue
		}
		r.Runs.Read++
		backend := result.Backend
		if backend == "" {
			backend = task.Backend
		}
		hasUsage, hasCalls := false, false
		var tokens int64
		for _, e := range events {
			hasUsage = hasUsage || e.Kind == "usage" && e.Usage != nil
			hasCalls = hasCalls || e.Kind == "tool_call"
			if e.Kind == "usage" && e.Usage != nil {
				tokens += e.Usage.Input
				if backend != "codex" {
					tokens += e.Usage.Cache
				}
			}
		}
		if hasUsage {
			r.Runs.WithUsage++
		}
		var tokenFigure *int64
		if hasUsage {
			tokenFigure = &tokens
		} else if result.Input != nil {
			tokens = *result.Input
			if backend != "codex" {
				tokens += result.Cache
			}
			tokenFigure = &tokens
		}
		if tokenFigure != nil {
			r.Runs.WithTokens++
		}
		r.stats.add(events, tokenFigure)
		if backend == "claude" && hasCalls {
			r.Claude.add(events, tokenFigure)
		}
	}
	r.stats.finish()
	r.Claude.finish()
	return r, nil
}
func options(args []string) (string, int, error) {
	home := os.Getenv("HRN_HOME")
	if home == "" {
		base, err := os.UserHomeDir()
		if err != nil {
			return "", 0, errors.New("cannot find home")
		}
		home = filepath.Join(base, ".hrn")
	}
	days := 30
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
		case "--home", "--days":
			flag := args[i]
			i++
			if i == len(args) {
				return "", 0, errors.New("missing parameter")
			}
			if flag == "--home" {
				home = args[i]
				if home == "" {
					return "", 0, errors.New("empty home")
				}
			} else {
				n, err := strconv.Atoi(args[i])
				if err != nil || n < 1 || n > 36500 {
					return "", 0, errors.New("invalid days (1..36500)")
				}
				days = n
			}
		default:
			return "", 0, errors.New("usage: tool-shapes --home path --days count --json")
		}
	}
	return home, days, nil
}
func main() {
	home, days, err := options(os.Args[1:])
	var r *report
	if err == nil {
		r, err = measure(home, days, time.Now())
	}
	if err != nil {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"error": err.Error()})
		os.Exit(1)
	}
	if err = json.NewEncoder(os.Stdout).Encode(r); err != nil {
		fmt.Fprintln(os.Stderr, `{"error":"cannot write report"}`)
		os.Exit(1)
	}
}
