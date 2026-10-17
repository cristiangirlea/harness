package tools

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"testing"

	"github.com/cristiangirlea/harness/internal/mcp"
)

func TestOriginalRoundTrip(t *testing.T) {
	ws, tl := setup(t)
	tl["read_original"] = ReadOriginal()
	ws.ShapeTools, ws.RunDir = true, t.TempDir()
	text := "count 10000\n" + strings.Repeat("working\n", 10000) + "done\n"
	got := ws.toolOutput(text)
	hash := sha256.Sum256([]byte(text))
	handle := fmt.Sprintf("%x", hash)[:16]
	if !strings.Contains(got, "[repeated 10000 times]") || !strings.HasSuffix(got, fmt.Sprintf("original: %s (%d bytes); read_original reads it", handle, len(text))) {
		t.Fatalf("summary = %q", got)
	}
	data, err := os.ReadFile(filepath.Join(ws.RunDir, "originals", handle+".txt"))
	must(t, err)
	if string(data) != text {
		t.Fatal("original changed")
	}
	if ws.toolOutput(text) != got {
		t.Fatal("repeat changed the handle")
	}
	if tl["read_original"].Effect != EffectRead {
		t.Fatal("read_original must be read-only")
	}
	for _, r := range []string{"", "0-0", "0-5", "5-10", fmt.Sprintf("%d-%d", len(text)-5, len(text))} {
		out, err := call(t, ws, tl["read_original"], map[string]any{"handle": handle, "range": r})
		must(t, err)
		want := text
		if r != "" {
			var a, b int
			fmt.Sscanf(r, "%d-%d", &a, &b)
			want = text[a:b]
		}
		if out != want {
			t.Fatalf("range %q: got %q", r, out)
		}
	}
	if got := ws.toolOutput("small"); got != "small" {
		t.Fatal(got)
	}
}

func TestOriginalFallback(t *testing.T) {
	text := strings.Repeat("working\n", 10000)
	for _, tc := range []struct {
		name    string
		enabled bool
		dir     string
	}{
		{"off", false, t.TempDir()}, {"no record", true, ""}, {"invariant", true, t.TempDir()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, _ := setup(t)
			ws.ShapeTools, ws.RunDir = tc.enabled, tc.dir
			want := Truncate(text)
			if tc.enabled && tc.dir != "" {
				hash := sha256.Sum256([]byte(text))
				want += fmt.Sprintf("\noriginal: %s (%d bytes); read_original reads it", fmt.Sprintf("%x", hash)[:16], len(text))
			}
			if got := ws.toolOutput(text); got != want {
				t.Fatal("fallback changed")
			}
			if tc.dir != "" {
				_, err := os.Stat(filepath.Join(tc.dir, "originals"))
				if tc.enabled {
					must(t, err)
				} else if !os.IsNotExist(err) {
					t.Fatal("wrote when disabled")
				}
			}
		})
	}
	ws, _ := setup(t)
	ws.ShapeTools = true
	ws.RunDir = filepath.Join(t.TempDir(), "file")
	must(t, os.WriteFile(ws.RunDir, []byte("blocked"), 0o644))
	if got := ws.toolOutput("count 10000\n" + text); got != Truncate("count 10000\n"+text) {
		t.Fatal("storage failure did not fall back")
	}
}

func TestOriginalRetrievalAfterSummaryRejection(t *testing.T) {
	ws, _ := setup(t)
	ws.ShapeTools, ws.RunDir = true, t.TempDir()
	text := strings.Repeat("x", 25000) + "middle" + strings.Repeat("y", 24994)
	got := ws.toolOutput(text)
	cut, handleLine, found := strings.Cut(got, "\noriginal: ")
	if !found || cut != Truncate(text) || strings.Contains(cut, "middle") {
		t.Fatal("rejected summary did not return a cut with an original handle")
	}
	handle, _, _ := strings.Cut(handleLine, " ")
	for _, r := range []string{"", "25000-25006"} {
		out, err := call(t, ws, ReadOriginal(), map[string]any{"handle": handle, "range": r})
		must(t, err)
		want := text
		if r != "" {
			want = "middle"
		}
		if out != want {
			t.Fatalf("range %q did not retrieve the saved original", r)
		}
	}
}

func TestReadOriginalRejectsInvalidInput(t *testing.T) {
	ws, tl := setup(t)
	tl["read_original"] = ReadOriginal()
	ws.RunDir = t.TempDir()
	for _, handle := range []string{"", "../12345678901234", "0123456789abcdeg", "0123456789abcdef/x", "0123456789abcde", "0123456789abcdef0", "0123456789ABCDEF", `0123456789abcde\f`} {
		if _, err := call(t, ws, tl["read_original"], map[string]any{"handle": handle}); err == nil {
			t.Errorf("accepted %q", handle)
		}
	}
	handle := "0123456789abcdef"
	if _, err := call(t, ws, tl["read_original"], map[string]any{"handle": handle}); err == nil || !strings.Contains(err.Error(), handle) {
		t.Fatalf("unknown error: %v", err)
	}
	must(t, os.MkdirAll(filepath.Join(ws.RunDir, "originals"), 0o755))
	must(t, os.WriteFile(filepath.Join(ws.RunDir, "originals", handle+".txt"), []byte("abc"), 0o644))
	for _, r := range []string{"-1-2", "2-1", "0-4", "1", "1-2-3", "x-2", "0-99999999999999999999999999", "99999999999999999999999999-2"} {
		if _, err := call(t, ws, tl["read_original"], map[string]any{"handle": handle, "range": r}); err == nil {
			t.Errorf("accepted range %q", r)
		}
	}
	if _, err := call(t, ws, tl["read_original"], map[string]any{"handle": handle, "range": 1}); err == nil {
		t.Fatal("accepted a range of the wrong type")
	}
	if _, err := tl["read_original"].Run(t.Context(), ws, []byte(`{"handle":`)); err == nil {
		t.Fatal("accepted bad JSON")
	}
	ws.RunDir = ""
	if _, err := call(t, ws, tl["read_original"], map[string]any{"handle": handle}); err == nil {
		t.Fatal("read without a record")
	}
}

func TestInterruptedOriginalWriteDoesNotPublish(t *testing.T) {
	ws, _ := setup(t)
	ws.RunDir = t.TempDir()
	text := "complete original"
	hash := sha256.Sum256([]byte(text))
	handle := fmt.Sprintf("%x", hash)[:16]
	name := filepath.Join(ws.RunDir, "originals", handle+".txt")
	interrupted := errors.New("interrupted write")
	_, err := ws.storeOriginalWithWriter(text, func(w io.Writer, s string) (int, error) {
		n, err := io.WriteString(w, s[:len(s)/2])
		must(t, err)
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Error("published the original before its contents were complete")
		}
		return n, interrupted
	})
	if !errors.Is(err, interrupted) {
		t.Fatalf("write error lost: %v", err)
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatal("interrupted write left a hash-named file")
	}
	files, err := os.ReadDir(filepath.Dir(name))
	must(t, err)
	if len(files) != 0 {
		t.Fatal("interrupted write left a temporary file")
	}
	got, err := ws.storeOriginal(text)
	must(t, err)
	if got != handle {
		t.Fatal("retry changed the handle")
	}
	data, err := os.ReadFile(name)
	must(t, err)
	if string(data) != text {
		t.Fatal("retry did not publish the complete original")
	}
}

func TestMCPShaping(t *testing.T) {
	ws, _ := setup(t)
	ws.ShapeTools, ws.RunDir = true, t.TempDir()
	text := "count 10000\n" + strings.Repeat("working\n", 10000)
	f := &fakeCaller{tools: []mcp.ToolInfo{{Name: "output"}}, result: mcp.CallResult{Content: []mcp.Content{mcp.TextContent(text)}}}
	tl := FromMCP("test", f)[0]
	for _, bad := range []bool{false, true} {
		f.result.IsError = bad
		out, err := call(t, ws, tl, map[string]any{})
		if bad {
			if err == nil {
				t.Fatal("lost MCP error")
			}
			out = err.Error()
		} else {
			must(t, err)
		}
		if !strings.Contains(out, "original:") {
			t.Fatal("MCP not shaped")
		}
	}
}

func TestFileReadsAreNotShaped(t *testing.T) {
	ws, tl := setup(t)
	ws.ShapeTools, ws.RunDir = true, t.TempDir()
	must(t, os.WriteFile(filepath.Join(ws.Root, "large.txt"), []byte(strings.Repeat("line\n", 10000)), 0o644))
	out, err := call(t, ws, tl["read_file"], map[string]any{"path": "large.txt", "limit": 10000})
	must(t, err)
	if strings.Contains(out, "original:") {
		t.Fatal("file read shaped")
	}
	if _, err := os.Stat(filepath.Join(ws.RunDir, "originals")); !os.IsNotExist(err) {
		t.Fatal("stored a file read")
	}
}

func TestOriginalRefusesSymlinkEscape(t *testing.T) {
	for _, directory := range []bool{false, true} {
		ws, tl := setup(t)
		tl["read_original"] = ReadOriginal()
		ws.RunDir = t.TempDir()
		outside := t.TempDir()
		name := "0123456789abcdef.txt"
		must(t, os.WriteFile(filepath.Join(outside, name), []byte("outside"), 0o644))
		link, target := filepath.Join(ws.RunDir, "originals"), outside
		if !directory {
			must(t, os.Mkdir(link, 0o755))
			link = filepath.Join(link, name)
			target = filepath.Join(outside, name)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		if _, err := call(t, ws, tl["read_original"], map[string]any{"handle": strings.TrimSuffix(name, ".txt")}); err == nil {
			t.Fatal("read escaped the originals directory")
		}
	}
}

func TestOriginalPreservesExistingBytes(t *testing.T) {
	ws, _ := setup(t)
	ws.ShapeTools, ws.RunDir = true, t.TempDir()
	text := "count 10000\n" + strings.Repeat("working\n", 10000)
	handle, err := ws.storeOriginal(text)
	must(t, err)
	p := filepath.Join(ws.RunDir, "originals", handle+".txt")
	must(t, os.WriteFile(p, []byte("different bytes"), 0o644))
	if _, err := ws.storeOriginal(text); err == nil {
		t.Fatal("replaced a conflicting original")
	}
	if got := ws.toolOutput(text); got != Truncate(text) {
		t.Fatal("conflicting original did not fall back")
	}
	data, err := os.ReadFile(p)
	must(t, err)
	if string(data) != "different bytes" {
		t.Fatal("existing original overwritten")
	}
}

func TestAtOutputCapDoesNotStore(t *testing.T) {
	ws, _ := setup(t)
	ws.ShapeTools, ws.RunDir = true, t.TempDir()
	text := strings.Repeat("x", maxOutput)
	if got := ws.toolOutput(text); got != text {
		t.Fatal("changed output at the cap")
	}
	if _, err := os.Stat(filepath.Join(ws.RunDir, "originals")); !os.IsNotExist(err) {
		t.Fatal("stored output at the cap")
	}
}

func TestConcurrentIdenticalOriginalWritesLeaveOneCompleteFile(t *testing.T) {
	ws, _ := setup(t)
	ws.RunDir = t.TempDir()
	text := strings.Repeat("same output\n", 5000)
	var wg sync.WaitGroup
	handles := make([]string, 16)
	errs := make([]error, len(handles))
	for i := range handles {
		wg.Go(func() { handles[i], errs[i] = ws.storeOriginal(text) })
	}
	wg.Wait()
	for i := range handles {
		if errs[i] != nil || handles[i] != handles[0] {
			t.Fatalf("write %d: handle %q err %v", i, handles[i], errs[i])
		}
	}
	files, err := os.ReadDir(filepath.Join(ws.RunDir, "originals"))
	must(t, err)
	if len(files) != 1 || files[0].Name() != handles[0]+".txt" {
		t.Fatalf("want one hash-named file, got %v", files)
	}
	data, err := os.ReadFile(filepath.Join(ws.RunDir, "originals", files[0].Name()))
	must(t, err)
	if string(data) != text {
		t.Fatal("original incomplete")
	}
}

func TestShortOriginalWriteWithoutAnErrorPublishesNothing(t *testing.T) {
	ws, _ := setup(t)
	ws.RunDir = t.TempDir()
	text := "complete original"
	_, err := ws.storeOriginalWithWriter(text, func(w io.Writer, s string) (int, error) {
		return io.WriteString(w, s[:len(s)-1])
	})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write not reported: %v", err)
	}
	files, err := os.ReadDir(filepath.Join(ws.RunDir, "originals"))
	must(t, err)
	if len(files) != 0 {
		t.Fatalf("short write left %v", files)
	}
}

func TestFailedOriginalRenameRemovesTheTemporaryFile(t *testing.T) {
	ws, _ := setup(t)
	ws.RunDir = t.TempDir()
	text := "complete original"
	hash := sha256.Sum256([]byte(text))
	name := filepath.Join(ws.RunDir, "originals", fmt.Sprintf("%x", hash)[:16]+".txt")
	_, err := ws.storeOriginalWithWriter(text, func(w io.Writer, s string) (int, error) {
		// A non-empty directory at the handle's name makes the rename fail.
		must(t, os.MkdirAll(filepath.Join(name, "blocker"), 0o755))
		return io.WriteString(w, s)
	})
	if err == nil {
		t.Fatal("rename over a directory succeeded")
	}
	files, err := os.ReadDir(filepath.Dir(name))
	must(t, err)
	if len(files) != 1 || !files[0].IsDir() {
		t.Fatalf("failed rename left a temporary file: %v", files)
	}
}

func TestCopyOriginalsCarriesOnlyHashNamedFiles(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	src := &Workspace{RunDir: from}
	text := strings.Repeat("earlier output\n", 3000)
	handle, err := src.storeOriginal(text)
	must(t, err)
	dir := filepath.Join(from, "originals")
	must(t, os.WriteFile(filepath.Join(dir, "0123456789abcdef.txt"), []byte("bytes that do not hash to the name"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not an original"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, ".original-x.tmp"), []byte("partial"), 0o644))
	outside := filepath.Join(t.TempDir(), "secret.txt")
	must(t, os.WriteFile(outside, []byte("outside"), 0o644))
	linked := os.Symlink(outside, filepath.Join(dir, "fedcba9876543210.txt")) == nil
	must(t, CopyOriginals(from, to))
	files, err := os.ReadDir(filepath.Join(to, "originals"))
	must(t, err)
	if len(files) != 1 || files[0].Name() != handle+".txt" {
		t.Fatalf("copied %v, want only %s.txt (symlink tried: %v)", files, handle, linked)
	}
	ws, _ := setup(t)
	ws.ShapeTools, ws.RunDir = true, to
	out, err := call(t, ws, ReadOriginal(), map[string]any{"handle": handle})
	must(t, err)
	if out != text {
		t.Fatal("carried original changed")
	}
	if err := CopyOriginals(t.TempDir(), to); err != nil {
		t.Fatalf("a run without originals is not an error: %v", err)
	}
	must(t, CopyOriginals(from, to))
}

func BenchmarkNearCeilingCaptureAndShaping(b *testing.B) {
	var sb strings.Builder
	for i := 0; sb.Len() < maxCapture-200; i++ {
		fmt.Fprintf(&sb, "=== RUN   TestCase%d\n--- PASS: TestCase%d (0.00s)\n", i, i)
	}
	sb.WriteString("--- FAIL: TestLast (0.00s)\nFAIL\n")
	text := sb.String()
	ws := &Workspace{ShapeTools: true, RunDir: b.TempDir()}
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	runtime.GC()
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	base, peak := sample[0].Value.Uint64(), uint64(0)
	done, sampled := make(chan struct{}), make(chan struct{})
	go func() { // live and unswept heap objects, polled while the loop runs
		defer close(sampled)
		s := []metrics.Sample{{Name: sample[0].Name}}
		for {
			select {
			case <-done:
				return
			default:
			}
			metrics.Read(s)
			peak = max(peak, s[0].Value.Uint64())
			runtime.Gosched()
		}
	}()
	defer func() {
		close(done)
		<-sampled
		b.ReportMetric(float64(peak-min(peak, base))/(1<<20), "peak-heap-MiB")
	}()
	for b.Loop() {
		out := &boundedBuffer{keepHead: maxOutput * 2 / 3, keepTail: maxOutput / 3, fullLimit: maxCapture}
		for chunk := text; chunk != ""; {
			n := min(len(chunk), 32<<10)
			_, _ = out.Write([]byte(chunk[:n]))
			chunk = chunk[n:]
		}
		if out.fullDropped || !strings.Contains(ws.toolOutput(out.String()), "read_original") {
			b.Fatal("near-ceiling output was not captured and shaped")
		}
	}
}
