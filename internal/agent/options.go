package agent

import (
	"encoding/json"
	"flag"
	"fmt"
	"reflect"
	"strings"
	"time"
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
	Model    string `json:"model,omitempty" desc:"Model id for the backend (api default: claude-opus-5)."`
	Mode     Mode   `json:"mode,omitempty" desc:"Permission mode: readonly | ask | auto | yolo."`
	MaxTurns int    `json:"max_turns,omitempty" flag:"max-turns" desc:"Stop after this many model turns (api default 50)."`
	Effort   string `json:"effort,omitempty" desc:"api backend only: low | medium | high | xhigh | max."`

	// MCP lists config files (Claude Code .mcp.json format) whose stdio
	// servers are offered as tools by the api backend and passed through to
	// the claude CLI. Absolute paths once a caller has resolved them.
	MCP []string `json:"mcp,omitempty" desc:"MCP config files, in Claude Code's .mcp.json format."`

	// NoInstructions skips the project instruction files (HX.md, AGENTS.md,
	// CLAUDE.md) that the api backend otherwise puts in its system prompt.
	NoInstructions bool `json:"no_instructions,omitempty" desc:"Skip the project instruction files HX.md, AGENTS.md and CLAUDE.md (api backend)."`

	// NoContextEdits turns off server-side context management, which the api
	// backend asks for by default so old tool results are cleared server-side
	// instead of being resent every turn.
	NoContextEdits bool `json:"no_context_edits,omitempty" desc:"Keep every tool result in the request instead of letting the server clear old ones (api backend)."`

	// AllowDangerous lifts the api backend's command guard, which otherwise
	// refuses a few command lines (git push, sudo, rm -rf /, curl | sh).
	AllowDangerous bool `json:"allow_dangerous,omitempty" desc:"Let run_command run git push, sudo, rm -rf / and the like (api backend)."`

	// Timeout ends the run when it passes; zero means no deadline.
	Timeout Duration `json:"timeout,omitempty" desc:"Stop the run after this long, e.g. 20m."`

	// MaxCostUSD stops the api loop before a turn that would take the
	// estimated cost past it. The CLI backends cannot enforce it.
	MaxCostUSD float64 `json:"max_cost_usd,omitempty" flag:"max-cost" desc:"Stop the api loop before a turn that would pass this estimated cost, in USD."`

	// TaskBudget is the api backend's token ceiling for the whole task: the
	// model paces itself and finishes rather than being cut off.
	TaskBudget int `json:"task_budget,omitempty" desc:"Token ceiling the api backend gives the model to pace itself (min 20000)."`
}

// Validate rejects what the API or hx would refuse, where the values are typed
// rather than after an agent has started. An empty mode passes: each entry
// point has its own default.
func (o Options) Validate() error {
	if o.Mode != "" {
		if _, err := ParseMode(string(o.Mode)); err != nil {
			return err
		}
	}
	return ValidateLimits(time.Duration(o.Timeout), o.MaxCostUSD, o.TaskBudget)
}

// Over returns o with every unset field taken from base: a task's options over
// its spec's. A switch left off inherits the spec's, so a task can turn one on
// but not back off; a list left nil inherits, and an empty list stays empty.
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

// stringList collects a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }
