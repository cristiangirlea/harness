package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/route/table"
)

func TestRouteIgnoresADisabledBackendWithoutRecordingEvidence(t *testing.T) {
	home := t.TempDir()
	if err := prefs.Save(home, prefs.Prefs{EnabledNames: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	if err := table.Save(home, table.Table{"review": {Backend: "codex", Model: "model", Effort: "high", Eval: "evidence"}}); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = old }()
	var o Options
	var backend string
	routed, err := o.Route("", "Purpose: review\nReview the change", &backend, home)
	if err != nil || routed != nil || backend != "" || o.Model != "" || o.Effort != "" {
		t.Fatalf("got %+v, %s, %+v, %v", routed, backend, o, err)
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hrn: warning: ignoring route for review: backend codex is disabled") {
		t.Fatalf("warning: %s", data)
	}
}

func TestRouteUsesItsBackendWithoutAPreferenceFile(t *testing.T) {
	home := t.TempDir()
	if err := table.Save(home, table.Table{"review": {Backend: "codex", Model: "model", Effort: "high", Eval: "evidence"}}); err != nil {
		t.Fatal(err)
	}
	var o Options
	var backend string
	routed, err := o.Route("", "Purpose: review\nReview the change", &backend, home)
	if err != nil || routed == nil || backend != "codex" || o.Model != "model" || o.Effort != "high" {
		t.Fatalf("got %+v, %s, %+v, %v", routed, backend, o, err)
	}
}

func TestRouteWithNote(t *testing.T) {
	for _, tc := range []struct {
		name, declared string
		backend        string
		model          string
		effort         string
		row            table.Row
		prefs          []string
		wantNote       string
		wantContains   string
		wantRoute      bool
	}{
		{name: "explicit backend", backend: "codex", wantNote: "backend codex was named for this run"},
		{name: "no row", declared: "implement", wantNote: "no route row for purpose implement"},
		{name: "disabled backend", declared: "review", row: table.Row{Backend: "codex", Model: "m"}, prefs: []string{"claude"}, wantNote: "ignoring route for review: backend codex is disabled"},
		{name: "unknown backend", declared: "review", row: table.Row{Backend: "missing", Model: "m"}, wantContains: `ignoring route for review: unknown backend "missing"`},
		{name: "invalid effort", declared: "review", row: table.Row{Effort: "bogus"}, wantContains: `ignoring route for review: unknown effort "bogus"`},
		{name: "applied", declared: "review", row: table.Row{Backend: "codex", Model: "m", Effort: "high"}, wantRoute: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.row != (table.Row{}) {
				if err := table.Save(home, table.Table{tc.declared: tc.row}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.prefs != nil {
				if err := prefs.Save(home, prefs.Prefs{EnabledNames: tc.prefs}); err != nil {
					t.Fatal(err)
				}
			}
			o := Options{Model: tc.model, Effort: tc.effort}
			backend := tc.backend
			routed, note, err := o.RouteWithNote(tc.declared, "", &backend, home)
			if err != nil || note != tc.wantNote && !strings.Contains(note, tc.wantContains) || (routed != nil) != tc.wantRoute {
				t.Fatalf("route=%+v note=%q err=%v", routed, note, err)
			}
		})
	}
}
