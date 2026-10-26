package agent

import (
	"encoding/json"
	"flag"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestShapeToolsOption(t *testing.T) {
	var o Options
	if err := json.Unmarshal([]byte(`{}`), &o); err != nil || o.ShapeTools {
		t.Fatal("old records must leave shaping off")
	}
	fs := flag.NewFlagSet("shape", flag.ContinueOnError)
	BindFlags(fs, &o)
	if err := fs.Parse([]string{"--shape-tools"}); err != nil || !o.ShapeTools {
		t.Fatalf("shape flag: %v", err)
	}
	b, err := json.Marshal(o)
	if err != nil || !strings.Contains(string(b), `"shape_tools":true`) {
		t.Fatalf("record: %s %v", b, err)
	}
	if b, err := json.Marshal(Options{}); err != nil || strings.Contains(string(b), "shape_tools") {
		t.Fatalf("default record: %s %v", b, err)
	}
	if !(Options{}).Over(o).ShapeTools {
		t.Fatal("batch option did not inherit")
	}
}

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
		"--mcp", "a.json", "--mcp", "b.json", "--no-instructions", "--no-context-edits", "--lean", "--compact-window", "300000", "--allow-dangerous",
		"--timeout", "90s", "--max-cost", "2.5", "--task-budget", "30000", "--append-system-prompt", "be brief", "--no-skills"})
	if err != nil {
		t.Fatal(err)
	}
	want := Options{Model: "claude-sonnet-5", Mode: ModeYolo, MaxTurns: 7, Effort: "low", MCP: []string{"a.json", "b.json"},
		NoInstructions: true, NoContextEdits: true, Lean: Bool(true), CompactWindow: "300000", AllowDangerous: true,
		Timeout: Duration(90 * time.Second), MaxCostUSD: 2.5, TaskBudget: 30000, AppendSystemPrompt: "be brief", NoSkills: true}
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

func TestIssueOptionsComeFromFlagsAndATaskIssueWinsOverTheSpec(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o Options
	BindFlags(fs, &o)
	if err := fs.Parse([]string{"--issue", "o/n#12", "--take-over"}); err != nil || o.Issue != "o/n#12" || !o.TakeOver {
		t.Fatalf("flags = %+v, %v", o, err)
	}
	b, _ := json.Marshal(o)
	if !strings.Contains(string(b), `"issue":"o/n#12"`) || !strings.Contains(string(b), `"take_over":true`) {
		t.Errorf("record = %s", b)
	}
	if b, _ := json.Marshal(Options{}); strings.Contains(string(b), "issue") || strings.Contains(string(b), "take_over") {
		t.Errorf("unset issue options must be omitted: %s", b)
	}
	spec := Options{Issue: "o/n#1", TakeOver: true}
	if got := (Options{Issue: "o/n#2"}).Over(spec); got.Issue != "o/n#2" || !got.TakeOver {
		t.Errorf("a task's issue must win and take_over only enable: %+v", got)
	}
	if got := (Options{}).Over(Options{Issue: "o/n#1"}); got.Issue != "o/n#1" || got.TakeOver {
		t.Errorf("an unset issue inherits the spec's: %+v", got)
	}
}

// Lean has three states: unset takes the entry point's default (on in a
// batch, off for hrn run), and an explicit false is honoured, also over a
// spec that turned it on.
func TestLeanIsTriState(t *testing.T) {
	for _, tc := range []struct {
		json string
		want *bool
	}{{`{}`, nil}, {`{"lean":false}`, Bool(false)}, {`{"lean":true}`, Bool(true)}} {
		var o Options
		if err := json.Unmarshal([]byte(tc.json), &o); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(o.Lean, tc.want) {
			t.Errorf("%s: Lean = %v, want %v", tc.json, o.Lean, tc.want)
		}
	}
	if got := (Options{Lean: Bool(false)}).Over(Options{Lean: Bool(true)}); got.Lean == nil || *got.Lean {
		t.Errorf("a task's lean false must win over the spec's true: %v", got.Lean)
	}
	if got := (Options{}).Over(Options{Lean: Bool(true)}); !got.IsLean() {
		t.Error("an unset lean must inherit the spec's")
	}
	if b, _ := json.Marshal(Options{Lean: Bool(false)}); !strings.Contains(string(b), `"lean":false`) {
		t.Errorf("an explicit false must be recorded: %s", b)
	}
	for _, tc := range []struct {
		args []string
		want *bool
	}{{nil, nil}, {[]string{"--lean"}, Bool(true)}, {[]string{"--lean=false"}, Bool(false)}} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		var o Options
		BindFlags(fs, &o)
		if err := fs.Parse(tc.args); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(o.Lean, tc.want) {
			t.Errorf("%v: Lean = %v, want %v", tc.args, o.Lean, tc.want)
		}
	}
}

