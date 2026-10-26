package agent

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/purpose"
	"github.com/cristiangirlea/harness/internal/route/table"
	"github.com/cristiangirlea/harness/internal/tools"
)

// Options shares CLI, batch, and MCP configuration.
type Options struct {
	ShapeTools        bool   `json:"shape_tools,omitempty" flag:"shape-tools" desc:"Summarise large terminal, log and test results and keep the original behind a handle (api backend; default off)."`
	Retrieval         string `json:"retrieval,omitempty" desc:"Project retrieval: off | optional | required (default off)."`
	RetrievalTenant   string `json:"retrieval_tenant,omitempty" flag:"retrieval-tenant" desc:"Authorized retrieval tenant ID; no credentials."`
	RetrievalProject  string `json:"retrieval_project,omitempty" flag:"retrieval-project" desc:"Authorized retrieval project ID."`
	RetrievalRevision string `json:"retrieval_revision,omitempty" flag:"retrieval-revision" desc:"Exact repository revision for approved knowledge."`
	RetrievalVersion  string `json:"retrieval_version,omitempty" flag:"retrieval-version" desc:"Pin an approved knowledge version; empty resolves active once."`

	Model    string `json:"model,omitempty" desc:"Model id for the backend (api default: claude-opus-5)."`
	Mode     Mode   `json:"mode,omitempty" desc:"Permission mode: readonly | ask | auto | yolo."`
	MaxTurns int    `json:"max_turns,omitempty" flag:"max-turns" desc:"Stop after this many model turns (api default 50)."`
	Effort   string `json:"effort,omitempty" desc:"Reasoning effort for api, codex and claude: low | medium | high | xhigh | max; empty uses the backend default."`

	// MCP contains resolved stdio-server config paths.
	MCP []string `json:"mcp,omitempty" desc:"MCP config files, in Claude Code's .mcp.json format."`

	// NoInstructions omits project instructions from the API prompt.
	NoInstructions bool `json:"no_instructions,omitempty" desc:"Skip the project instruction files HRN.md, AGENTS.md and CLAUDE.md (api backend)."`

	// NoContextEdits disables server-side clearing.
	NoContextEdits bool `json:"no_context_edits,omitempty" desc:"Keep every tool result in the request instead of letting the server clear old ones (api backend)."`

	// Lean limits MCP servers and Claude skills; nil enables it.
	Lean *bool `json:"lean,omitempty" desc:"Load only this run's --mcp servers, and on claude no skills (default on; claude and codex backends). false loads every MCP server, plugin and skill your CLI is set up with."`

	// Skills preserves Claude skills in lean runs.
	Skills *bool `json:"skills,omitempty" desc:"Keep the claude CLI's skills and slash commands in a lean run (default off; claude backend)."`

	// AppendSystemPrompt follows the backend's system prompt.
	AppendSystemPrompt string `json:"append_system_prompt,omitempty" desc:"Text added after the backend's own system prompt, such as a rule or tool guidance (api and claude backends; at most 16 KiB)."`

	// NoSkills disables Claude skills.
	NoSkills bool `json:"no_skills,omitempty" desc:"Start the claude CLI with skills turned off (--disable-slash-commands) (claude backend)."`

	// CompactWindow sets Claude's compaction threshold.
	CompactWindow CompactWindow `json:"compact_window,omitempty" desc:"Compact the claude CLI's conversation at this many tokens of context: 100000 to 1000000, or auto (claude backend)."`

	// AllowDangerous lifts command and environment guards.
	AllowDangerous bool `json:"allow_dangerous,omitempty" desc:"Lift the guard: let agents push, merge and deploy with your credentials, and let run_command run sudo, rm -rf / and the like."`

	// Timeout ends the run when it passes; zero means no deadline.
	Timeout Duration `json:"timeout,omitempty" desc:"Stop the run after this long, e.g. 20m."`

	// MaxCostUSD caps estimated API cost.
	MaxCostUSD float64 `json:"max_cost_usd,omitempty" flag:"max-cost" desc:"Stop the api loop before a turn that would pass this estimated cost, in USD."`

	// TaskBudget gives the model a task-wide token ceiling.
	TaskBudget int `json:"task_budget,omitempty" desc:"Token ceiling the api backend gives the model to pace itself (min 20000)."`

	// NoSwitch prevents continuation on a partner backend.
	NoSwitch bool `json:"no_switch,omitempty" desc:"Stay on this backend when it reaches its usage limit, instead of carrying on on an enabled partner (claude and codex pair by default)."`

	// Issue is leased for the run so that a second run for it is refused.
	Issue    string `json:"issue,omitempty" desc:"GitHub issue this run works on (owner/name#N, its URL, or #N for this repository's origin); taken as a lease so a second run for it is refused."`
	TakeOver bool   `json:"take_over,omitempty" flag:"take-over" desc:"Take the issue lease from its current holder."`
}

