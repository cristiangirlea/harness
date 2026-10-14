package ws

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestHelperExit is a process that ends at once, so its pid names a process
// that has exited.
func TestHelperExit(t *testing.T) {
	if os.Getenv("HRN_WS_HELPER") != "exit" {
		return
	}
	var hold [1]byte
	_, _ = os.Stdin.Read(hold[:])
	os.Exit(0)
}

// exitedPID records the helper's start identity while it is held running,
// then lets it exit and returns its pid and start identity.
func exitedPID(t *testing.T) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperExit$")
	cmd.Env = append(os.Environ(), "HRN_WS_HELPER=exit")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	start, known := startID(cmd.Process.Pid)
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if !known && (runtime.GOOS == "windows" || runtime.GOOS == "linux") {
		t.Fatal("cannot read the running helper's start identity")
	}
	return cmd.Process.Pid, start
}

func TestAliveSeesThisProcessAndNotOneThatExited(t *testing.T) {
	if !alive(os.Getpid()) {
		t.Error("this process should be alive")
	}
	pid, start := exitedPID(t)
	if alive(pid) {
		if current, known := startID(pid); start == "" || !known || current == start {
			t.Errorf("pid %d has exited and was waited for, but alive says it runs", pid)
		}
	}
	if alive(0) || alive(-1) {
		t.Error("no pid is not a live process")
	}
}

func TestLeaseWithReusedPIDIsStale(t *testing.T) {
	pid := os.Getpid()
	id, known := startID(pid)
	if !known {
		t.Skip("process start identity is unavailable on this platform")
	}
	if (Lease{PID: pid, Start: id, Taken: time.Now().Add(-24 * time.Hour)}).stale() {
		t.Error("a process with the recorded start identity holds the lease despite its Taken time")
	}
	other := strings.SplitN(id, ":", 2)[0] + ":1"
	if other == id {
		other += "0"
	}
	if !(Lease{PID: pid, Start: other}).stale() {
		t.Error("a process with another start identity cannot hold the lease")
	}
	if (Lease{PID: pid}).stale() {
		t.Error("an old lease without Start falls back to pid liveness")
	}
	// A slot folder shared by Windows hrn and hrn under WSL: the other
	// platform's identity cannot be compared, so the live pid still holds.
	foreign := "linux:1"
	if strings.HasPrefix(id, "linux:") {
		foreign = "win:1"
	}
	if (Lease{PID: pid, Start: foreign}).stale() {
		t.Error("a start identity from another platform cannot tell the holder apart")
	}
}

func TestExitedHelperStartKeepsReusedPIDLeaseFree(t *testing.T) {
	_, start := exitedPID(t)
	if start == "" {
		t.Skip("process start identity is unavailable on this platform")
	}
	if (Lease{PID: os.Getpid(), Start: start}).processHolds() {
		t.Error("the live test process cannot hold a lease recorded for an exited helper")
	}
}

func TestStartIDForMissingPID(t *testing.T) {
	if id, known := startID(1 << 30); known || id != "" {
		t.Errorf("a nonexistent pid has start identity %q, %t", id, known)
	}
}
