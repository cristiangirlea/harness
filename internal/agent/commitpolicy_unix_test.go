//go:build unix

package agent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestRunRefusesAnHrnRulesFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".hrn"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, ".hrn", "rules.md"), 0o644); err != nil {
		t.Skipf("cannot make a FIFO here: %v", err)
	}
	done := make(chan string, 1)
	go func() { done <- readRules(dir) }()
	select {
	case got := <-done:
		if got != "" {
			t.Errorf("readRules = %q, want nothing from a FIFO", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readRules blocked on a FIFO named rules.md")
	}
}
