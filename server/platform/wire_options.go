package platform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Mode string

const (
	ModeReadOnly Mode = "readonly" // read and search only
	ModeAsk      Mode = "ask"      // reads auto-approved; writes and commands need approval
	ModeAuto     Mode = "auto"     // reads and file edits auto-approved; commands need approval
	ModeYolo     Mode = "yolo"     // everything auto-approved
)

func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case ModeReadOnly, ModeAsk, ModeAuto, ModeYolo:
		return m, nil
	}
	return "", fmt.Errorf("unknown mode %q (want readonly, ask, auto, yolo)", s)
}

var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

type Duration time.Duration

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*d = 0 // absent, as a generated spec or call may spell it
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("duration %s: want a string like \"20m\"", data)
	}
	return d.Set(s)
}

// Set parses Duration as a flag.Value.
func (d *Duration) Set(s string) error {
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is not a duration like 20m", s)
	}
	*d = Duration(v)
	return nil
}

// CompactWindow is auto or 100000-1000000; records store the count as a number.
type CompactWindow string

const minCompactWindow, maxCompactWindow = 100000, 1000000

func (w CompactWindow) Check() error {
	if w == "" || w == "auto" {
		return nil
	}
	if n, err := strconv.Atoi(string(w)); err == nil && strconv.Itoa(n) == string(w) && n >= minCompactWindow && n <= maxCompactWindow {
		return nil
	}
	return fmt.Errorf("compact_window %q: want a plain decimal integer from %d to %d (no sign or leading zeros), or \"auto\"", string(w), minCompactWindow, maxCompactWindow)
}

func (w CompactWindow) String() string { return string(w) }

// Set validates CompactWindow as a flag.Value.
func (w *CompactWindow) Set(s string) error {
	if err := CompactWindow(s).Check(); err != nil {
		return err
	}
	*w = CompactWindow(s)
	return nil
}

func (w CompactWindow) MarshalJSON() ([]byte, error) {
	if w == "auto" {
		return json.Marshal("auto")
	}
	return []byte(w), nil
}

func (w *CompactWindow) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*w = ""
		return nil
	}
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		*w = CompactWindow(strconv.Itoa(n))
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil || s == "" {
		return fmt.Errorf("compact_window %s: want an integer from %d to %d, or \"auto\"", data, minCompactWindow, maxCompactWindow)
	}
	return w.Set(s)
}

type JobOptions struct {
	ShapeTools        bool   `json:"shape_tools,omitempty"`
	Retrieval         string `json:"retrieval,omitempty"`
	RetrievalTenant   string `json:"retrieval_tenant,omitempty"`
	RetrievalProject  string `json:"retrieval_project,omitempty"`
	RetrievalRevision string `json:"retrieval_revision,omitempty"`
	RetrievalVersion  string `json:"retrieval_version,omitempty"`

	Model              string        `json:"model,omitempty"`
	Mode               Mode          `json:"mode,omitempty"`
	MaxTurns           int           `json:"max_turns,omitempty"`
	Effort             string        `json:"effort,omitempty"`
	MCP                []string      `json:"mcp,omitempty"`
	NoInstructions     bool          `json:"no_instructions,omitempty"`
	NoContextEdits     bool          `json:"no_context_edits,omitempty"`
	Lean               *bool         `json:"lean,omitempty"`
	Skills             *bool         `json:"skills,omitempty"`
	AppendSystemPrompt string        `json:"append_system_prompt,omitempty"`
	NoSkills           bool          `json:"no_skills,omitempty"`
	CompactWindow      CompactWindow `json:"compact_window,omitempty"`
	AllowDangerous     bool          `json:"allow_dangerous,omitempty"`
	InheritEnv         bool          `json:"inherit_env,omitempty"`
	Timeout            Duration      `json:"timeout,omitempty"`
	MaxCostUSD         float64       `json:"max_cost_usd,omitempty"`
	TaskBudget         int           `json:"task_budget,omitempty"`
	NoSwitch           bool          `json:"no_switch,omitempty"`
	Issue              string        `json:"issue,omitempty"`
	TakeOver           bool          `json:"take_over,omitempty"`
}

func (o JobOptions) ValidateShape() error {
	if o.Retrieval != "" && o.Retrieval != "off" && o.Retrieval != "optional" && o.Retrieval != "required" {
		return fmt.Errorf("invalid retrieval mode")
	}
	if o.Retrieval != "" && o.Retrieval != "off" && (o.RetrievalTenant == "" || o.RetrievalProject == "" || o.RetrievalRevision == "" || o.Model == "") {
		return fmt.Errorf("retrieval requires explicit tenant, project, revision and model")
	}
	if o.Mode != "" {
		if _, err := ParseMode(string(o.Mode)); err != nil {
			return err
		}
	}
	if o.Effort != "" && !slices.Contains(EffortLevels, o.Effort) {
		return fmt.Errorf("unknown effort %q (want %s)", o.Effort, strings.Join(EffortLevels, ", "))
	}
	// Windows escapes each quote and backslash on the command line, so they count twice.
	if n := len(o.AppendSystemPrompt) + strings.Count(o.AppendSystemPrompt, `"`) + strings.Count(o.AppendSystemPrompt, `\`); n > MaxAppendSystemPrompt {
		return fmt.Errorf("append_system_prompt takes %d bytes on a command line; at most %d fit", n, MaxAppendSystemPrompt)
	}
	if strings.ContainsRune(o.AppendSystemPrompt, 0) {
		return fmt.Errorf("append_system_prompt holds a NUL byte, which no command line can carry")
	}
	if err := o.CompactWindow.Check(); err != nil {
		return err
	}
	if math.IsNaN(o.MaxCostUSD) || math.IsInf(o.MaxCostUSD, 0) {
		return fmt.Errorf("max cost must be finite")
	}
	return ValidateLimits(time.Duration(o.Timeout), o.MaxCostUSD, o.TaskBudget)
}

// MaxAppendSystemPrompt reserves Windows command-line space.
const MaxAppendSystemPrompt = 16 << 10

const MinTaskBudget = 20000

// ValidateLimits rejects unsupported execution limits.
func ValidateLimits(timeout time.Duration, maxCostUSD float64, taskBudget int) error {
	if timeout < 0 {
		return fmt.Errorf("timeout %s is negative", timeout)
	}
	if maxCostUSD < 0 {
		return fmt.Errorf("max cost %.2f is negative", maxCostUSD)
	}
	if taskBudget != 0 && taskBudget < MinTaskBudget {
		return fmt.Errorf("task budget %d is below the API minimum of %d tokens", taskBudget, MinTaskBudget)
	}
	return nil
}

func (o *JobOptions) UnmarshalJSON(data []byte) error {
	type plain JobOptions
	v := plain(*o)
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return err
	}
	*o = JobOptions(v)
	return nil
}

func decodeStoredOptions(data []byte) (JobOptions, error) {
	type plain JobOptions
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return JobOptions{}, err
	}
	if v.Mode != "" {
		if _, err := ParseMode(string(v.Mode)); err != nil {
			return JobOptions{}, err
		}
	}
	return JobOptions(v), nil
}
