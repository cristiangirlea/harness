package agent

import (
	"errors"
	"flag"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/cristiangirlea/harness/internal/kernel"
	"github.com/cristiangirlea/harness/internal/tools"
)

// Options shares CLI, batch, and MCP configuration.
type Options = kernel.Options

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

// Bool returns a pointer to v, for an option that is set or left unset.
func Bool(v bool) *bool { return &v }

// ValidateOptions checks the option shape and guarded-run restrictions.
func ValidateOptions(o Options) error {
	// Guard errors precede execution limit errors.
	shape := o
	shape.Timeout, shape.MaxCostUSD, shape.TaskBudget = 0, 0, 0
	if err := shape.ValidateShape(); err != nil {
		return err
	}
	if o.AllowDangerous && tools.Guarded() {
		return ErrGuardedRun
	}
	if o.InheritEnv && tools.Guarded() {
		return ErrGuardedInherit
	}
	return o.ValidateShape()
}

// MaxAppendSystemPrompt reserves Windows command-line space.
const MaxAppendSystemPrompt = kernel.MaxAppendSystemPrompt

// ErrGuardedRun reserves lifting the guard for the operator; its text names no variable because the model reads it.
var ErrGuardedRun = errors.New("allow_dangerous is refused inside a guarded run: only the operator can lift the guard")

// ErrGuardedInherit refuses inherit_env inside a guarded run: it would hand the agent's runs the operator's whole environment, which only the operator may do.
var ErrGuardedInherit = errors.New("inherit_env is refused inside a guarded run: only the operator can give an agent their environment")

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
	return strings.ReplaceAll(kernel.JSONName(f), "_", "-")
}

var EffortLevels = kernel.EffortLevels
var Modes = kernel.Modes

type Duration = kernel.Duration
type CompactWindow = kernel.CompactWindow

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

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }
