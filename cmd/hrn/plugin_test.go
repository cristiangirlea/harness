package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/kernel"
	"github.com/cristiangirlea/harness/internal/pluginproto"
	"github.com/cristiangirlea/harness/internal/runlog"
)

const fixturePluginVersion = "0.1.0"

var fixtureCapabilities = []pluginproto.Capability{
	{Name: "echo", Description: "Return the input."},
	{Name: "fail", Description: "Report a failure."},
	{Name: "wait", Description: "Wait for cancellation."},
	{Name: "noisy", Description: "Emit terminal control characters."},
	{Name: "vanish", Description: "Exit without answering."},
	{Name: "giveup", Description: "Cancel the call on its own."},
	{Name: "fancy", Description: "Return output with characters a terminal may act on."},
}

// fancyOutput holds a raw C1 control sequence introducer and a right-to-left override, both valid in JSON.
var fancyOutput = `{"text":"` + string(rune(0x9b)) + "31mred" + string(rune(0x202e)) + string(rune(0x061c)) + `"}`

func fakePluginMain(name string) int {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), pluginproto.MaxLine+2)
	encoder := json.NewEncoder(os.Stdout)
	var seq int64
	var waiting string
	for scanner.Scan() {
		frame, err := pluginproto.ParseHostLine(scanner.Bytes())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		switch frame := frame.(type) {
		case *pluginproto.Hello:
			if seq != 0 {
				return 1
			}
			seq++
			if name == "refuses" {
				if err := encoder.Encode(pluginproto.Error{
					V: 1, Seq: seq, Type: pluginproto.TypeError, Code: pluginproto.CodeUnsupportedProtocol,
					Message: "fixture speaks v2", Protocols: []string{"v2"},
				}); err != nil {
					return 1
				}
				return 2
			}
			if name != "fixture" && name != "echo" {
				return 1
			}
			if name == "echo" {
				if override, err := os.ReadFile(".plugin-fixture-name"); err == nil {
					name = string(override)
				}
			}
			err = encoder.Encode(pluginproto.Ready{
				V: 1, Seq: seq, Type: pluginproto.TypeReady, Protocol: pluginproto.V1,
				Plugin:       pluginproto.Program{Name: name, Version: fixturePluginVersion},
				Capabilities: fixtureCapabilities,
			})
		case *pluginproto.Call:
			if seq == 0 || waiting != "" {
				return 1
			}
			if frame.Capability == "wait" {
				waiting = frame.ID
				continue
			}
			err = fakePluginCall(encoder, &seq, frame)
		case *pluginproto.Cancel:
			seq++
			if waiting != frame.ID {
				err = encoder.Encode(pluginproto.Error{
					V: 1, Seq: seq, Type: pluginproto.TypeError, ID: frame.ID,
					Code: pluginproto.CodeBadRequest, Message: "unknown call",
				})
			} else {
				waiting = ""
				err = encoder.Encode(pluginproto.Result{
					V: 1, Seq: seq, Type: pluginproto.TypeResult, ID: frame.ID, Status: pluginproto.StatusCanceled,
				})
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func fakePluginCall(encoder *json.Encoder, seq *int64, call *pluginproto.Call) error {
	res := pluginproto.Result{V: 1, Type: pluginproto.TypeResult, ID: call.ID, Status: pluginproto.StatusOK}
	var eventText string
	switch call.Capability {
	case "echo":
		eventText = "working"
		res.Output = call.Input
		if len(res.Output) == 0 {
			res.Output = json.RawMessage("{}")
		}
		res.Evidence = []pluginproto.Evidence{{Kind: "check", Source: "fixture", Excerpt: "echoed"}}
	case "fail":
		res.Status = pluginproto.StatusFailed
		res.Error = &pluginproto.Problem{Code: pluginproto.CodeInternal, Message: "fixture failure"}
	case "noisy":
		eventText = "\x1b]0;x\x07bad"
	case "vanish":
		os.Exit(3)
	case "giveup":
		res.Status = pluginproto.StatusCanceled
	case "fancy":
		res.Output = json.RawMessage(fancyOutput)
	default:
		return fmt.Errorf("unexpected capability %q", call.Capability)
	}
	if eventText != "" {
		*seq++
		if err := encoder.Encode(pluginproto.Event{
			V: 1, Seq: *seq, Type: pluginproto.TypeEvent, ID: call.ID, Kind: pluginproto.KindProgress, Text: eventText,
		}); err != nil {
			return err
		}
	}
	*seq++
	res.Seq = *seq
	return encoder.Encode(res)
}

func useEmptyPluginHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Chdir(t.TempDir())
	return home
}

func installCommandPlugin(t *testing.T, home, name string) (path, hash string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	dir := filepath.Join(home, "plugins", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "hrn-"+name)
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	copyFile, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(copyFile, source)
	if err := errors.Join(copyErr, copyFile.Close()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, fmt.Sprintf("%x", sha256.Sum256(data))
}

func runPluginCommand(t *testing.T, stdin string, wantCode int, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out, log bytes.Buffer
	code, err := cmdPluginContext(ctx, strings.NewReader(stdin), &out, &log, args)
	if code != wantCode {
		t.Fatalf("%v: exit %d, want %d, err %v\nstdout: %s\nstderr: %s", args, code, wantCode, err, &out, &log)
	}
	return out.String(), log.String(), err
}

func TestPluginListShowsInstalledPluginsAndAnEmptyArrayWhenNoneExist(t *testing.T) {
	home := useEmptyPluginHome(t)
	out, _, err := runPluginCommand(t, "", 0, "list")
	if err != nil || !strings.Contains(out, "no plugins found") {
		t.Fatalf("list: %q, %v", out, err)
	}
	out, _, err = runPluginCommand(t, "", 0, "list", "--json")
	if err != nil || strings.TrimSpace(out) != `{"plugins":[]}` {
		t.Fatalf("empty list: %q, %v", out, err)
	}

	path, _ := installCommandPlugin(t, home, "fixture")
	out, _, err = runPluginCommand(t, "", 0, "list")
	if err != nil || out != fmt.Sprintf("fixture  installed  %s\n", path) {
		t.Fatalf("list: %q, %v", out, err)
	}
	out, _, err = runPluginCommand(t, "", 0, "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		Name      string `json:"name"`
		Path      string `json:"path"`
		Installed bool   `json:"installed"`
	}
	var list struct {
		Plugins []row `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(list.Plugins, []row{{Name: "fixture", Path: path, Installed: true}}) {
		t.Fatalf("list JSON: %s", out)
	}
}

func TestPluginInfoShowsTheHandshakeAndExecutableHashWithoutRecordingARun(t *testing.T) {
	home := useEmptyPluginHome(t)
	path, hash := installCommandPlugin(t, home, "fixture")
	out, _, err := runPluginCommand(t, "", 0, "info", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fixture", fixturePluginVersion, "v1", path, hash} {
		if !strings.Contains(out, want) {
			t.Errorf("info misses %q:\n%s", want, out)
		}
	}
	for _, cap := range fixtureCapabilities {
		if !strings.Contains(out, cap.Name) || !strings.Contains(out, cap.Description) {
			t.Errorf("info misses %+v:\n%s", cap, out)
		}
	}
	out, _, err = runPluginCommand(t, "", 0, "info", "fixture", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		Name         string                   `json:"name"`
		Version      string                   `json:"version"`
		Protocol     string                   `json:"protocol"`
		Path         string                   `json:"path"`
		SHA256       string                   `json:"sha256"`
		Capabilities []pluginproto.Capability `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatal(err)
	}
	if info.Name != "fixture" || info.Version != fixturePluginVersion || info.Protocol != "v1" ||
		info.Path != path || info.SHA256 != hash || !reflect.DeepEqual(info.Capabilities, fixtureCapabilities) {
		t.Fatalf("info JSON: %s", out)
	}
	_, _, err = runPluginCommand(t, "", 1, "info", "missing")
	if err == nil || !strings.Contains(err.Error(), "hrn plugin install OWNER/hrn-missing") {
		t.Fatalf("missing info: %v", err)
	}
	requireNoPluginRuns(t)
}

func TestPluginCallKeepsOutputUsageAndReportedEvidenceInItsRun(t *testing.T) {
	home := useEmptyPluginHome(t)
	path, hash := installCommandPlugin(t, home, "fixture")
	out, log, err := runPluginCommand(t, "", 0, "call", "fixture", "echo", "--input", `{"a":1}`)
	if err != nil || out != "{\n  \"a\": 1\n}\n" || !strings.Contains(log, "progress: working\n") {
		t.Fatalf("echo: stdout %q, stderr %q, err %v", out, log, err)
	}
	rec := loadCommandPluginRun(t, log)
	if !rec.Done || rec.Backend != runlog.PluginBackend || rec.Result.Backend != runlog.PluginBackend ||
		rec.Result.Status != kernel.StatusOK || rec.Result.Final != `{"a":1}` || rec.Result.Turns != 0 ||
		rec.Task.Prompt != "plugin fixture: echo" || rec.Task.Mode != "" || time.Duration(rec.Task.Timeout) != 10*time.Minute {
		t.Fatalf("echo record: %+v", rec)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	gotDir, err := filepath.EvalSymlinks(rec.Task.Dir)
	if err != nil {
		t.Fatal(err)
	}
	wantDir, err := filepath.EvalSymlinks(cwd)
	if err != nil || gotDir != wantDir {
		t.Fatalf("record directory %q, want %q: %v", gotDir, wantDir, err)
	}
	if rec.Plugin == nil || rec.Plugin.Name != "fixture" || rec.Plugin.Version != fixturePluginVersion ||
		rec.Plugin.Protocol != "v1" || rec.Plugin.Capability != "echo" || rec.Plugin.Path != path ||
		rec.Plugin.SHA256 != hash || !rec.Plugin.Installed || rec.Plugin.InputTruncated ||
		!jsonEqual(rec.Plugin.Input, `{"a":1}`) {
		t.Fatalf("echo plugin metadata: %+v", rec.Plugin)
	}
	wantEvidence := []kernel.Evidence{{Kind: "check", Source: "fixture", Excerpt: "echoed"}}
	if !reflect.DeepEqual(rec.Plugin.Reported, wantEvidence) || len(rec.Result.Evidence) != 0 {
		t.Fatalf("reported %+v, verified %+v", rec.Plugin.Reported, rec.Result.Evidence)
	}
	if cost, known, estimated := rec.Result.KnownCost(); cost != 0 || !known || estimated {
		t.Fatalf("cost: %v, known %t, estimated %t", cost, known, estimated)
	}
	events, err := runlog.Events(rec.ID)
	if err != nil || len(events) != 1 || events[0].Kind != kernel.EventInfo || events[0].Text != "progress: working" {
		t.Fatalf("events: %+v, %v", events, err)
	}

	show, err := captureStdout(t, func() error { return cmdShow([]string{rec.ID}) })
	if err != nil || !bytes.Contains(show, []byte(rec.ID)) || !bytes.Contains(show, []byte("plugin fixture: echo")) {
		t.Fatalf("show: %q, %v", show, err)
	}
	report, err := captureStdout(t, func() error { return cmdReport([]string{"--by", "backend"}) })
	if err != nil || !bytes.Contains(report, []byte("\nplugin ")) {
		t.Fatalf("report: %q, %v", report, err)
	}
}

func jsonEqual(input json.RawMessage, want string) bool {
	var compact bytes.Buffer
	return json.Compact(&compact, input) == nil && compact.String() == want
}

func loadCommandPluginRun(t *testing.T, log string) runlog.Record {
	t.Helper()
	for line := range strings.Lines(log) {
		if id, ok := strings.CutPrefix(strings.TrimSpace(line), "run "); ok {
			rec, err := runlog.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			return rec
		}
	}
	t.Fatalf("no run id in %q", log)
	return runlog.Record{}
}

func TestPluginCallsRecordFailuresWithOrWithoutAReplyAndTimeout(t *testing.T) {
	home := useEmptyPluginHome(t)
	installCommandPlugin(t, home, "fixture")
	for _, tc := range []struct {
		name string
		args []string
		want kernel.Status
		msg  string
	}{
		{"failure", []string{"fail"}, kernel.StatusError, "internal: fixture failure"},
		{"no result", []string{"vanish"}, kernel.StatusError, "plugin exited before the result"},
		{"plugin gave up", []string{"giveup"}, kernel.StatusCanceled, "plugin canceled the call"},
		{"timeout", []string{"wait", "--timeout", "300ms"}, kernel.StatusTimeout, "timed out after 300ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, log, err := runPluginCommand(t, "", 1, append([]string{"call", "fixture"}, tc.args...)...)
			summary := fmt.Sprintf("plugin fixture %s: %s: ", tc.args[0], tc.want)
			if err != nil || out != "" || !strings.Contains(log, summary) || !strings.Contains(log, tc.msg) {
				t.Fatalf("call: stdout %q, stderr %q, err %v", out, log, err)
			}
			rec := loadCommandPluginRun(t, log)
			if !rec.Done || rec.Result.Status != tc.want || !strings.Contains(rec.Result.Error, tc.msg) || rec.Result.Final != "" {
				t.Fatalf("result: %+v", rec.Result)
			}
		})
	}
}

func TestPluginCallStripsTerminalControlsFromEventOutput(t *testing.T) {
	home := useEmptyPluginHome(t)
	installCommandPlugin(t, home, "fixture")
	out, log, err := runPluginCommand(t, "", 0, "call", "fixture", "noisy")
	if err != nil || out != "{}\n" || !strings.Contains(log, "bad") || strings.ContainsAny(log, "\x1b\x07") {
		t.Fatalf("call: stdout %q, stderr %q, err %v", out, log, err)
	}
}

func TestPluginCallsPrintOneJSONResultAndCanReadInputFromStdin(t *testing.T) {
	home := useEmptyPluginHome(t)
	installCommandPlugin(t, home, "fixture")
	out, log, err := runPluginCommand(t, " \n{\"stdin\":true}\n", 0, "call", "fixture", "echo", "--input-file", "-", "--timeout", "0", "--json")
	if err != nil {
		t.Fatal(err)
	}
	rec := loadCommandPluginRun(t, log)
	var result struct {
		RunID            string            `json:"run_id"`
		Plugin           string            `json:"plugin"`
		Version          string            `json:"version"`
		Capability       string            `json:"capability"`
		Status           kernel.Status     `json:"status"`
		Output           json.RawMessage   `json:"output"`
		Error            string            `json:"error"`
		ReportedEvidence []kernel.Evidence `json:"reported_evidence"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.RunID != rec.ID || result.Plugin != "fixture" || result.Version != fixturePluginVersion ||
		result.Capability != "echo" || result.Status != kernel.StatusOK || !jsonEqual(result.Output, `{"stdin":true}`) ||
		result.Error != "" || len(result.ReportedEvidence) != 1 || rec.Task.Timeout != 0 {
		t.Fatalf("call JSON: %s", out)
	}
	out, log, err = runPluginCommand(t, "", 1, "call", "fixture", "fail", "--json")
	if err != nil {
		t.Fatal(err)
	}
	rec = loadCommandPluginRun(t, log)
	var failure map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &failure); err != nil {
		t.Fatal(err)
	}
	if string(failure["run_id"]) != fmt.Sprintf("%q", rec.ID) || string(failure["status"]) != `"error"` ||
		string(failure["error"]) != `"internal: fixture failure"` || failure["output"] != nil {
		t.Fatalf("failed call JSON: %s", out)
	}
}

func TestPluginCallRejectsInvalidInputBeforeRecordingARun(t *testing.T) {
	home := useEmptyPluginHome(t)
	installCommandPlugin(t, home, "fixture")
	for _, args := range [][]string{
		{"--input", "{}", "--input-file", "-"},
		{"--input", "[]"},
		{"--input", "{"},
		{"--input="},
		{"--timeout", "-1s"},
	} {
		_, log, err := runPluginCommand(t, "", 2, append([]string{"call", "fixture", "echo"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "hrn plugin call") || strings.Contains(log, "run ") {
			t.Errorf("%v: stderr %q, err %v", args, log, err)
		}
	}
	const maxLine = pluginproto.MaxLine
	input := "{}" + strings.Repeat(" ", maxLine-1)
	_, _, err := runPluginCommand(t, input, 2, "call", "fixture", "echo", "--input-file", "-")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize input: %v", err)
	}
	requireNoPluginRuns(t)
}

func TestPluginCallRejectsAnUnknownCapabilityWithoutRecordingARun(t *testing.T) {
	home := useEmptyPluginHome(t)
	installCommandPlugin(t, home, "fixture")
	_, log, err := runPluginCommand(t, "", 2, "call", "fixture", "unknown")
	if err == nil || !strings.Contains(err.Error(), `no capability "unknown"`) || strings.Contains(log, "run ") {
		t.Fatalf("unknown capability: stderr %q, err %v", log, err)
	}
	requireNoPluginRuns(t)
}

func TestPluginCallRejectsInputWhoseCallLineWouldPassTheLimit(t *testing.T) {
	home := useEmptyPluginHome(t)
	installCommandPlugin(t, home, "fixture")
	// The input alone fits the limit; the call line around it does not. Whitespace would not
	// do: the call line holds the input compacted.
	input := `{"a":"` + strings.Repeat("x", pluginproto.MaxLine-20) + `"}`
	_, log, err := runPluginCommand(t, "", 2, "call", "fixture", "echo", "--input", input)
	if err == nil || !strings.Contains(err.Error(), "call line would be") || strings.Contains(log, "run ") {
		t.Fatalf("framed size: stderr %q, err %v", log, err)
	}
	requireNoPluginRuns(t)
}

func TestPluginCallRecordsACanceledRunWhenInterrupted(t *testing.T) {
	home := useEmptyPluginHome(t)
	installCommandPlugin(t, home, "fixture")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	// Interrupt once the run is recorded: a fixed delay could land during a slow handshake.
	log := &cancelOnRunWriter{cancel: cancel}
	code, err := cmdPluginContext(ctx, strings.NewReader(""), &out, log, []string{"call", "fixture", "wait", "--timeout", "0"})
	if code != 1 || err != nil {
		t.Fatalf("interrupted call: exit %d, err %v, stderr %q", code, err, log.String())
	}
	rec := loadCommandPluginRun(t, log.String())
	if !rec.Done || rec.Result.Status != kernel.StatusCanceled {
		t.Fatalf("result: %+v", rec.Result)
	}
}

type cancelOnRunWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelOnRunWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), "run ") {
		w.cancel()
	}
	return n, err
}

func (w *cancelOnRunWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestPluginCallEscapesCharactersATerminalMayActOnWithoutChangingTheData(t *testing.T) {
	home := useEmptyPluginHome(t)
	installCommandPlugin(t, home, "fixture")
	var want map[string]string
	if err := json.Unmarshal([]byte(fancyOutput), &want); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"call", "fixture", "fancy"}, {"call", "fixture", "fancy", "--json"}} {
		out, _, err := runPluginCommand(t, "", 0, args...)
		if err != nil || strings.ContainsRune(out, 0x9b) || strings.ContainsRune(out, 0x202e) || strings.ContainsRune(out, 0x061c) || !strings.Contains(out, `\u`+"009b") || !strings.Contains(out, `\u`+"202e") || !strings.Contains(out, `\u`+"061c") {
			t.Fatalf("%v: stdout %q, err %v", args, out, err)
		}
		var got map[string]string
		if len(args) == 3 {
			err = json.Unmarshal([]byte(out), &got)
		} else {
			var envelope struct {
				Output map[string]string `json:"output"`
			}
			err = json.Unmarshal([]byte(out), &envelope)
			got = envelope.Output
		}
		if err != nil || got["text"] != want["text"] {
			t.Fatalf("%v: decoded %q, %v; want %q", args, got, err, want)
		}
	}
}

func TestPluginCallDoesNotRecordMissingPluginsOrRefusedHandshakes(t *testing.T) {
	home := useEmptyPluginHome(t)
	_, _, err := runPluginCommand(t, "", 1, "call", "missing", "echo")
	if err == nil || !strings.Contains(err.Error(), "hrn plugin install OWNER/hrn-missing") {
		t.Fatalf("missing call: %v", err)
	}
	requireNoPluginRuns(t)
	installCommandPlugin(t, home, "refuses")
	_, _, err = runPluginCommand(t, "", 1, "call", "refuses", "echo")
	if err == nil || !strings.Contains(err.Error(), "unsupported_protocol") {
		t.Fatalf("refused handshake: %v", err)
	}
	requireNoPluginRuns(t)
}

func requireNoPluginRuns(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(runlog.Root())
	if len(entries) != 0 || (err != nil && !errors.Is(err, os.ErrNotExist)) {
		t.Fatalf("run directory: %v, %v", entries, err)
	}
}