// compact_window is an integer from 100000 to 1000000 or "auto", the values
// claude --autocompact accepts, checked before a run starts.
func TestCompactWindow(t *testing.T) {
	for _, tc := range []struct {
		json, want string
	}{{`{}`, ""}, {`{"compact_window":200000}`, "200000"}, {`{"compact_window":"200000"}`, "200000"},
		{`{"compact_window":"100000"}`, "100000"}, {`{"compact_window":"1000000"}`, "1000000"},
		{`{"compact_window":"auto"}`, "auto"}} {
		var o Options
		if err := json.Unmarshal([]byte(tc.json), &o); err != nil {
			t.Fatalf("%s: %v", tc.json, err)
		}
		if string(o.CompactWindow) != tc.want {
			t.Errorf("%s: CompactWindow = %q, want %q", tc.json, o.CompactWindow, tc.want)
		}
		if err := o.Validate(); err != nil {
			t.Errorf("%s: %v", tc.json, err)
		}
		b, _ := json.Marshal(o)
		var back Options
		if err := json.Unmarshal(b, &back); err != nil || back.CompactWindow != o.CompactWindow {
			t.Errorf("%s: round trip through %s gives %q (%v)", tc.json, b, back.CompactWindow, err)
		}
	}
	if b, _ := json.Marshal(Options{CompactWindow: "200000"}); !strings.Contains(string(b), `"compact_window":200000`) {
		t.Errorf("a window is written as a number: %s", b)
	}
	for _, bad := range []string{`{"compact_window":"200k"}`, `{"compact_window":true}`, `{"compact_window":2.5e5}`,
		`{"compact_window":"99999"}`, `{"compact_window":"1000001"}`, `{"compact_window":"0200000"}`,
		`{"compact_window":"+200000"}`, `{"compact_window":"2.5e5"}`, `{"compact_window":""}`} {
		var o Options
		if err := json.Unmarshal([]byte(bad), &o); err == nil {
			t.Errorf("%s must be refused, got %q", bad, o.CompactWindow)
		}
	}
	for _, v := range []string{"100000", "1000000"} {
		if err := (Options{CompactWindow: CompactWindow(v)}).Validate(); err != nil {
			t.Errorf("%s is in range: %v", v, err)
		}
	}
	for _, v := range []string{"99999", "1000001", "0", "-5", "200k", "AUTO", "0200000", "+200000"} {
		if err := (Options{CompactWindow: CompactWindow(v)}).Validate(); err == nil || !strings.Contains(err.Error(), "compact_window") {
			t.Errorf("%s must be refused naming compact_window, got %v", v, err)
		}
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var o Options
		BindFlags(fs, &o)
		if err := fs.Parse([]string{"--compact-window", v}); err == nil {
			t.Errorf("--compact-window %s must be refused", v)
		}
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
		{Options{MaxCostUSD: math.NaN()}, "finite"},
		{Options{MaxCostUSD: math.Inf(1)}, "finite"},
		{Options{MaxCostUSD: math.Inf(-1)}, "finite"},
		{Options{TaskBudget: 100}, "20000"},
		{Options{AppendSystemPrompt: strings.Repeat("x", MaxAppendSystemPrompt+1)}, "at most 16384 fit"},
		{Options{AppendSystemPrompt: strings.Repeat(`"`, MaxAppendSystemPrompt/2+1)}, "at most 16384 fit"},
		{Options{AppendSystemPrompt: strings.Repeat(`\`, MaxAppendSystemPrompt/2+1)}, "at most 16384 fit"},
		{Options{AppendSystemPrompt: "a\x00b"}, "NUL"},
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
