package hrnmcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/kernel"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/prefs/prefstest"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestRunToolOffersOnlyEnabledBackendsAndRefusesDisabledOnes(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"codex", "claude"}}); err != nil {
		t.Fatal(err)
	}
	tool := runTool(t.TempDir(), false)
	props := tool.InputSchema["properties"].(map[string]any)
	if got := props["backend"].(map[string]any)["enum"]; !reflect.DeepEqual(got, []any{"codex", "claude"}) {
		t.Errorf("enum: %v", got)
	}
	// An unknown name cannot launch a real backend even before the refusal exists.
	result, err := tool.Handler(context.Background(), mcp.Call{Arguments: json.RawMessage(`{"prompt":"hello","backend":"disabled-test"}`)})
	want := "run: backend disabled-test is disabled; enabled: codex, claude (hrn backends set)"
	if err != nil || !result.IsError || len(result.Content) == 0 || result.Content[0].Text != want {
		t.Fatalf("got %+v, %v", result, err)
	}
}

func TestBackendsToolShowsEnabledAndDefaultPreferences(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"claude"}, DefaultName: "claude"}); err != nil {
		t.Fatal(err)
	}
	result, err := backendsTool().Handler(context.Background(), mcp.Call{})
	if err != nil {
		t.Fatal(err)
	}
	data := result.StructuredContent.(map[string]any)
	for _, entry := range data["backends"].([]map[string]any) {
		want := entry["name"] == "claude"
		if entry["enabled"] != want || entry["default"] != want {
			t.Errorf("entry: %v", entry)
		}
	}
	if data["default"] != "claude" {
		t.Errorf("default: %v", data["default"])
	}
}

func TestRunToolOffersNoEnumWhenThePreferenceFileIsMalformed(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(runlog.Home(), "backends.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	props := runTool(t.TempDir(), false).InputSchema["properties"].(map[string]any)
	backend := props["backend"].(map[string]any)
	if _, has := backend["enum"]; has {
		t.Fatalf("an empty enum would make every value invalid: %v", backend)
	}
}

type installStub struct {
	name      string
	installed bool
}

func (s installStub) Name() string { return s.name }
func (s installStub) Detect() (string, error) {
	if !s.installed {
		return "", errors.New("not installed")
	}
	return "stub", nil
}
func (s installStub) Run(context.Context, agent.Task, func(agent.Event)) agent.Result {
	panic("must not run")
}

func TestBackendsToolNamesOneDefaultWhenTheConfiguredOneIsNotInstalled(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	for _, s := range []installStub{{"codex", false}, {"claude", true}} {
		orig, _ := agent.Get(s.name)
		agent.Register(s)
		t.Cleanup(func() { agent.Register(orig) })
	}
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"codex", "claude"}, DefaultName: "codex"}); err != nil {
		t.Fatal(err)
	}
	result, err := backendsTool().Handler(context.Background(), mcp.Call{})
	if err != nil {
		t.Fatal(err)
	}
	data := result.StructuredContent.(map[string]any)
	if data["default"] != "claude" {
		t.Errorf("default: %v", data["default"])
	}
	for _, entry := range data["backends"].([]map[string]any) {
		if entry["default"] != (entry["name"] == "claude") {
			t.Errorf("entry: %v", entry)
		}
	}
	if strings.Count(result.Content[0].Text, "(default)") != 1 || !strings.Contains(result.Content[0].Text, "claude    enabled (default)") {
		t.Errorf("text: %s", result.Content[0].Text)
	}
}

func TestRunToolOffersEveryBackendWithoutAPreferenceFile(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	props := runTool(t.TempDir(), false).InputSchema["properties"].(map[string]any)
	got := props["backend"].(map[string]any)["enum"].([]any)
	want := make([]any, 0, len(agent.Names()))
	for _, name := range agent.Names() {
		want = append(want, name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("enum %v, want %v", got, want)
	}
}

func TestRunAndBackendsToolsHandleNamedAccounts(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	home := t.TempDir()
	orig, _ := agent.Get("claude")
	agent.Register(installStub{"claude", true})
	t.Cleanup(func() { agent.Register(orig) })
	pref := prefs.Prefs{Accounts: map[string][]prefs.Account{"claude": {{Name: "team-a", Home: home}}}}
	if err := prefs.Save(runlog.Home(), pref); err != nil {
		t.Fatal(err)
	}
	tool := runTool(t.TempDir(), false)
	if tool.InputSchema["properties"].(map[string]any)["account"] == nil {
		t.Fatal("missing account schema")
	}
	_, err := tool.Handler(t.Context(), mcp.Call{Arguments: json.RawMessage(`{"prompt":"hello","backend":"claude","account":"missing"}`)})
	if err == nil || !strings.Contains(err.Error(), "unknown claude account") {
		t.Fatalf("invalid account = %v", err)
	}
	records, err := runlog.List(0)
	if err != nil || len(records) != 0 {
		t.Fatalf("recorded a refused run: %v, %v", records, err)
	}
	result, err := backendsTool().Handler(t.Context(), mcp.Call{})
	if err != nil || len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "    team-a: usage unknown") || strings.Contains(result.Content[0].Text, home) {
		t.Fatalf("listing: %+v, %v", result, err)
	}
}

