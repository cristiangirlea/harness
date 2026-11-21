package runlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/kernel"
)

func TestLoadAcceptsRecordsWrittenBeforePluginCalls(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := filepath.Join(Root(), "old-run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const old = `{"id":"old-run","backend":"codex","task":{"prompt":"old"},"result":{"status":"ok"}}`
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err := Load("old-run")
	if err != nil || !rec.Done || rec.Backend != "codex" || rec.Plugin != nil {
		t.Fatalf("Load: %+v, %v", rec, err)
	}
}

func TestPluginMetadataRoundTripsBeforeAndAfterTheResult(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	run := PluginRun{
		Name: "fixture", Version: "0.1.0", Protocol: "v1", Capability: "echo",
		SHA256: strings.Repeat("a", 64), Path: filepath.Join(t.TempDir(), "hrn-fixture"),
		Installed: true, Input: json.RawMessage("{}"),
	}
	w, err := CreatePlugin(kernel.Task{Dir: t.TempDir()}, run)
	if err != nil {
		t.Fatal(err)
	}
	rec, loadErr := Load(w.ID())
	run.Reported = []kernel.Evidence{{Kind: "check", Source: "fixture", Excerpt: "echoed"}}
	w.SetPluginReported(run.Reported)
	if err := w.Close(kernel.Result{Backend: PluginBackend, Status: kernel.StatusOK}); err != nil {
		t.Fatal(err)
	}
	if loadErr != nil || rec.Done || rec.Backend != PluginBackend || rec.Plugin == nil ||
		!reflect.DeepEqual(rec.Plugin.Input, run.Input) || len(rec.Plugin.Reported) != 0 {
		t.Fatalf("Load before close: %+v, %v", rec, loadErr)
	}
	rec, err = Load(w.ID())
	if err != nil || !rec.Done || !reflect.DeepEqual(rec.Plugin, &run) || len(rec.Result.Evidence) != 0 {
		t.Fatalf("Load after close: %+v, plugin %+v, %v", rec, rec.Plugin, err)
	}
}

func TestPluginRecordsKeepInputUpToTheLimit(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, extra := range []int{0, 1} {
		input := json.RawMessage(`{"value":"` + strings.Repeat("a", (256<<10)-12+extra) + `"}`)
		w, err := CreatePlugin(kernel.Task{Dir: t.TempDir()}, PluginRun{Name: "fixture", Capability: "echo", Input: input})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(kernel.Result{Backend: PluginBackend, Status: kernel.StatusOK}); err != nil {
			t.Fatal(err)
		}
		rec, err := Load(w.ID())
		if err != nil || rec.Plugin == nil {
			t.Fatalf("Load: %+v, %v", rec, err)
		}
		if rec.Plugin.InputTruncated != (extra != 0) || (len(rec.Plugin.Input) == 0) != (extra != 0) {
			t.Fatalf("input of %d bytes: kept %d, truncated %t", len(input), len(rec.Plugin.Input), rec.Plugin.InputTruncated)
		}
	}
}
