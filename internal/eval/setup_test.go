package eval

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// setupRun records a run in a workspace holding files, and returns its id.
func setupRun(t *testing.T, files map[string]string, task agent.Task) string {
	t.Helper()
	task.Dir = t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(task.Dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w, err := runlog.Create("api", task)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	return w.ID()
}

func TestSummarizeKeepsEachVariantsSetupAndSaysWhenItsCellsDiffer(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	spec := Spec{Matrix: []Variant{{Name: "same", Backend: "api"}, {Name: "mixed", Backend: "api"}, {Name: "old", Backend: "api"}}}
	spec.Tasks = []batch.Task{{ID: "a"}}
	cell := func(id, run string) batch.TaskResult {
		return batch.TaskResult{ID: id, Status: batch.StatusPass, Attempts: []batch.Attempt{{RunID: run, Result: agent.Result{Status: agent.StatusOK}}}}
	}
	guide := map[string]string{"AGENTS.md": "guide"}
	ownPrompt := agent.Task{}
	ownPrompt.AppendSystemPrompt = "a task's own system prompt"
	res := batch.Result{Tasks: []batch.TaskResult{
		cell("e.a.same.r1", setupRun(t, guide, agent.Task{})),
		cell("e.a.same.r2", setupRun(t, guide, ownPrompt)),
		cell("e.a.mixed.r1", setupRun(t, guide, agent.Task{})),
		cell("e.a.mixed.r2", setupRun(t, map[string]string{"AGENTS.md": "other"}, agent.Task{})),
		cell("e.a.old.r1", "20200101-000000-gone00"),
	}}
	rows := Summarize(spec, res, nil).Variants
	same, mixed, old := rows[0], rows[1], rows[2]
	if same.Setup == nil || same.Setup.Files["AGENTS.md"] == "" || same.SetupMixed || same.SetupTask != "a" {
		t.Errorf("same, whose cells differ only by an option = %+v, mixed %v", same.Setup, same.SetupMixed)
	}
	if mixed.Setup == nil || !mixed.SetupMixed {
		t.Errorf("mixed: setup %+v, mixed %v", mixed.Setup, mixed.SetupMixed)
	}
	if old.Setup != nil || old.SetupMixed {
		t.Errorf("a cell whose run is gone has no setup: %+v", old.Setup)
	}
}

func TestCompareFlagsASetupChangeBeyondTheVariantsOwnFields(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	guide := map[string]string{"AGENTS.md": "guide", "CLAUDE.md": "c"}
	plain := agent.Task{}
	briefed := agent.Task{}
	briefed.AppendSystemPrompt = "Be brief."
	load := func(id string) *agent.Setup {
		rec, err := runlog.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		return rec.Setup
	}
	base := load(setupRun(t, guide, plain))
	onlyOption := load(setupRun(t, guide, briefed))
	unrelated := load(setupRun(t, map[string]string{"AGENTS.md": "edited", "HRN.md": "new"}, briefed))

	side := func(id string, s *agent.Setup) Record {
		return Record{ID: id, Table: Table{Variants: []VariantRow{{Name: "v", Attempts: 1, Setup: s}}}}
	}
	if d := Compare(side("a", base), side("b", onlyOption)); len(d.SetupChanged) != 0 {
		t.Fatalf("only the variant's own option changed, yet flagged: %+v", d.SetupChanged)
	}
	d := Compare(side("a", base), side("b", unrelated))
	if len(d.SetupChanged) != 1 || d.SetupChanged[0].Variant != "v" {
		t.Fatalf("setup changed = %+v", d.SetupChanged)
	}
	if got := d.SetupChanged[0].Files; !slices.Equal(got, []string{"AGENTS.md", "CLAUDE.md", "HRN.md"}) {
		t.Errorf("files = %v", got)
	}
	// An old record on either side has nothing to compare.
	if d := Compare(side("a", nil), side("b", unrelated)); len(d.SetupChanged) != 0 {
		t.Errorf("a side without a setup was flagged: %+v", d.SetupChanged)
	}
}

func TestCompareNamesTheMCPPluginAndSkillChanges(t *testing.T) {
	a := &agent.Setup{BaseDigest: "1", MCP: []string{"docs"}, Plugins: []string{"p"}, Skills: []string{"check"}}
	b := &agent.Setup{BaseDigest: "2", MCP: []string{"docs", "web"}, Skills: []string{"deliver"}}
	side := func(id string, s *agent.Setup) Record {
		return Record{ID: id, Table: Table{Variants: []VariantRow{{Name: "v", Setup: s}}}}
	}
	d := Compare(side("a", a), side("b", b))
	if len(d.SetupChanged) != 1 {
		t.Fatalf("setup changed = %+v", d.SetupChanged)
	}
	c := d.SetupChanged[0]
	if !slices.Equal(c.MCP, []string{"web"}) || !slices.Equal(c.Plugins, []string{"p"}) || !slices.Equal(c.Skills, []string{"check", "deliver"}) {
		t.Errorf("change = %+v", c)
	}
}

func TestCompareLeavesASetupChangeFromAnotherBackendToTheVariantNote(t *testing.T) {
	a := &agent.Setup{Backend: "codex", BaseDigest: "1", Files: map[string]string{"AGENTS.md": "x"}}
	b := &agent.Setup{Backend: "claude", BaseDigest: "2", Files: map[string]string{"CLAUDE.md": "y"}}
	side := func(id, backend string, s *agent.Setup) Record {
		return Record{ID: id, Spec: Spec{Matrix: []Variant{{Name: "v", Backend: backend}}},
			Table: Table{Variants: []VariantRow{{Name: "v", Setup: s}}}}
	}
	d := Compare(side("a", "codex", a), side("b", "claude", b))
	if len(d.SetupChanged) != 0 || !slices.Equal(d.VariantChanged, []string{"v"}) {
		t.Fatalf("setup changed %+v, variant changed %v", d.SetupChanged, d.VariantChanged)
	}
}

func TestSummarizeChecksMixedSetupsWithinEachTaskOnly(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	spec := Spec{Matrix: []Variant{{Name: "v", Backend: "api"}}}
	spec.Tasks = []batch.Task{{ID: "a"}, {ID: "b"}}
	cell := func(id, run string) batch.TaskResult {
		return batch.TaskResult{ID: id, Status: batch.StatusPass, Attempts: []batch.Attempt{{RunID: run}}}
	}
	res := batch.Result{Tasks: []batch.TaskResult{
		cell("e.a.v.r1", setupRun(t, map[string]string{"AGENTS.md": "one"}, agent.Task{})),
		cell("e.a.v.r2", setupRun(t, map[string]string{"AGENTS.md": "one"}, agent.Task{})),
		cell("e.b.v.r1", setupRun(t, map[string]string{"AGENTS.md": "two"}, agent.Task{})),
	}}
	if row := Summarize(spec, res, nil).Variants[0]; row.Setup == nil || row.SetupMixed {
		t.Fatalf("two tasks with their own setups, each repeated alike, read as mixed: %+v", row)
	}
}

func TestSummarizePutsTheVariantPromptPrefixInItsDigest(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	run := setupRun(t, map[string]string{"AGENTS.md": "a"}, agent.Task{})
	res := func(variant string) batch.Result {
		return batch.Result{Tasks: []batch.TaskResult{{ID: "e.a." + variant + ".r1", Status: batch.StatusPass, Attempts: []batch.Attempt{{RunID: run}}}}}
	}
	row := func(v Variant) VariantRow {
		spec := Spec{Matrix: []Variant{v}}
		spec.Tasks = []batch.Task{{ID: "a"}}
		return Summarize(spec, res(v.Key()), nil).Variants[0]
	}
	plain, briefed := row(Variant{Name: "v", Backend: "api"}), row(Variant{Name: "v", Backend: "api", PromptPrefix: "Read the test first."})
	if plain.Setup == nil || briefed.Setup == nil || plain.Setup.Digest == briefed.Setup.Digest || plain.Setup.BaseDigest != briefed.Setup.BaseDigest {
		t.Fatalf("plain %+v, briefed %+v", plain.Setup, briefed.Setup)
	}
}

func TestCompareNotesEveryVariantFieldThatChanged(t *testing.T) {
	for name, change := range map[string]func(*Variant){
		"lean":              func(v *Variant) { v.Lean = agent.Bool(true) },
		"lean false":        func(v *Variant) { v.Lean = agent.Bool(false) },
		"shape_tools":       func(v *Variant) { v.ShapeTools = true },
		"retrieval":         func(v *Variant) { v.Retrieval = "optional" },
		"retrieval_version": func(v *Variant) { v.RetrievalVersion = "v2" },
	} {
		before := Variant{Name: "v", Backend: "claude"}
		after := before
		change(&after)
		d := Compare(Record{ID: "a", Spec: Spec{Matrix: []Variant{before}}}, Record{ID: "b", Spec: Spec{Matrix: []Variant{after}}})
		if !slices.Equal(d.VariantChanged, []string{"v"}) {
			t.Errorf("%s: variant changed = %v", name, d.VariantChanged)
		}
	}
	same := Variant{Name: "v", Lean: agent.Bool(true)}
	other := Variant{Name: "v", Lean: agent.Bool(true)}
	if d := Compare(Record{Spec: Spec{Matrix: []Variant{same}}}, Record{Spec: Spec{Matrix: []Variant{other}}}); len(d.VariantChanged) != 0 {
		t.Errorf("two equal lean settings read as changed")
	}
}

func TestCompareNotesABackendChangeTheVariantDidNotName(t *testing.T) {
	side := func(id, backend string) Record {
		return Record{ID: id, Spec: Spec{Matrix: []Variant{{Name: "v"}}},
			Table: Table{Variants: []VariantRow{{Name: "v", Setup: &agent.Setup{Backend: backend, BaseDigest: backend}}}}}
	}
	d := Compare(side("a", "codex"), side("b", "claude"))
	if !slices.Equal(d.VariantChanged, []string{"v"}) || len(d.SetupChanged) != 0 {
		t.Fatalf("variant changed %v, setup changed %+v", d.VariantChanged, d.SetupChanged)
	}
}

func TestSummarizeTakesTheSetupFromTheFirstTaskOnly(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	spec := Spec{Matrix: []Variant{{Name: "v", Backend: "api"}}}
	spec.Tasks = []batch.Task{{ID: "a"}, {ID: "b"}}
	res := batch.Result{Tasks: []batch.TaskResult{
		{ID: "e.a.v.r1", Attempts: []batch.Attempt{{RunID: "20200101-000000-gone00"}}},
		{ID: "e.b.v.r1", Attempts: []batch.Attempt{{RunID: setupRun(t, map[string]string{"AGENTS.md": "b"}, agent.Task{})}}},
	}}
	if row := Summarize(spec, res, nil).Variants[0]; row.Setup != nil {
		t.Fatalf("a setup from task b would be compared with task a on the other side: %+v", row.Setup)
	}
}

func TestCompareSkipsTheSetupNoteWhenTheSpecsStartWithOtherTasks(t *testing.T) {
	side := func(id, task, digest string) Record {
		return Record{ID: id, Table: Table{Variants: []VariantRow{{Name: "v", SetupTask: task, Setup: &agent.Setup{BaseDigest: digest}}}}}
	}
	if d := Compare(side("a", "a", "1"), side("b", "b", "2")); len(d.SetupChanged) != 0 {
		t.Fatalf("setups of two different tasks were compared: %+v", d.SetupChanged)
	}
	if d := Compare(side("a", "a", "1"), side("b", "a", "2")); len(d.SetupChanged) != 1 {
		t.Fatalf("the same first task with another setup was not noted")
	}
}
