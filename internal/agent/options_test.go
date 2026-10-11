package agent

import (
	"encoding/json"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Every option must be settable from every entry point. The flags and the MCP
// schema are derived from the struct, so this test is what makes forgetting
// one impossible rather than merely unlikely.
func TestEveryOptionHasAFlagAndASchemaEntry(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	var o Options
	BindFlags(fs, &o)
	schema := Options{}.Schema()

	typ := reflect.TypeOf(Options{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Tag.Get("desc") == "" {
			t.Errorf("%s has no desc tag", f.Name)
		}
		if fl := fs.Lookup(flagName(f)); fl == nil {
			t.Errorf("%s has no flag --%s", f.Name, flagName(f))
		} else if fl.Usage != f.Tag.Get("desc") {
			t.Errorf("--%s help differs from the desc tag", flagName(f))
		}
		name := jsonName(f)
		p, ok := schema[name].(map[string]any)
		if !ok {
			t.Errorf("%s has no schema entry %q", f.Name, name)
			continue
		}
		if p["description"] != f.Tag.Get("desc") || p["type"] == nil {
			t.Errorf("schema %q = %v", name, p)
		}
	}
	if len(schema) != typ.NumField() {
		t.Errorf("schema has %d entries for %d fields", len(schema), typ.NumField())
	}
}

func TestFlagsSetTheOptions(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := Options{Mode: ModeAsk} // the caller's default shows through
	BindFlags(fs, &o)
	err := fs.Parse([]string{"--model", "claude-sonnet-5", "--mode", "yolo", "--max-turns", "7", "--effort", "low",
		"--mcp", "a.json", "--mcp", "b.json", "--no-instructions", "--no-context-edits", "--allow-dangerous",
		"--timeout", "90s", "--max-cost", "2.5", "--task-budget", "30000"})
	if err != nil {
		t.Fatal(err)
	}
	want := Options{Model: "claude-sonnet-5", Mode: ModeYolo, MaxTurns: 7, Effort: "low", MCP: []string{"a.json", "b.json"},
		NoInstructions: true, NoContextEdits: true, AllowDangerous: true,
		Timeout: Duration(90 * time.Second), MaxCostUSD: 2.5, TaskBudget: 30000}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("parsed = %+v\nwant     %+v", o, want)
	}

	fs = flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o = Options{Mode: ModeAsk}
	BindFlags(fs, &o)
	if err := fs.Parse(nil); err != nil || o.Mode != ModeAsk {
		t.Errorf("an untouched flag keeps the default: mode = %q err=%v", o.Mode, err)
	}
	for _, bad := range [][]string{{"--mode", "weird"}, {"--timeout", "soon"}} {
		fs = flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		BindFlags(fs, &o)
		if err := fs.Parse(bad); err == nil {
			t.Errorf("%v should be refused by the parser", bad)
		}
	}
}

func TestOverInheritsWhatIsUnset(t *testing.T) {
	base := Options{Model: "m0", Mode: ModeAuto, MaxTurns: 5, Effort: "high", MCP: []string{"s.json"},
		NoInstructions: true, Timeout: Duration(time.Minute), MaxCostUSD: 1, TaskBudget: 20000}
	task := Options{Model: "m1", AllowDangerous: true, MaxCostUSD: 0.5}
	got := task.Over(base)
	want := Options{Model: "m1", Mode: ModeAuto, MaxTurns: 5, Effort: "high", MCP: []string{"s.json"},
		NoInstructions: true, AllowDangerous: true, Timeout: Duration(time.Minute), MaxCostUSD: 0.5, TaskBudget: 20000}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Over = %+v\nwant   %+v", got, want)
	}
	// A switch cannot be turned back off, and an empty list is a choice.
	if !(Options{}).Over(Options{NoContextEdits: true}).NoContextEdits {
		t.Error("a task cannot turn a spec-wide switch off")
	}
	if got := (Options{MCP: []string{}}).Over(base); len(got.MCP) != 0 || got.MCP == nil {
		t.Errorf("an empty list must stay empty, got %v", got.MCP)
	}
}

