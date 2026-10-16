package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/update"
)

func TestUpdateFlagsNameOneSource(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("HRN_HOME", t.TempDir())
	o, err := updateOptions(nil)
	if err != nil || o.Source.Release == nil || o.Source.Ref != "" || o.Source.File != "" {
		t.Errorf("no flags: %+v, %v; want the release", o.Source, err)
	}
	if o.Home != os.Getenv("HRN_HOME") {
		t.Errorf("home %q", o.Home)
	}
	o, err = updateOptions([]string{"--main", "--dry-run"})
	if err != nil || o.Source.Ref != "main" || o.Source.Release != nil || !o.DryRun {
		t.Errorf("--main: %+v dry=%v, %v", o.Source, o.DryRun, err)
	}
	o, err = updateOptions([]string{"--ref", "v0.2.0"})
	if err != nil || o.Source.Ref != "v0.2.0" {
		t.Errorf("--ref: %+v, %v", o.Source, err)
	}
	o, err = updateOptions([]string{"--file", "dist/hrn.exe"})
	if err != nil || o.Source.File != "dist/hrn.exe" || o.Source.Release != nil {
		t.Errorf("--file: %+v, %v", o.Source, err)
	}
	o, err = updateOptions([]string{"--rollback"})
	if err != nil || !o.Rollback || o.Source.Release != nil {
		t.Errorf("--rollback: %+v rollback=%v, %v", o.Source, o.Rollback, err)
	}
	for _, bad := range [][]string{
		{"--main", "--ref", "x"},
		{"--file", "a", "--main"},
		{"--rollback", "--file", "a"},
		{"extra"},
		{"--nope"},
	} {
		if _, err := updateOptions(bad); err == nil || !strings.Contains(err.Error(), updateUsage) {
			t.Errorf("%v: err = %v; want a refusal with the usage", bad, err)
		}
	}
}

func TestUpdatePassesTheTokenToTheReleaseSourceOnly(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_test")
	o, err := updateOptions(nil)
	if err != nil || o.Source.Release == nil || o.Source.Release.Token != "ghp_test" {
		t.Fatalf("%+v, %v", o.Source.Release, err)
	}
	o, _ = updateOptions([]string{"--file", "x"})
	if o.Source.Release != nil {
		t.Error("a --file run made a release source")
	}
}

// The command's dry run over a fake installation: the plan is printed and
// no file or process is touched. Versions are answered in memory here; the
// swap itself is tested with real copies in internal/update.
func TestUpdateDryRunPrintsThePlanAndChangesNothing(t *testing.T) {
	dir := t.TempDir()
	name := "hrn"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(dir, name)
	candidate := filepath.Join(t.TempDir(), name)
	os.WriteFile(target, []byte("old"), 0o755)
	os.WriteFile(candidate, []byte("new"), 0o755)
	lines := map[string]string{target: "hrn 0.2.0 (go1.26.4, a0e5d2f12345)", candidate: "hrn 0.2.0 (go1.26.4, b1f6e3a23456)"}

	o, err := updateOptions([]string{"--file", candidate, "--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	o.Targets = []string{target}
	o.Version = func(ctx context.Context, path string) (update.Version, error) {
		return update.ParseVersionLine(lines[path])
	}
	o.Lister = fakeProcesses{{PID: 11, Exe: target, Args: []string{"watch", "--lan"}}}
	o.Restarter = update.Restarter{
		Stop:  func(context.Context, int) error { t.Error("stop called"); return nil },
		Start: func(string, []string, string, string) (int, error) { t.Error("start called"); return 0, nil },
	}
	o.Now = func() time.Time { return time.Date(2026, 10, 15, 9, 7, 0, 0, time.Local) }
	var out bytes.Buffer
	code, err := update.Run(context.Background(), &out, o)
	if err != nil || code != 0 {
		t.Fatalf("code %d, err %v\n%s", code, err, out.String())
	}
	for _, want := range []string{
		"installed: hrn 0.2.0 (go1.26.4, a0e5d2f12345)",
		"candidate: hrn 0.2.0 (go1.26.4, b1f6e3a23456)",
		"targets:",
		"would rename " + target + " to hrn-a0e5d2f12345-20261015-0907.prev" + filepath.Ext(name),
		"would restart watch (pid 11): hrn watch --lan",
		"dry run: nothing changed",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if b, _ := os.ReadFile(target); string(b) != "old" {
		t.Error("the target changed")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("files beside the target: %d", len(entries))
	}
}

type fakeProcesses []update.Process

func (f fakeProcesses) Processes() ([]update.Process, error) { return []update.Process(f), nil }

func TestUpdateUsageIsListed(t *testing.T) {
	if !strings.Contains(usage, "hrn update [--main | --ref REF | --file PATH] [--dry-run] [--rollback]") {
		t.Error("the usage text does not list hrn update")
	}
}
