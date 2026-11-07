//go:build unix

package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWriteFileRefusesAParentSymlinkSwapBeforeCreatingDirectories(t *testing.T) {
	writeFileRefusesParentSwapBeforeCreatingDirectories(t, func(t *testing.T, link, target string) {
		must(t, os.Symlink(target, link))
	})
}

func TestWriteAndEditFileRefuseAFIFOWithoutBlocking(t *testing.T) {
	for _, tool := range []string{"write_file", "edit_file"} {
		t.Run(tool, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperFileToolsRefuseFIFO$")
			cmd.Env = append(os.Environ(), "HRN_TEST_FIFO_TOOL="+tool)
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("%s blocked on FIFO for 5 seconds: %s", tool, out)
			}
			if err != nil {
				t.Fatalf("%s: %v: %s", tool, err, out)
			}
		})
	}
}

func TestHelperFileToolsRefuseFIFO(t *testing.T) {
	tool := os.Getenv("HRN_TEST_FIFO_TOOL")
	if tool == "" {
		return
	}
	ws, tl := setup(t)
	must(t, syscall.Mkfifo(filepath.Join(ws.Root, "pipe"), 0o600))
	// Both ends held open, so only the regular-file check can refuse the open.
	held, err := os.OpenFile(filepath.Join(ws.Root, "pipe"), os.O_RDWR, 0)
	must(t, err)
	defer held.Close()
	_, err = call(t, ws, tl[tool], map[string]any{"path": "pipe", "content": "changed", "old_string": "original", "new_string": "changed"})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("FIFO error = %v; want a not-a-regular-file refusal", err)
	}
}

func TestFileToolsRefuseAParentSymlinkSwapOutsideTheWorkspace(t *testing.T) {
	fileToolsRefuseParentSwap(t, func(t *testing.T, link, target string) {
		must(t, os.Symlink(target, link))
	})
}

func TestReadAndWriteFileRefuseAFinalSymlinkSwapOutsideTheWorkspace(t *testing.T) {
	for _, tool := range []string{"write_file", "read_file"} {
		t.Run(tool, func(t *testing.T) {
			ws, tl := setup(t)
			p := filepath.Join(ws.Root, "file.txt")
			outside := filepath.Join(t.TempDir(), "file.txt")
			must(t, os.WriteFile(p, []byte("original"), 0o644))
			must(t, os.WriteFile(outside, []byte("outside"), 0o644))
			fired := false
			beforeOpenInside = func(string) {
				beforeOpenInside = nil
				fired = true
				must(t, os.Remove(p))
				must(t, os.Symlink(outside, p))
			}
			t.Cleanup(func() { beforeOpenInside = nil })
			_, err := call(t, ws, tl[tool], map[string]any{"path": "file.txt", "content": "changed"})
			if !fired || err == nil {
				t.Errorf("swap called = %v, error = %v; want swap and refusal", fired, err)
			}
			data, err := os.ReadFile(outside)
			must(t, err)
			if string(data) != "outside" {
				t.Fatalf("outside file changed: %q", data)
			}
		})
	}
}

func TestFileToolsRefuseASecretSymlinkSwappedInInsideTheWorkspace(t *testing.T) {
	fileToolsRefuseASecretSwappedInInsideTheWorkspace(t, relativeSymlink)
}

func TestOpenInsideRefusesASecretReachedThroughASymlink(t *testing.T) {
	openInsideRefusesASecretReachedThroughALink(t, relativeSymlink)
}