func TestValidate(t *testing.T) {
	if err := (Options{}).Validate(); err != nil {
		t.Errorf("empty options are valid: %v", err)
	}
	if err := (Options{Mode: ModeYolo, Timeout: Duration(time.Minute), MaxCostUSD: 2.5, TaskBudget: 20000}).Validate(); err != nil {
		t.Errorf("valid options refused: %v", err)
	}
	for _, tc := range []struct {
		o    Options
		want string
	}{
		{Options{Mode: "weird"}, "unknown mode"},
		{Options{Timeout: Duration(-time.Second)}, "negative"},
		{Options{MaxCostUSD: -1}, "negative"},
		{Options{TaskBudget: 100}, "20000"},
	} {
		if err := tc.o.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err = %v, want %q", tc.o, err, tc.want)
		}
	}
}

func TestValidateAcceptsOnlyKnownEffortLevels(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		if err := (Options{Effort: effort}).Validate(); err != nil {
			t.Errorf("effort %q should be accepted: %v", effort, err)
		}
	}
	for _, effort := range []string{"turbo", "HIGH"} {
		err := (Options{Effort: effort}).Validate()
		if err == nil {
			t.Errorf("effort %q should be refused", effort)
			continue
		}
		if !strings.Contains(err.Error(), `"`+effort+`"`) || !strings.Contains(err.Error(), "low, medium, high, xhigh, max") {
			t.Errorf("error must name the rejected effort and allowed levels: %v", err)
		}
	}
}

func TestDurationJSON(t *testing.T) {
	type doc struct {
		Timeout Duration `json:"timeout,omitempty"`
	}
	for _, tc := range []struct {
		in   string
		want Duration
	}{
		{`{"timeout":"20m"}`, Duration(20 * time.Minute)},
		{`{"timeout":"1h30m"}`, Duration(90 * time.Minute)},
		{`{"timeout":null}`, 0}, // absent, as a generated document spells it
		{`{}`, 0},
	} {
		var d doc
		if err := json.Unmarshal([]byte(tc.in), &d); err != nil || d.Timeout != tc.want {
			t.Errorf("%s: got %s err=%v, want %s", tc.in, d.Timeout, err, tc.want)
		}
	}
	var d doc
	if err := json.Unmarshal([]byte(`{"timeout":"soon"}`), &d); err == nil || !strings.Contains(err.Error(), "20m") {
		t.Errorf("a bad duration should say what it wants: %v", err)
	}
	// A bare number is refused: 300 meaning seconds would have run as 300ns.
	if err := json.Unmarshal([]byte(`{"timeout":300}`), &d); err == nil || !strings.Contains(err.Error(), "20m") {
		t.Errorf("a number must be refused, not read as nanoseconds: %v", err)
	}
	out, _ := json.Marshal(doc{Timeout: Duration(20 * time.Minute)})
	if string(out) != `{"timeout":"20m0s"}` {
		t.Errorf("marshal = %s", out)
	}
	if out, _ := json.Marshal(doc{}); string(out) != `{}` {
		t.Errorf("zero must be omitted: %s", out)
	}
}

// A run record written before Options carried the timeout as timeout_ns; it
// must still load with the same value.
func TestTaskReadsLegacyTimeoutNS(t *testing.T) {
	var task Task
	if err := json.Unmarshal([]byte(`{"prompt":"p","dir":"/x","mode":"ask","timeout_ns":90000000000}`), &task); err != nil {
		t.Fatal(err)
	}
	if task.Timeout != Duration(90*time.Second) || task.Mode != ModeAsk || task.Prompt != "p" {
		t.Errorf("legacy record = %+v", task)
	}
	out, err := json.Marshal(Task{Prompt: "p", Dir: "/x", Options: Options{Mode: ModeAsk, Timeout: Duration(time.Minute)}})
	if err != nil || !strings.Contains(string(out), `"timeout":"1m0s"`) || strings.Contains(string(out), "timeout_ns") {
		t.Errorf("a new record writes timeout as a duration: %s err=%v", out, err)
	}
	if !strings.Contains(string(out), `"prompt":"p"`) || !strings.Contains(string(out), `"mode":"ask"`) {
		t.Errorf("the task's own fields and the options must both be written: %s", out)
	}
}