// Route fills an unspecified backend/model/effort from evidence.
func (o *Options) Route(declared, prompt string, backend *string, home string) (*table.RoutedBy, error) {
	routed, _, err := o.RouteWithNote(declared, prompt, backend, home)
	return routed, err
}

// RouteWithNote preserves why an explicit or invalid choice bypassed routing.
func (o *Options) RouteWithNote(declared, prompt string, backend *string, home string) (*table.RoutedBy, string, error) {
	if *backend != "" || o.Model != "" || o.Effort != "" {
		if *backend != "" {
			return nil, "backend " + *backend + " was named for this run", nil
		}
		if o.Model != "" && o.Effort != "" {
			return nil, "model and effort were named for this run", nil
		}
		if o.Model != "" {
			return nil, "model " + o.Model + " was named for this run", nil
		}
		return nil, "effort " + o.Effort + " was named for this run", nil
	}
	t, err := table.Load(home)
	if err != nil {
		return nil, "", err
	}
	resolved := purpose.Resolve(declared, prompt)
	category := resolved.Category
	r, ok := t.Pick(category)
	if !ok {
		return nil, "no route row for purpose " + category, nil
	}
	if r.Backend != "" {
		p, err := BackendPreferences(home)
		if err != nil {
			return nil, "", err
		}
		if !p.Enabled(r.Backend) {
			note := fmt.Sprintf("ignoring route for %s: backend %s is disabled", category, r.Backend)
			fmt.Fprintln(os.Stderr, "hrn: warning: "+note)
			return nil, note, nil
		}
		if _, err := Get(r.Backend); err != nil {
			note := fmt.Sprintf("ignoring route for %s: %v", category, err)
			fmt.Fprintln(os.Stderr, "hrn: warning: "+note)
			return nil, note, nil
		}
	}
	if err := (Options{Effort: r.Effort}).Validate(); err != nil {
		note := fmt.Sprintf("ignoring route for %s: %v", category, err)
		fmt.Fprintln(os.Stderr, "hrn: warning: "+note)
		return nil, note, nil
	}
	if r.Backend == "" && r.Model == "" && r.Effort == "" {
		return nil, "no route row for purpose " + category, nil
	}
	*backend, o.Model, o.Effort = r.Backend, r.Model, r.Effort
	return &table.RoutedBy{Category: category, Declared: resolved.Rule == "declared", Eval: r.Eval, Set: r.Set}, "", nil
}

// WithDefault adds the default backend to a route note, for a run that named none and had no route.
func WithDefault(note, backend string) string {
	d := "default backend " + backend + " was used"
	if note == "" {
		return d
	}
	return note + "; " + d
}

// ContinuationNote keeps a continuation's own reason, and adds the limit's reason whenever one was weighed.
func ContinuationNote(continues, switched string) string {
	if switched == "" {
		return ""
	}
	return "continues run " + continues + "; " + switched
}

// IsLean reports whether lean is on: unset or true.
func (o Options) IsLean() bool { return o.Lean == nil || *o.Lean }

// Bool returns a pointer to v, for an option that is set or left unset.
func Bool(v bool) *bool { return &v }

// Validate rejects unsupported options; empty mode permits caller defaults.
func (o Options) Validate() error {
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
	if err := o.CompactWindow.check(); err != nil {
		return err
	}
	if o.AllowDangerous && tools.Guarded() {
		return ErrGuardedRun
	}
	if math.IsNaN(o.MaxCostUSD) || math.IsInf(o.MaxCostUSD, 0) {
		return fmt.Errorf("max cost must be finite")
	}
	return ValidateLimits(time.Duration(o.Timeout), o.MaxCostUSD, o.TaskBudget)
}

// MaxAppendSystemPrompt reserves Windows command-line space.
const MaxAppendSystemPrompt = 16 << 10