func TestBackendsToolStructuresAccountQuotaAndLimits(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	home := t.TempDir()
	p := prefs.Prefs{Accounts: map[string][]prefs.Account{"claude": {
		{Name: "team-a", Home: home}, {Name: "team-b", Home: home},
	}}}
	if err := prefs.Save(runlog.Home(), p); err != nil {
		t.Fatal(err)
	}
	rec, err := runlog.Create("claude", agent.Task{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	until := now.Add(time.Hour).Truncate(time.Second)
	rec.Event(agent.Event{Time: now, Kind: agent.EventQuota, Quota: &kernel.Quota{FiveHour: &kernel.QuotaWindow{UsedPercent: 20, ResetsAt: until.Unix()}}})
	if err := rec.Close(agent.Result{Backend: "claude", Account: "team-a", Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	limited := agent.Result{Status: agent.StatusLimited, LimitResets: until}
	if err := limits.NoteAccount("claude", "team-a", limited); err != nil {
		t.Fatal(err)
	}
	if err := limits.Note("codex", limited); err != nil {
		t.Fatal(err)
	}
	res, err := backendsTool().Handler(t.Context(), mcp.Call{})
	if err != nil || res.IsError {
		t.Fatalf("backends: %v", err)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Backends []struct {
			Name         string
			Accounts     []map[string]string
			LimitedUntil time.Time `json:"limited_until"`
		}
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	for _, entry := range data.Backends {
		switch entry.Name {
		case "claude":
			if len(entry.Accounts) != 2 {
				t.Fatalf("accounts: %v", entry.Accounts)
			}
			a, b := entry.Accounts[0], entry.Accounts[1]
			if len(a) != 3 || a["name"] != "team-a" || !strings.HasPrefix(a["quota"], "5h: 80% left") ||
				a["limited_until"] != until.UTC().Format(time.RFC3339) || !strings.Contains(res.Text(), "team-a: "+a["quota"]) ||
				!reflect.DeepEqual(b, map[string]string{"name": "team-b", "quota": "usage unknown"}) {
				t.Error("account names, quota or limit did not match")
			}
		case "codex":
			if entry.Accounts != nil || !entry.LimitedUntil.Equal(until) {
				t.Error("legacy backend limit changed")
			}
		}
	}
}

func TestBackendsToolOrganisation(t *testing.T) {
	t.Setenv("HRN_HOME", prefstest.TempDir(t))
	path := prefstest.OrgPath(t)
	prefstest.Write(t, filepath.Join(runlog.Home(), "backends.json"), `{"billing":"metered"}`)
	list := func() mcp.CallResult {
		t.Helper()
		result, err := backendsTool().Handler(context.Background(), mcp.Call{})
		if err != nil || result.IsError {
			t.Fatalf("backends = %+v, %v", result, err)
		}
		return result
	}
	if _, ok := list().StructuredContent.(map[string]any)["organisation"]; ok {
		t.Fatal("organisation without a file")
	}
	prefstest.Write(t, path, "{}")
	data := list().StructuredContent.(map[string]any)
	if got, err := json.Marshal(data["organisation"]); err != nil || string(got) != `{"locked":[],"sets":[]}` {
		t.Fatalf("empty organisation = %s, %v", got, err)
	}
	prefstest.Write(t, path, `{"enabled":["claude"],"billing":"plan","locked":["billing"]}`)
	result := list()
	for _, line := range []string{"  enabled: set by the organisation\n", "  billing: set by the organisation (locked)\n", "backends: billing set by the organisation; your backends.json values for them are ignored"} {
		if !strings.Contains(result.Content[0].Text, line) {
			t.Errorf("missing %q", line)
		}
	}
	data = result.StructuredContent.(map[string]any)
	if got, err := json.Marshal(data["organisation"]); err != nil || string(got) != `{"locked":["billing"],"sets":["enabled","billing"]}` {
		t.Fatalf("organisation = %s, %v", got, err)
	}
}

func TestRunToolRefusesBackendExcludedByOrganisation(t *testing.T) {
	t.Setenv("HRN_HOME", prefstest.TempDir(t))
	prefstest.Write(t, prefstest.OrgPath(t), `{"enabled":["claude"],"locked":["enabled"]}`)
	prefstest.Write(t, filepath.Join(runlog.Home(), "backends.json"), `{"enabled":["codex"]}`)
	original, err := agent.Get("codex")
	if err != nil {
		t.Fatal(err)
	}
	agent.Register(installStub{"codex", true})
	t.Cleanup(func() { agent.Register(original) })
	tool := runTool(prefstest.TempDir(t), false)
	result, err := tool.Handler(context.Background(), mcp.Call{Arguments: json.RawMessage(`{"prompt":"hello","backend":"codex"}`)})
	if err != nil || !result.IsError || len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "backend codex is disabled") {
		t.Fatalf("run = %+v, %v", result, err)
	}
}
