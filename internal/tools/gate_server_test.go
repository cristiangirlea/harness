package tools

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/mcp"
)

// serveGateTools connects a client to the server in this process.
func serveGateTools(t *testing.T, ws *Workspace, shape bool) *mcp.Client {
	t.Helper()
	sum := ""
	if ws.GatePolicy != "" {
		var err error
		sum, err = GatePolicySum(ws.GatePolicy)
		must(t, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	toServer, fromClient := io.Pipe()
	toClient, fromServer := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = NewGateServer(ws, "test", shape, sum).Serve(ctx, toServer, fromServer)
	}()
	t.Cleanup(func() {
		fromClient.Close()
		fromServer.Close()
		<-done
	})
	client := &mcp.Client{}
	if err := client.Connect(ctx, toClient, fromClient); err != nil {
		t.Fatal(err)
	}
	return client
}

func gateToolNames(c *mcp.Client) []string {
	var names []string
	for _, tool := range c.Tools() {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

func TestGateServerOffersTheCommandToolAndReadsOriginalsOnlyWhenShaping(t *testing.T) {
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := gateToolNames(serveGateTools(t, ws, false)); !slices.Equal(got, []string{"run_command"}) {
		t.Fatalf("without shaping: %v", got)
	}
	if got := gateToolNames(serveGateTools(t, ws, true)); !slices.Equal(got, []string{"read_original", "run_command"}) {
		t.Fatalf("with shaping: %v", got)
	}
}

func TestGateServerRefusesWhatTheGuardRefusesWithoutRunningIt(t *testing.T) {
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := serveGateTools(t, ws, false)
	res, err := client.CallTool(context.Background(), "run_command", json.RawMessage(`{"command":"git push origin main"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text(), "git-push") {
		t.Fatalf("refusal: %+v", res)
	}
}

func TestGateServerReturnsACommandsOutputAndFailure(t *testing.T) {
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := serveGateTools(t, ws, false)
	ok, err := client.CallTool(context.Background(), "run_command", json.RawMessage(`{"command":"echo through-the-tool"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ok.IsError || !strings.Contains(ok.Text(), "through-the-tool") || !strings.Contains(ok.Text(), "[exit code 0]") {
		t.Fatalf("output: %+v", ok)
	}
	failed, err := client.CallTool(context.Background(), "run_command", json.RawMessage(`{"command":"echo before-failing; exit 3"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !failed.IsError || !strings.Contains(failed.Text(), "before-failing") || !strings.Contains(failed.Text(), "[exit code 3]") {
		t.Fatalf("failure keeps the output: %+v", failed)
	}
}

func TestGateServerRunsACommandThroughTheGateUnderTheRunsPolicy(t *testing.T) {
	ws, _, log := gatedWorkspace(t)
	client := serveGateTools(t, ws, false)
	res, err := client.CallTool(context.Background(), "run_command", json.RawMessage(`{"command":"echo through"}`))
	if err != nil || res.IsError {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if runs := gateRuns(t, log); len(runs) != 1 || !slices.Contains(runs[0], "--policy") {
		t.Fatalf("gate runs = %q, want one under the run's policy", runs)
	}
}

func TestGateServerKeepsHrnsGuardWhenTheGateWouldAllowTheCommand(t *testing.T) {
	ws, _, log := gatedWorkspace(t)
	client := serveGateTools(t, ws, false)
	res, err := client.CallTool(context.Background(), "run_command", json.RawMessage(`{"command":"git push origin main"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text(), "git-push") || len(gateRuns(t, log)) != 0 {
		t.Fatalf("result = %+v, gate runs = %q", res, gateRuns(t, log))
	}
}

func TestGateServerRunsNothingOnceThePolicyFileIsRewritten(t *testing.T) {
	ws, _, log := gatedWorkspace(t)
	client := serveGateTools(t, ws, false)
	must(t, os.WriteFile(ws.GatePolicy, []byte(`{"version":1,"default":"allow","rules":[]}`), 0o600))
	res, err := client.CallTool(context.Background(), "run_command", json.RawMessage(`{"command":"echo after-the-rewrite"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text(), "policy file was changed") || len(gateRuns(t, log)) != 0 {
		t.Fatalf("result = %+v, gate runs = %q", res, gateRuns(t, log))
	}
	must(t, os.Remove(ws.GatePolicy))
	if res, err = client.CallTool(context.Background(), "run_command", json.RawMessage(`{"command":"echo after-removal"}`)); err != nil || !res.IsError {
		t.Fatalf("after removal: %+v, %v", res, err)
	}
}

func TestGateServerRunsNothingTheGateRefuses(t *testing.T) {
	ws, _, log := gatedWorkspace(t)
	client := serveGateTools(t, ws, false)
	res, err := client.CallTool(context.Background(), "run_command", json.RawMessage(`{"command":"fake-refuse now"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || len(gateRuns(t, log)) != 0 {
		t.Fatalf("result = %+v, gate runs = %q", res, gateRuns(t, log))
	}
}