// ErrGuardedRun reserves lifting the guard for the operator; its text names no variable because the model reads it.
var ErrGuardedRun = errors.New("allow_dangerous is refused inside a guarded run: only the operator can lift the guard")

// Over inherits unset values; non-nil pointers and lists override base.
func (o Options) Over(base Options) Options {
	ov, bv := reflect.ValueOf(&o).Elem(), reflect.ValueOf(base)
	for i := 0; i < ov.NumField(); i++ {
		if f := ov.Field(i); f.IsZero() {
			f.Set(bv.Field(i))
		}
	}
	return o
}

// Schema derives MCP properties from CLI option fields.
func (Options) Schema() map[string]any {
	props := map[string]any{}
	t := reflect.TypeOf(Options{})
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		p := map[string]any{"description": f.Tag.Get("desc")}
		switch f.Type {
		case reflect.TypeOf(Mode("")):
			p["type"] = "string"
			names := make([]any, 0, len(Modes))
			for _, m := range Modes {
				names = append(names, string(m))
			}
			p["enum"] = names
		case reflect.TypeOf(Duration(0)):
			p["type"] = "string"
		case reflect.TypeOf(CompactWindow("")):
			p["type"] = "string" // a decimal token count, or "auto"
		default:
			switch f.Type.Kind() {
			case reflect.String:
				p["type"] = "string"
			case reflect.Int:
				p["type"] = "integer"
			case reflect.Float64:
				p["type"] = "number"
			case reflect.Bool:
				p["type"] = "boolean"
			case reflect.Pointer:
				if f.Type.Elem().Kind() != reflect.Bool {
					panic(fmt.Sprintf("agent.Options.%s: no schema for %s", f.Name, f.Type))
				}
				p["type"] = "boolean"
			case reflect.Slice:
				p["type"] = "array"
				p["items"] = map[string]any{"type": "string"}
			default:
				panic(fmt.Sprintf("agent.Options.%s: no schema for %s", f.Name, f.Type))
			}
		}
		props[jsonName(f)] = p
	}
	return props
}

// BindFlags uses o's current values as defaults.
func BindFlags(fs *flag.FlagSet, o *Options) {
	v := reflect.ValueOf(o).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, desc := flagName(f), f.Tag.Get("desc")
		switch p := v.Field(i).Addr().Interface().(type) {
		case *string:
			fs.StringVar(p, name, *p, desc)
		case *int:
			fs.IntVar(p, name, *p, desc)
		case *float64:
			fs.Float64Var(p, name, *p, desc)
		case *bool:
			fs.BoolVar(p, name, *p, desc)
		case **bool:
			fs.Var(setBool{p}, name, desc)
		case *[]string:
			fs.Var((*stringList)(p), name, desc)
		case flag.Value:
			fs.Var(p, name, desc)
		default:
			panic(fmt.Sprintf("agent.Options.%s: no flag for type %s", f.Name, f.Type))
		}
	}
}

func flagName(f reflect.StructField) string {
	if name := f.Tag.Get("flag"); name != "" {
		return name
	}
	return strings.ReplaceAll(jsonName(f), "_", "-")
}

func jsonName(f reflect.StructField) string {
	return strings.Split(f.Tag.Get("json"), ",")[0]
}

// EffortLevels contains case-sensitive effort names.
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// Modes lists the permission modes, for the MCP schema and the flag help.
var Modes = []Mode{ModeReadOnly, ModeAsk, ModeAuto, ModeYolo}

func (m Mode) String() string { return string(m) }

// Set validates Mode as a flag.Value.
func (m *Mode) Set(s string) error {
	v, err := ParseMode(s)
	if err != nil {
		return err
	}
	*m = v
	return nil
}

// Duration stores durations as text to prevent nanosecond mistakes.
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

// Nil distinguishes an absent boolean flag from an explicit false.
type setBool struct{ p **bool }

func (b setBool) String() string {
	if b.p == nil || *b.p == nil {
		return ""
	}
	return strconv.FormatBool(**b.p)
}

func (b setBool) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	*b.p = &v
	return nil
}

func (setBool) IsBoolFlag() bool { return true }

// CompactWindow is auto or 100000-1000000; records store the count as a number.
type CompactWindow string

const minCompactWindow, maxCompactWindow = 100000, 1000000

func (w CompactWindow) check() error {
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
	if err := CompactWindow(s).check(); err != nil {
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

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }
