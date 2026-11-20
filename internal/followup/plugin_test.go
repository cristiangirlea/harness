package followup

import (
	"testing"

	"github.com/cristiangirlea/harness/internal/kernel"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestPluginCallsCannotBeContinuedEvenWithABackendOverride(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.CreatePlugin(kernel.Task{Dir: t.TempDir()}, runlog.PluginRun{Name: "fixture", Capability: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(kernel.Result{Backend: runlog.PluginBackend, Status: kernel.StatusOK}); err != nil {
		t.Fatal(err)
	}
	want := "run " + w.ID() + " is a plugin call; plugin calls cannot be continued"
	for _, backend := range []string{"", "api"} {
		_, err := Make(Request{RunID: w.ID(), Backend: backend})
		if err == nil || err.Error() != want {
			t.Errorf("backend %q: got %v, want %q", backend, err, want)
		}
	}
}
