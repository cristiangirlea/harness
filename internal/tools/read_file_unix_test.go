//go:build unix

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadFileRefusesAFIFOWithoutBlocking(t *testing.T) {
	ws, _ := setup(t)
	must(t, syscall.Mkfifo(filepath.Join(ws.Root, "pipe"), 0o600))
	done := make(chan error, 1)
	go func() {
		_, err := readFile().Run(context.Background(), ws, json.RawMessage(`{"path":"pipe"}`))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || err.Error() != `"pipe" is not a regular file` {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}
}

func TestReadFileOpensAFIFOWithNoWriterWithoutBlocking(t *testing.T) {
	ws, _ := setup(t)
	pipe := filepath.Join(ws.Root, "pipe")
	must(t, syscall.Mkfifo(pipe, 0o600))
	done := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(pipe, os.O_RDONLY|readOpenFlags, 0)
		if err == nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("opening a FIFO with no writer blocked")
	}
}
