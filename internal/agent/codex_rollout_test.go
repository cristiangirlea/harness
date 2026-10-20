package agent

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const recordedCodexThread = "01a154be-7824-76e3-94f2-306f25710b68"

// TestHelperCodexRecordedStream replays a recorded `codex exec --json` stream.
func TestHelperCodexRecordedStream(t *testing.T) {
	path := os.Getenv("HRN_CODEX_RECORDED_STREAM")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		os.Exit(2)
	}
	os.Stdout.Write(data)
	switch os.Getenv("HRN_CODEX_RECORDED_EXIT") {
	case "fail":
		os.Exit(1)
	case "hang":
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func runRecordedCodex(t *testing.T, home, dir, model string) Result {
	t.Helper()
	return runRecordedCodexTask(t, home, Task{Prompt: "work", Dir: dir, Options: Options{Model: model}})
}

func runRecordedCodexTask(t *testing.T, home string, task Task) Result {
	t.Helper()
	stream, err := filepath.Abs(filepath.Join("testdata", "codex-exec.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRN_CODEX_RECORDED_STREAM", stream)
	t.Setenv("CODEX_HOME", home)
	b := NewCodexBackend()
	b.name, b.candidates = os.Args[0], nil
	b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperCodexRecordedStream$", "--"} }
	return b.Run(context.Background(), task, func(Event) {})
}

// placeRollout copies the recorded rollout into a session-date folder, as codex writes it.
func placeRollout(t *testing.T, home string, day time.Time, rewrite func(string) string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "codex-rollout.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "sessions", filepath.FromSlash(day.Format("2006/01/02")))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "rollout-" + day.Format("2006-01-02T15-04-05") + "-" + recordedCodexThread + ".jsonl"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(rewrite(string(data))), 0o644); err != nil {
		t.Fatal(err)
	}
}

func same(s string) string { return s }

func TestCodexRecordsTheModelItsRolloutReports(t *testing.T) {
	for _, tc := range []struct {
		name, requested string
		relativeHome    bool
		bracketHome     bool
		day             time.Time
		rewrite         func(string) string
	}{
		{name: "default model", day: time.Now()},
		{name: "requested model differs", requested: "gpt-6-luna", day: time.Now()},
		{name: "relative CODEX_HOME", relativeHome: true, day: time.Now()},
		{name: "session began the day before", day: time.Now().AddDate(0, 0, -1)},
		{name: "a line cut short before the turn", day: time.Now(), rewrite: func(s string) string {
			return strings.Replace(s, "\n", "\n{\"type\":\"response_item\",\"payload\":{\"text\":\"cut\n", 1)
		}},
		{name: "a fork names its parent", day: time.Now(), rewrite: func(s string) string {
			s = strings.Replace(s, `"source":"exec"`, `"source":"exec","forked_from_id":"parent-thread"`, 1)
			return s + `{"type":"session_meta","payload":{"id":"parent-thread","cli_version":"0.1.0"}}` + "\n"
		}},
		{name: "home path with pattern characters", bracketHome: true, day: time.Now()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			home := t.TempDir()
			homeEnv := home
			if tc.relativeHome {
				home, homeEnv = filepath.Join(dir, "codex-home"), "codex-home"
			}
			if tc.bracketHome {
				home = filepath.Join(home, "work[old]")
				homeEnv = home
			}
			rewrite := same
			if tc.rewrite != nil {
				rewrite = tc.rewrite
			}
			placeRollout(t, home, tc.day, rewrite)
			res := runRecordedCodex(t, homeEnv, dir, tc.requested)
			if res.Status != StatusOK || res.Session != recordedCodexThread || res.Final != "ok" {
				t.Fatalf("result = %+v", res)
			}
			if res.Model != "gpt-6.1-sol" || !res.ModelReported || res.CLIVersion != "0.162.0-alpha.17.2" {
				t.Fatalf("model = %q reported = %v cli = %q", res.Model, res.ModelReported, res.CLIVersion)
			}
			// The reported model prices the run: (20605-13440)*2 + 13440*0.10 + 5*10 per million.
			if want := 0.015724; math.Abs(res.CostUSD-want) > 1e-9 || !res.CostEstimate || res.Usage.CostStatus != "estimated" {
				t.Fatalf("cost = %v estimate = %v status = %s", res.CostUSD, res.CostEstimate, res.Usage.CostStatus)
			}
		})
	}
}

func TestCodexKeepsTheRequestedModelWithoutARollout(t *testing.T) {
	for _, tc := range []struct {
		name, requested, version string
		place                    func(t *testing.T, home string)
	}{
		{name: "no rollout, default model", place: func(*testing.T, string) {}},
		{name: "no rollout, requested model", requested: "requested-model", place: func(*testing.T, string) {}},
		{name: "rollout of another session", requested: "requested-model", place: func(t *testing.T, home string) {
			placeRollout(t, home, time.Now(), func(s string) string {
				return strings.ReplaceAll(s, `"id":"`+recordedCodexThread+`"`, `"id":"another-thread"`)
			})
		}},
		{name: "rollout without a turn", requested: "requested-model", version: "0.162.0-alpha.17.2", place: func(t *testing.T, home string) {
			placeRollout(t, home, time.Now(), func(s string) string {
				return strings.ReplaceAll(s, `"type":"turn_context"`, `"type":"other"`)
			})
		}},
		{name: "rollout model and version are not ids", requested: "requested-model", place: func(t *testing.T, home string) {
			placeRollout(t, home, time.Now(), func(s string) string {
				s = strings.ReplaceAll(s, `"model":"gpt-6.1-sol"`, `"model":"--yolo"`)
				return strings.ReplaceAll(s, `"cli_version":"0.162.0-alpha.17.2"`, `"cli_version":"--x"`)
			})
		}},
		{name: "rollout a week old", requested: "requested-model", place: func(t *testing.T, home string) {
			placeRollout(t, home, time.Now().AddDate(0, 0, -7), same)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			tc.place(t, home)
			res := runRecordedCodex(t, home, t.TempDir(), tc.requested)
			if res.Status != StatusOK || res.Session != recordedCodexThread {
				t.Fatalf("result = %+v", res)
			}
			if res.Model != tc.requested || res.ModelReported || res.CLIVersion != tc.version {
				t.Fatalf("model = %q reported = %v cli = %q, want %q unreported, cli %q", res.Model, res.ModelReported, res.CLIVersion, tc.requested, tc.version)
			}
			if res.CostEstimate || res.CostUSD != 0 {
				t.Fatalf("an unreported, unpriced model must not be costed: %+v", res)
			}
		})
	}
}

func TestCodexRunThatFailsOrTimesOutKeepsTheReportedModel(t *testing.T) {
	for _, tc := range []struct {
		exit    string
		timeout time.Duration
		status  Status
	}{
		{exit: "fail", status: StatusError},
		{exit: "hang", timeout: 3 * time.Second, status: StatusTimeout},
	} {
		t.Run(tc.exit, func(t *testing.T) {
			t.Setenv("HRN_CODEX_RECORDED_EXIT", tc.exit)
			home := t.TempDir()
			placeRollout(t, home, time.Now(), same)
			res := runRecordedCodexTask(t, home, Task{Prompt: "work", Dir: t.TempDir(), Options: Options{Timeout: Duration(tc.timeout)}})
			if res.Status != tc.status || res.Model != "gpt-6.1-sol" || !res.ModelReported || res.CLIVersion != "0.162.0-alpha.17.2" {
				t.Fatalf("result = %+v", res)
			}
		})
	}
}

func TestCLIVersionIsAdditiveAndOmittedWhenEmpty(t *testing.T) {
	data, err := json.Marshal(Result{Backend: "codex", Status: StatusOK})
	if err != nil || strings.Contains(string(data), "cli_version") {
		t.Fatalf("empty must be omitted: %s %v", data, err)
	}
}

func TestCodexRolloutSearchCoversEveryDateOfTheRunAndOneEitherSide(t *testing.T) {
	started := time.Date(2026, 10, 19, 23, 30, 0, 0, time.Local)
	now := started.Add(time.Hour)
	for _, tc := range []struct {
		day   time.Time
		found bool
	}{
		{started.AddDate(0, 0, -2), false},
		{started.AddDate(0, 0, -1), true},
		{started, true},
		{now, true},
		{now.AddDate(0, 0, 1), true},
		{now.AddDate(0, 0, 2), false},
	} {
		home := t.TempDir()
		placeRollout(t, home, tc.day, same)
		model, _ := codexRollout(home, recordedCodexThread, started, now)
		if (model != "") != tc.found {
			t.Fatalf("rollout dated %s: model %q, want found = %v", tc.day.Format("2006-01-02"), model, tc.found)
		}
	}
}
