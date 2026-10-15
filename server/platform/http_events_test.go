package platform

import (
	"sync"
	"testing"
)

func TestEventStreamsLimitEachTenantAndReleaseSlots(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	var streams eventStreams
	for i := 0; i < 4; i++ {
		if !streams.acquire("tenant-a") {
			t.Fatalf("stream %d refused before the cap", i+1)
		}
	}
	if streams.acquire("tenant-a") {
		t.Fatal("fifth stream accepted")
	}
	if !streams.acquire("tenant-b") {
		t.Fatal("another tenant shared the cap")
	}
	streams.release("tenant-a")
	if !streams.acquire("tenant-a") {
		t.Fatal("released slot was not reusable")
	}
	for i := 0; i < 4; i++ {
		streams.release("tenant-a")
	}
	streams.release("tenant-b")
	if len(streams.active) != 0 {
		t.Fatalf("idle tenants retained: %v", streams.active)
	}
}

func TestEventStreamsEnforceTheCapUnderConcurrentRequests(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	var streams eventStreams
	var wg sync.WaitGroup
	accepted := make(chan bool, 32)
	for i := 0; i < cap(accepted); i++ {
		wg.Go(func() { accepted <- streams.acquire("tenant") })
	}
	wg.Wait()
	close(accepted)
	count := 0
	for ok := range accepted {
		if ok {
			count++
		}
	}
	if count != 4 {
		t.Fatalf("accepted %d concurrent streams, want 4", count)
	}
}

func TestOnlyCompletedJobStatesAreTerminal(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, state := range []State{Succeeded, Failed, Canceled, Queued, Admitted, Running, WaitingApproval, RecoveryRequired, "", "unknown"} {
		t.Run(string(state), func(t *testing.T) {
			want := state == Succeeded || state == Failed || state == Canceled
			if got := state.terminal(); got != want {
				t.Fatalf("terminal(%q) = %v, want %v", state, got, want)
			}
		})
	}
}
