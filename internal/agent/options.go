package agent

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/tools"
)

// Options is everything a caller can set on a run besides the prompt and the
// workspace. It is one struct so that the run flags, a batch spec and its
// tasks, and the MCP run tool all take the same fields from the same place: an
// option added here reaches every entry point, and options_test.go fails if
// one of them stops picking it up.
//
// Each field carries a json tag (its name in run records, batch specs and MCP
// calls), a desc tag (the one-line help the flag and the MCP schema show) and,
// where the flag is not the json name with dashes, a flag tag.
type Options struct {
	Retrieval         string `json:"retrieval,omitempty" desc:"Project retrieval: off | optional | required (default off)."`
	RetrievalTenant   string `json:"retrieval_tenant,omitempty" flag:"retrieval-tenant" desc:"Authorized retrieval tenant ID; no credentials."`
	RetrievalProject  string `json:"retrieval_project,omitempty" flag:"retrieval-project" desc:"Authorized retrieval project ID."`
	RetrievalRevision string `json:"retrieval_revision,omitempty" flag:"retrieval-revision" desc:"Exact repository revision for approved knowledge."`
	RetrievalVersion  string `json:"retrieval_version,omitempty" flag:"retrieval-version" desc:"Pin an approved knowledge version; empty resolves active once."`

	Model    string `json:"model,omitempty" desc:"Model id for the backend (api default: claude-opus-5)."`
	Mode     Mode   `json:"mode,omitempty" desc:"Permission mode: readonly | ask | auto | yolo."`
	MaxTurns int    `json:"max_turns,omitempty" flag:"max-turns" desc:"Stop after this many model turns (api default 50)."`
	Effort   string `json:"effort,omitempty" desc:"Reasoning effort for api, codex and claude: low | medium | high | xhigh | max; empty uses the backend default."`

	// MCP lists config files (Claude Code .mcp.json format) whose stdio
	// servers are offered as tools by the api backend and passed through to
	// the claude CLI. Absolute paths once a caller has resolved them.
	MCP []string `json:"mcp,omitempty" desc:"MCP config files, in Claude Code's .mcp.json format."`

	// NoInstructions skips the project instruction files (HRN.md, AGENTS.md,
	// CLAUDE.md) that the api backend otherwise puts in its system prompt.
	NoInstructions bool `json:"no_instructions,omitempty" desc:"Skip the project instruction files HRN.md, AGENTS.md and CLAUDE.md (api backend)."`

	// NoContextEdits turns off server-side context management, which the api
	// backend asks for by default so old tool results are cleared server-side
	// instead of being resent every turn.
	NoContextEdits bool `json:"no_context_edits,omitempty" desc:"Keep every tool result in the request instead of letting the server clear old ones (api backend)."`

	// Lean starts the claude CLI with only this run's MCP configs, so the
	// operator's other MCP servers stay out of every turn's context. Settings,
	// rules, hooks, plugins and skills still load. The guard is unchanged.
	// Unset (nil) takes the entry point's default: on for a batch or eval
	// task on a backend that applies it, off for hrn run and the MCP run tool;
	// an explicit false is kept, also over a spec's true.
	Lean *bool `json:"lean,omitempty" desc:"Start the claude CLI with only this run's --mcp servers: none of your other MCP servers, plugin servers or claude.ai connectors; your settings, rules, plugins and skills still load (claude backend)."`

	// CompactWindow is the context size, in tokens, at which the claude CLI
	// compacts the conversation (--autocompact): 100000 to 1000000, or
	// "auto". Unset keeps Claude Code's own threshold, which on a 1M-window
	// model is close to the whole window (#204).
	CompactWindow CompactWindow `json:"compact_window,omitempty" desc:"Compact the claude CLI's conversation at this many tokens of context: 100000 to 1000000, or auto (claude backend)."`

	// AllowDangerous lifts the guard: the api backend's command guard, which
	// otherwise refuses a few command lines (git push, sudo, rm -rf /, curl |
	// sh), and the guarded environment every backend runs in (tools.GuardEnv).
	AllowDangerous bool `json:"allow_dangerous,omitempty" desc:"Lift the guard: let agents push, merge and deploy with your credentials, and let run_command run sudo, rm -rf / and the like."`

	// Timeout ends the run when it passes; zero means no deadline.
	Timeout Duration `json:"timeout,omitempty" desc:"Stop the run after this long, e.g. 20m."`

	// MaxCostUSD stops the api loop before a turn that would take the
	// estimated cost past it. The CLI backends cannot enforce it.
	MaxCostUSD float64 `json:"max_cost_usd,omitempty" flag:"max-cost" desc:"Stop the api loop before a turn that would pass this estimated cost, in USD."`

	// TaskBudget is the api backend's token ceiling for the whole task: the
	// model paces itself and finishes rather than being cut off.
	TaskBudget int `json:"task_budget,omitempty" desc:"Token ceiling the api backend gives the model to pace itself (min 20000)."`

	// NoSwitch keeps a run on its backend when that backend has reached its
	// plan's usage limit. Otherwise claude and codex carry on each other's
	// work (internal/limits).
	NoSwitch bool `json:"no_switch,omitempty" desc:"Stay on this backend when it reaches its plan's usage limit, instead of carrying on on the other of claude and codex."`
}

// IsLean reports whether lean is on: set, and true.
func (o Options) IsLean() bool { return o.Lean != nil && *o.Lean }

// Bool returns a pointer to v, for an option that is set or left unset.
func Bool(v bool) *bool { return &v }

// Validate rejects what the API or hrn would refuse, where the values are typed
// rather than after an agent has started. An empty mode passes: each entry
// point has its own default.
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
	if err := o.CompactWindow.check(); err != nil {
		return err
	}
	if o.AllowDangerous && tools.Guarded() {
		return ErrGuardedRun
	}
	return ValidateLimits(time.Duration(o.Timeout), o.MaxCostUSD, o.TaskBudget)
}

// ErrGuardedRun refuses allow_dangerous to an hrn started inside a guarded
// run (tools.GuardedVar is set): from an agent's shell, or from an MCP server
// its CLI started with its environment. Lifting the guard is the operator's
// call. The text names no variable: the model reads it.
var ErrGuardedRun = errors.New("allow_dangerous is refused inside a guarded run: only the operator can lift the guard")

// Over returns o with every unset field taken from base: a task's options over
// its spec's. A switch left off inherits the spec's, so a task can turn one on
// but not back off, except lean, which is unset only when nil; a list left nil
// inherits, and an empty list stays empty.
func (o Options) Over(base Options) Options {
	ov, bv := reflect.ValueOf(&o).Elem(), reflect.ValueOf(base)
	for i := 0; i < ov.NumField(); i++ {
		if f := ov.Field(i); f.IsZero() {
			f.Set(bv.Field(i))
		}
	}
	return o
}

// Schema describes the options as JSON-schema properties, for the MCP run
// tool, from the same fields and descriptions the flags use.
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
			p["type"] = []any{"integer", "string"} // a token count, or "auto"
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

// BindFlags registers one flag per option on fs, bound to o. The flag is named
// by the field's flag tag, or its json name with dashes. Whatever o holds when
// this is called shows as the default, so a caller sets its defaults first.
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

// flagName is the flag an Options field is set with.
func flagName(f reflect.StructField) string {
	if name := f.Tag.Get("flag"); name != "" {
		return name
	}
	return strings.ReplaceAll(jsonName(f), "_", "-")
}

func jsonName(f reflect.StructField) string {
	return strings.Split(f.Tag.Get("json"), ",")[0]
}

// EffortLevels are the reasoning efforts a caller may ask for. The api backend
// sends the level as is, codex and claude pass it to their CLIs, and the other
// backends report it as not enforced. The names are exact: "HIGH" is refused.
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// Modes lists the permission modes, for the MCP schema and the flag help.
var Modes = []Mode{ModeReadOnly, ModeAsk, ModeAuto, ModeYolo}

func (m Mode) String() string { return string(m) }

// Set parses a flag value, so a *Mode is a flag.Value and a bad mode is
// refused by the flag parser itself.
func (m *Mode) Set(s string) error {
	v, err := ParseMode(s)
	if err != nil {
		return err
	}
	*m = v
	return nil
}

// Duration is a time.Duration that reads as "20m" in a spec, a flag or an MCP
// call and is written the same way in a run record. A bare number is refused:
// "timeout": 300 would otherwise be 300 nanoseconds, and the records that
// stored nanoseconds used their own key, timeout_ns, which Task still reads.
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

// Set parses a flag value, so a *Duration is a flag.Value.
func (d *Duration) Set(s string) error {
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is not a duration like 20m", s)
	}
	*d = Duration(v)
	return nil
}

// setBool is a boolean flag that stays nil until it is given, so that an
// absent flag and --x=false differ.
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

// CompactWindow is "auto" or a token count from 100000 to 1000000 in decimal,
// as claude --autocompact takes it. A spec or call writes the count as a
// number, and a record stores it the same way.
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

// Set parses a flag value, so a *CompactWindow is a flag.Value.
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
	if err := json.Unmarshal(data, &s); err != nil || s != "auto" {
		return fmt.Errorf("compact_window %s: want an integer from %d to %d, or \"auto\"", data, minCompactWindow, maxCompactWindow)
	}
	*w = "auto"
	return nil
}

// stringList collects a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }
