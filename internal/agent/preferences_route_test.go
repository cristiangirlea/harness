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
