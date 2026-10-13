package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// lockedBuffer is a renderer's output that a test reads while it writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *lockedBuffer) waitFor(t *testing.T, s string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(l.String(), s) {
		if time.Now().After(deadline) {
			t.Fatalf("output never showed %q:\n%s", s, l.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestShowFollowPrintsARunUntilItFinishes(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	start := time.Now()
	w, err := runlog.Create("stub-cmd", agent.Task{Prompt: "fix the build", Dir: "/repo", Options: agent.Options{Mode: agent.ModeAuto}})
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Time: start, Kind: agent.EventText, Text: "Looking at the build."})

	var out lockedBuffer
	r := &renderer{out: &out}
	done := make(chan error, 1)
	go func() { done <- followRun(context.Background(), r, w.ID(), 5*time.Millisecond) }()
	out.waitFor(t, "Looking at the build.")
	if !strings.Contains(out.String(), "fix the build") || !strings.Contains(out.String(), "mode auto") {
		t.Errorf("the header is missing:\n%s", out.String())
	}

	w.Event(agent.Event{Time: start.Add(12 * time.Second), Kind: agent.EventToolCall, Tool: "Bash", Text: "go build ./..."})
	out.waitFor(t, "→ Bash go build ./...")
	if !strings.Contains(out.String(), "+12.0s") {
		t.Errorf("a step that took a while shows how long it took:\n%s", out.String())
	}
	w.Event(agent.Event{Time: start.Add(12100 * time.Millisecond), Kind: agent.EventToolResult, Text: "ok\nsecond line\nthird"})
	out.waitFor(t, "✓ ok second line third")
	if strings.Contains(out.String(), "+0.1s") {
		t.Errorf("a quick step needs no gap line:\n%s", out.String())
	}
	select {
	case err := <-done:
		t.Fatalf("followRun returned while the run was going: %v", err)
	default:
	}

	if err := w.Close(agent.Result{Backend: "stub-cmd", Status: agent.StatusOK, Turns: 2, Duration: 13 * time.Second}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("followRun did not return after the run finished")
	}
	if !strings.Contains(out.String(), "── ok · stub-cmd") || !strings.Contains(out.String(), "run "+w.ID()) {
		t.Errorf("the summary is missing:\n%s", out.String())
	}
}

func TestShowFollowStopsQuietlyWhenInterrupted(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("stub-cmd", agent.Task{Prompt: "long job"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(agent.Result{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var out lockedBuffer
	if err := followRun(ctx, &renderer{out: &out}, w.ID(), 5*time.Millisecond); err != nil {
		t.Fatalf("an interrupted follow is not an error: %v", err)
	}
	if !strings.Contains(out.String(), "still going") {
		t.Errorf("it says the run goes on:\n%s", out.String())
	}
}

func TestShowRefusesFollowWithJSON(t *testing.T) {
	if err := cmdShow([]string{"--json", "--follow", "x"}); err == nil || !strings.Contains(err.Error(), "--follow") {
		t.Fatalf("err = %v", err)
	}
}

func TestWatchRefusesAnAddressBeyondThisComputer(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8089", ":8089", "192.168.1.2:8089"} {
		if err := cmdWatch([]string{"--addr", addr}); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("hrn watch --addr %s = %v; want it refused", addr, err)
		}
	}
}

func TestWatchPublicFlagsMustBeCompleteAndUseAnHTTPSOrigin(t *testing.T) {
	for _, args := range [][]string{
		{"--public", "https://harness.example.com"},
		{"--access-team", "team.cloudflareaccess.com"},
		{"--access-aud", "aud-tag"},
		{"--public", "http://harness.example.com", "--access-team", "team.cloudflareaccess.com", "--access-aud", "aud-tag"},
		{"--public", "https://harness.example.com/path", "--access-team", "team.cloudflareaccess.com", "--access-aud", "aud-tag"},
		{"--public", "https://harness.example.com", "--access-team", "team.cloudflareaccess.com", "--access-aud", "aud-tag", "--lan"},
	} {
		if err := cmdWatch(args); err == nil {
			t.Errorf("cmdWatch(%q) accepted invalid public flags", args)
		}
	}
}
