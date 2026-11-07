package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func oldReadFile(data []byte, offset, limit int) string {
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = 2000
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if offset > len(lines) {
		return fmt.Sprintf("(file has %d lines)", len(lines))
	}
	end := min(offset-1+limit, len(lines))
	var b strings.Builder
	for i := offset - 1; i < end; i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, lines[i])
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "(%d more lines; continue with offset=%d)\n", len(lines)-end, end+1)
	}
	return Truncate(b.String())
}

func TestReadFilePreservesTheOriginalLineOutput(t *testing.T) {
	ws, _ := setup(t)
	inputs := []string{"", "a", "a\n", "\n", "\n\n", "a\r\nb\r\n", "a\rb\r", "a\r\r\nb\n\r", "\x00\xff\n", strings.Repeat("x", 128*1024) + "\r\nend", strings.Repeat("line\n", 2100), strings.Repeat("x", 4095) + "\r\nlast"}
	for _, n := range []int{4094, 4095, 4096} {
		inputs = append(inputs, strings.Repeat("x", n)+"\r\nlast")
	}
	inputs = append(inputs, strings.Repeat("x", 4095)+"\r", strings.Repeat("x", 4095)+"\ry", strings.Repeat("x", 4095)+"\r\r\nlast")
	inputs = append(inputs, strings.Repeat("ăîșț€😀\n", 3000), strings.Repeat("😀", 20000), strings.Repeat("é", 15001)+"\n"+strings.Repeat("ș", 9000))
	for i, input := range inputs {
		must(t, os.WriteFile(filepath.Join(ws.Root, "input.txt"), []byte(input), 0o600))
		for _, offset := range []int{-2, 0, 1, 2, 3, 10, 2000, 2101, 2102} {
			for _, limit := range []int{-1, 0, 1, 2, 10, 2000, 3000} {
				t.Run(fmt.Sprintf("input%d/offset%d/limit%d", i, offset, limit), func(t *testing.T) {
					got, err := call(t, ws, readFile(), map[string]any{"path": "input.txt", "offset": offset, "limit": limit})
					must(t, err)
					if want := oldReadFile([]byte(input), offset, limit); got != want {
						t.Fatalf("output differs: got %q, want %q", got, want)
					}
				})
			}
		}
	}
}

func makeLargeReadFile(t *testing.T, ws *Workspace) {
	t.Helper()
	f, err := os.Create(filepath.Join(ws.Root, "large.txt"))
	must(t, err)
	defer f.Close()
	block := []byte(strings.Repeat(strings.Repeat("x", 99)+"\n", 10000))
	for range 20 {
		_, err := f.Write(block)
		must(t, err)
	}
	must(t, f.Close())
}

func TestReadFileAllocatesLessThanFourMegabytesForTenLinesOfALargeFile(t *testing.T) {
	ws, _ := setup(t)
	makeLargeReadFile(t, ws)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out, err := call(t, ws, readFile(), map[string]any{"path": "large.txt", "limit": 10})
	runtime.ReadMemStats(&after)
	must(t, err)
	if !strings.Contains(out, "(199991 more lines; continue with offset=11)") {
		t.Fatalf("unexpected output: %q", out)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated >= 4*1024*1024 {
		t.Fatalf("allocated %d bytes for ten lines", allocated)
	}
}

func TestReadFileRefusesDirectoriesAsNonRegularFiles(t *testing.T) {
	ws, _ := setup(t)
	must(t, os.Mkdir(filepath.Join(ws.Root, "directory"), 0o700))
	_, err := call(t, ws, readFile(), map[string]any{"path": "directory"})
	if err == nil || err.Error() != `"directory" is not a regular file` {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadFileKeepsMemorySmallForOneLongSelectedLine(t *testing.T) {
	ws, _ := setup(t)
	data := []byte(strings.Repeat("x", 20*1024*1024))
	must(t, os.WriteFile(filepath.Join(ws.Root, "long.txt"), data, 0o600))
	want := oldReadFile(data, 1, 1)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out, err := call(t, ws, readFile(), map[string]any{"path": "long.txt", "limit": 1})
	runtime.ReadMemStats(&after)
	must(t, err)
	if out != want {
		t.Fatal("long-line output differs from the original")
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated >= 4*1024*1024 {
		t.Fatalf("allocated %d bytes for one long selected line", allocated)
	}
}

type readFileCancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *readFileCancelContext) Err() error {
	c.checks++
	if c.checks == 3 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestReadFileReturnsTheCancelledContextErrorForALargeFile(t *testing.T) {
	ws, _ := setup(t)
	makeLargeReadFile(t, ws)
	for _, duringRead := range []bool{false, true} {
		t.Run(fmt.Sprintf("duringRead=%t", duringRead), func(t *testing.T) {
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			var ctx context.Context = base
			if duringRead {
				ctx = &readFileCancelContext{Context: base, cancel: cancel}
			} else {
				cancel()
			}
			_, err := readFile().Run(ctx, ws, json.RawMessage(`{"path":"large.txt","limit":1}`))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v, want context.Canceled", err)
			}
		})
	}
}
