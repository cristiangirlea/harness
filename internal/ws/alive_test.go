package ws

import (
	"os"
	"os/exec"
	"testing"
)

// TestHelperExit is a process that ends at once, so its pid names a process
// that has exited.
func TestHelperExit(t *testing.T) {
	if os.Getenv("HRN_WS_HELPER") != "exit" {
		return
	}
	os.Exit(0)
}

// exitedPID starts the test binary as a helper that exits, waits for it, and
// returns its pid.
func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperExit$")
	cmd.Env = append(os.Environ(), "HRN_WS_HELPER=exit")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestAliveSeesThisProcessAndNotOneThatExited(t *testing.T) {
	if !alive(os.Getpid()) {
		t.Error("this process should be alive")
	}
	if pid := exitedPID(t); alive(pid) {
		t.Errorf("pid %d has exited and was waited for, but alive says it runs", pid)
	}
	if alive(0) || alive(-1) {
		t.Error("no pid is not a live process")
	}
}
