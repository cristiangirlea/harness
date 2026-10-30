package tools

import (
	"context"
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
	"time"

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
	_, _, err = CopyOriginals(from, to)
	must(t, err)
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
	if _, _, err := CopyOriginals(t.TempDir(), to); err != nil {
		t.Fatalf("a run without originals is not an error: %v", err)
	}
	_, _, err = CopyOriginals(from, to)
	must(t, err)
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

// writeOriginalFile puts text under handle in runDir's originals, as a store would.
func writeOriginalFile(t *testing.T, runDir, handle, text string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Join(runDir, "originals"), 0o755))
	must(t, os.WriteFile(filepath.Join(runDir, "originals", handle+".txt"), []byte(text), 0o644))
}

func TestTheRunCapRefusesANewOriginalAndKeepsEarlierOnesReadable(t *testing.T) {
	t.Setenv("HRN_ORIGINALS_RUN_MAX", "120000")
	ws, _ := setup(t)
	ws.ShapeTools, ws.RunDir = true, filepath.Join(t.TempDir(), "runs", "run")
	must(t, os.MkdirAll(ws.RunDir, 0o755))
	first := "count 6000\n" + strings.Repeat("working\n", 6000)
	if got := ws.toolOutput(first); !strings.Contains(got, "original: "+originalHandleOf(first)) {
		t.Fatalf("first original not stored: %q", got)
	}
	second := "count 9000\n" + strings.Repeat("waiting\n", 9000)
	if got := ws.toolOutput(second); got != Truncate(second) {
		t.Fatal("a refused original did not fall back to the head-and-tail cut")
	}
	if _, err := os.Stat(filepath.Join(ws.RunDir, "originals", originalHandleOf(second)+".txt")); !os.IsNotExist(err) {
		t.Fatal("stored an original past the run cap")
	}
	out, err := call(t, ws, ReadOriginal(), map[string]any{"handle": originalHandleOf(first)})
	must(t, err)
	if out != first {
		t.Fatal("the earlier original is no longer readable")
	}
	if got := ws.toolOutput(first); !strings.Contains(got, "original: ") {
		t.Fatal("an original already held was refused")
	}
}

func TestARunCapOfZeroRefusesNewOriginalsButReturnsOnesAlreadyHeld(t *testing.T) {
	t.Setenv("HRN_ORIGINALS_RUN_MAX", "0")
	ws, _ := setup(t)
	ws.ShapeTools, ws.RunDir = true, filepath.Join(t.TempDir(), "runs", "run")
	held := "count 6000\n" + strings.Repeat("working\n", 6000)
	writeOriginalFile(t, ws.RunDir, originalHandleOf(held), held)
	if got := ws.toolOutput(held); !strings.Contains(got, "original: "+originalHandleOf(held)) {
		t.Fatalf("an original already held was refused: %q", got)
	}
	fresh := "count 6000\n" + strings.Repeat("waiting\n", 6000)
	if got := ws.toolOutput(fresh); got != Truncate(fresh) {
		t.Fatal("a cap of 0 stored a new original")
	}
}

func TestConcurrentStoresOnOneRunStayUnderItsCap(t *testing.T) {
	t.Setenv("HRN_ORIGINALS_RUN_MAX", "120000")
	ws, _ := setup(t)
	ws.ShapeTools, ws.RunDir = true, filepath.Join(t.TempDir(), "runs", "run")
	must(t, os.MkdirAll(ws.RunDir, 0o755))
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ws.toolOutput("count 6000\n" + strings.Repeat(fmt.Sprintf("job%04d\n", i), 6000))
		}()
	}
	wg.Wait()
	entries, err := os.ReadDir(filepath.Join(ws.RunDir, "originals"))
	must(t, err)
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		must(t, err)
		total += info.Size()
	}
	if len(entries) == 0 || total > 120000 {
		t.Fatalf("%d originals, %d bytes, against a cap of 120000", len(entries), total)
	}
}

func TestTheMachineCapCountsOtherRunsOriginals(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	runs := filepath.Join(home, "runs")
	writeOriginalFile(t, filepath.Join(runs, "other"), "0123456789abcdef", strings.Repeat("o", 60000))
	text := "count 6000\n" + strings.Repeat("working\n", 6000)
	for _, tc := range []struct {
		limit  string
		stored bool
	}{{"100000", false}, {"120000", true}, {"a lot", true}, {"-1", true}} {
		t.Run(tc.limit, func(t *testing.T) {
			t.Setenv("HRN_ORIGINALS_MAX", tc.limit)
			ws, _ := setup(t)
			ws.ShapeTools, ws.RunDir = true, filepath.Join(runs, "mine")
			must(t, os.MkdirAll(ws.RunDir, 0o755))
			defer os.RemoveAll(ws.RunDir)
			got := ws.toolOutput(text)
			if stored := strings.Contains(got, "original: "); stored != tc.stored {
				t.Fatalf("stored=%v under HRN_ORIGINALS_MAX=%q", stored, tc.limit)
			}
			if !tc.stored && got != Truncate(text) {
				t.Fatal("a refused original did not fall back to the cut")
			}
		})
	}
}

func TestCopiedOriginalsAreNeverRefusedButCountTowardTheRunCap(t *testing.T) {
	t.Setenv("HRN_ORIGINALS_RUN_MAX", "60000")
	runs := filepath.Join(t.TempDir(), "runs")
	earlier := strings.Repeat("e", 70000)
	writeOriginalFile(t, filepath.Join(runs, "earlier"), originalHandleOf(earlier), earlier)
	ws, _ := setup(t)
	ws.ShapeTools, ws.RunDir = true, filepath.Join(runs, "next")
	must(t, os.MkdirAll(ws.RunDir, 0o755))
	n, _, err := CopyOriginals(filepath.Join(runs, "earlier"), ws.RunDir)
	if err != nil || n != 1 {
		t.Fatalf("copied %d: %v", n, err)
	}
	out, err := call(t, ws, ReadOriginal(), map[string]any{"handle": originalHandleOf(earlier), "range": "0-3"})
	must(t, err)
	if out != "eee" {
		t.Fatal("the copied original is not readable")
	}
	text := "count 4000\n" + strings.Repeat("working\n", 4000)
	if got := ws.toolOutput(text); got != Truncate(text) {
		t.Fatal("the copy did not count toward the run cap")
	}
}

func TestCopyOriginalsCopiesOnlyFilesNamedForTheirContent(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	good := "a real original"
	writeOriginalFile(t, from, originalHandleOf(good), good)
	writeOriginalFile(t, from, "0123456789abcdef", "bytes that hash to something else")
	must(t, os.WriteFile(filepath.Join(from, "originals", "notes.txt"), []byte("x"), 0o644))
	must(t, os.WriteFile(filepath.Join(from, "originals", ".original-x.tmp"), []byte("x"), 0o644))
	must(t, os.Mkdir(filepath.Join(from, "originals", "fedcba9876543210.txt"), 0o755))
	outside := filepath.Join(t.TempDir(), "outside.txt")
	must(t, os.WriteFile(outside, []byte("outside"), 0o644))
	linked := os.Symlink(outside, filepath.Join(from, "originals", originalHandleOf("outside")+".txt")) == nil
	n, skipped, err := CopyOriginals(from, to)
	must(t, err)
	entries, err := os.ReadDir(filepath.Join(to, "originals"))
	must(t, err)
	if n != 1 || len(entries) != 1 || entries[0].Name() != originalHandleOf(good)+".txt" {
		t.Fatalf("copied %d, left %v (link made: %v)", n, entries, linked)
	}
	wantSkipped := 2 // the wrong hash and the directory; notes and temporary files are not originals
	if linked {
		wantSkipped++
	}
	if skipped != wantSkipped {
		t.Fatalf("skipped %d, want %d (link made: %v)", skipped, wantSkipped, linked)
	}
	data, err := os.ReadFile(filepath.Join(to, "originals", entries[0].Name()))
	must(t, err)
	if string(data) != good {
		t.Fatal("copy changed the bytes")
	}
	if n, _, err := CopyOriginals(from, to); err != nil || n != 0 {
		t.Fatalf("a second copy wrote %d: %v", n, err)
	}
	if n, _, err := CopyOriginals(t.TempDir(), t.TempDir()); err != nil || n != 0 {
		t.Fatalf("a run without originals copied %d: %v", n, err)
	}
}

func TestAnOriginalLargerThanAShellCaptureIsCarriedOver(t *testing.T) {
	t.Setenv("HRN_ORIGINALS_RUN_MAX", "1")
	from, to := t.TempDir(), t.TempDir()
	text := strings.Repeat("m", maxCapture+1)
	writeOriginalFile(t, from, originalHandleOf(text), text)
	n, skipped, err := CopyOriginals(from, to)
	if err != nil || n != 1 || skipped != 0 {
		t.Fatalf("copied %d, skipped %d: %v", n, skipped, err)
	}
	ws, _ := setup(t)
	ws.RunDir = to
	out, err := call(t, ws, ReadOriginal(), map[string]any{"handle": originalHandleOf(text), "range": fmt.Sprintf("%d-%d", maxCapture-2, maxCapture+1)})
	must(t, err)
	if out != "mmm" {
		t.Fatalf("the continued run read %q", out)
	}
}

func TestReadOriginalReturnsTheAskedLinesWithTheirEndings(t *testing.T) {
	ws, _ := setup(t)
	ws.RunDir = t.TempDir()
	withNewline, without := "0000000000000001", "0000000000000002"
	writeOriginalFile(t, ws.RunDir, withNewline, "one\ntwo\nthree\n")
	writeOriginalFile(t, ws.RunDir, without, "one\ntwo\nthree")
	crlf := "0000000000000006"
	writeOriginalFile(t, ws.RunDir, crlf, "a\r\nb\r\n")
	for _, tc := range []struct{ handle, lines, want string }{
		{withNewline, "1-1", "one\n"}, {withNewline, "3-3", "three\n"}, {withNewline, "1-3", "one\ntwo\nthree\n"},
		{without, "3-3", "three"}, {without, "2-3", "two\nthree"}, {without, "1-1", "one\n"},
		{crlf, "1-1", "a\r\n"}, {crlf, "2-2", "b\r\n"},
	} {
		out, err := call(t, ws, ReadOriginal(), map[string]any{"handle": tc.handle, "lines": tc.lines})
		must(t, err)
		if out != tc.want {
			t.Errorf("lines %s of %s = %q, want %q", tc.lines, tc.handle, out, tc.want)
		}
	}
	empty := "0000000000000007"
	writeOriginalFile(t, ws.RunDir, empty, "")
	if _, err := call(t, ws, ReadOriginal(), map[string]any{"handle": empty, "lines": "1-1"}); err == nil || !strings.Contains(err.Error(), "0 lines") {
		t.Errorf("lines of an empty original: error %v", err)
	}
	for _, lines := range []string{"3-4", "4-4", "0-1", "2-1", "1", "a-b", "1-99999999999999999999"} {
		_, err := call(t, ws, ReadOriginal(), map[string]any{"handle": withNewline, "lines": lines})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", lines)) {
			t.Errorf("lines %q: error %v does not name what was asked", lines, err)
		}
	}
}

func TestReadOriginalReturnsTheValueAtAJSONPointerAsWritten(t *testing.T) {
	ws, _ := setup(t)
	ws.RunDir = t.TempDir()
	handle := "0000000000000003"
	writeOriginalFile(t, ws.RunDir, handle, `{"items": [ {"name": "a"}, {"name": "b", "n": 1.50} ],
  "a/b": 1, "m~n": 2, "e": 1E3, "": "empty key", "~1": "tilde one", "/": "slash", "big": 1e999, "d": 1, "d": 2}`)
	for pointer, want := range map[string]string{
		"/items/1": `{"name":"b","n":1.50}`, "/items/0/name": `"a"`, "/items/1/n": "1.50",
		"/a~1b": "1", "/m~0n": "2", "/e": "1E3", "/": `"empty key"`,
		"/~01": `"tilde one"`, "/~1": `"slash"`, "/big": "1e999", "/d": "1",
	} {
		out, err := call(t, ws, ReadOriginal(), map[string]any{"handle": handle, "pointer": pointer})
		must(t, err)
		if out != want {
			t.Errorf("pointer %q = %q, want %q", pointer, out, want)
		}
	}
	for pointer, says := range map[string]string{
		"/missing": `no key "missing"`, "/items/2": `no index "2" in the 2-element array at /items`,
		"/items/01": `no index "01"`, "/items/-": `no index "-"`, "/items/0/name/x": "not an object or array", "/big/x": "not an object or array",
		"items": "invalid pointer", "/m~2n": "~ must be followed", "/m~": "~ must be followed",
	} {
		_, err := call(t, ws, ReadOriginal(), map[string]any{"handle": handle, "pointer": pointer})
		if err == nil || !strings.Contains(err.Error(), says) || !strings.Contains(err.Error(), fmt.Sprintf("%q", pointer)) {
			t.Errorf("pointer %q: error %v, want one naming it and saying %q", pointer, err, says)
		}
	}
	if out, err := call(t, ws, ReadOriginal(), map[string]any{"handle": handle, "pointer": ""}); err != nil || !strings.HasPrefix(out, `{"items": [`) {
		t.Fatalf("an empty pointer did not return the whole original: %q %v", out, err)
	}
	writeOriginalFile(t, ws.RunDir, "0000000000000004", "not json\n")
	if _, err := call(t, ws, ReadOriginal(), map[string]any{"handle": "0000000000000004", "pointer": "/a"}); err == nil || !strings.Contains(err.Error(), "not one JSON value") {
		t.Fatalf("pointer into a non-JSON original: %v", err)
	}
}

func TestADeepPointerCostsAboutAsMuchAsAShallowOne(t *testing.T) {
	const depth = 2000
	doc := strings.Repeat(`{"a":`, depth) + `"` + strings.Repeat("x", 1<<20) + `"` + strings.Repeat("}", depth)
	run := func(pointer string) time.Duration {
		best := time.Duration(1 << 62)
		for range 3 {
			start := time.Now()
			if _, err := originalPointer(context.Background(), []byte(doc), pointer); err != nil {
				t.Fatal(err)
			}
			best = min(best, time.Since(start))
		}
		return best
	}
	shallow, deep := run("/a"), run(strings.Repeat("/a", depth))
	if deep > 20*shallow {
		t.Errorf("a pointer %d deep took %v, one level %v", depth, deep, shallow)
	}
}

func TestAPointerWalkStopsWhenTheRunIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := originalPointer(ctx, []byte(`{"a":{"b":1}}`), "/a/b"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestAMeasurementThatCannotReadTheRunsFailsClosed(t *testing.T) {
	ws := &Workspace{RunDir: filepath.Join(t.TempDir(), "ru\x00ns", "mine")}
	if err := ws.measureOriginals(); err == nil || ws.originalsSeen {
		t.Fatalf("err = %v, seen = %v: an unreadable runs directory must not count as empty", err, ws.originalsSeen)
	}
	t.Setenv("HRN_HOME", t.TempDir())
	ws = &Workspace{RunDir: filepath.Join(t.TempDir(), "runs", "mine")}
	if err := ws.measureOriginals(); err != nil || !ws.originalsSeen || ws.originalsAll != 0 {
		t.Fatalf("a runs directory not made yet is empty: err = %v, seen = %v, all = %d", err, ws.originalsSeen, ws.originalsAll)
	}
}

func TestASiblingRunThatCannotBeReadFailsTheMeasurementClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	runs := filepath.Join(home, "runs")
	must(t, os.MkdirAll(filepath.Join(runs, "other"), 0o755))
	must(t, os.WriteFile(filepath.Join(runs, "other", "originals"), []byte("not a directory"), 0o644))
	ws := &Workspace{RunDir: filepath.Join(runs, "mine")}
	if err := ws.measureOriginals(); err == nil || ws.originalsSeen {
		t.Fatalf("err = %v, seen = %v: a sibling run that cannot be read must not count as empty", err, ws.originalsSeen)
	}
}

func TestReadOriginalRefusesTwoSelectors(t *testing.T) {
	ws, _ := setup(t)
	ws.RunDir = t.TempDir()
	writeOriginalFile(t, ws.RunDir, "0000000000000005", "[1]\n")
	for _, args := range []map[string]any{
		{"range": "0-1", "lines": "1-1"}, {"range": "0-1", "pointer": "/0"}, {"lines": "1-1", "pointer": "/0"},
	} {
		args["handle"] = "0000000000000005"
		if _, err := call(t, ws, ReadOriginal(), args); err == nil || !strings.Contains(err.Error(), "at most one") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestTheLargestRunCapStillCopiesOriginals(t *testing.T) {
	t.Setenv("HRN_ORIGINALS_RUN_MAX", "9223372036854775807")
	from, to := t.TempDir(), t.TempDir()
	text := "small original"
	writeOriginalFile(t, from, originalHandleOf(text), text)
	if n, skipped, err := CopyOriginals(from, to); err != nil || n != 1 || skipped != 0 {
		t.Fatalf("copied %d, skipped %d: %v", n, skipped, err)
	}
}

func TestContinuationOriginalsShareIdentityAndMachineUsage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	runs := filepath.Join(home, "runs")
	from, to := filepath.Join(runs, "first"), filepath.Join(runs, "second")
	text := strings.Repeat("a", 40000)
	writeOriginalFile(t, from, originalHandleOf(text), text)
	must(t, os.MkdirAll(to, 0755))
	n, _, err := CopyOriginals(from, to)
	must(t, err)
	if n != 1 {
		t.Fatalf("carried %d", n)
	}
	a, err := os.Stat(filepath.Join(from, "originals", originalHandleOf(text)+".txt"))
	must(t, err)
	b, err := os.Stat(filepath.Join(to, "originals", originalHandleOf(text)+".txt"))
	must(t, err)
	if !os.SameFile(a, b) {
		t.Fatal("continuation duplicated the original")
	}
	ws := &Workspace{RunDir: filepath.Join(t.TempDir(), "elsewhere")}
	must(t, ws.measureOriginals())
	if ws.originalsAll != int64(len(text)) {
		t.Fatalf("machine total %d", ws.originalsAll)
	}
}

func TestMachineMeasurementDoesNotFollowOriginalsDirectoryLink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	external := t.TempDir()
	must(t, os.WriteFile(filepath.Join(external, "x"), []byte("outside"), 0644))
	run := filepath.Join(home, "runs", "other")
	must(t, os.MkdirAll(run, 0755))
	if err := os.Symlink(external, filepath.Join(run, "originals")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	ws := &Workspace{RunDir: filepath.Join(home, "runs", "mine")}
	must(t, ws.measureOriginals())
	if ws.originalsAll != 0 {
		t.Fatalf("followed link: %d", ws.originalsAll)
	}
}

func TestCopyOriginalsFallsBackWhenLinkFails(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	text := "original content"
	writeOriginalFile(t, from, originalHandleOf(text), text)
	n, skipped, err := copyOriginals(from, to, func(string, string) error { return errors.New("link unavailable") })
	if err != nil || n != 1 || skipped != 0 {
		t.Fatalf("copied %d, skipped %d: %v", n, skipped, err)
	}
	a, err := os.Stat(filepath.Join(from, "originals", originalHandleOf(text)+".txt"))
	must(t, err)
	b, err := os.Stat(filepath.Join(to, "originals", originalHandleOf(text)+".txt"))
	must(t, err)
	if os.SameFile(a, b) {
		t.Fatal("fallback did not make a copy")
	}
	data, err := os.ReadFile(filepath.Join(to, "originals", originalHandleOf(text)+".txt"))
	must(t, err)
	if string(data) != text {
		t.Fatal("fallback changed content")
	}
}

func TestOriginalRefusalReportsCause(t *testing.T) {
	for _, tc := range []struct{ name, runCap, machineCap, want string }{
		{"run", "0", "100000", "run cap"}, {"machine", "100000", "0", "machine cap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			t.Setenv("HRN_ORIGINALS_RUN_MAX", tc.runCap)
			t.Setenv("HRN_ORIGINALS_MAX", tc.machineCap)
			ws, _ := setup(t)
			ws.RunDir = t.TempDir()
			ws.ShapeTools = true
			var events []string
			ws.OriginalInfo = func(s string) { events = append(events, s) }
			ws.toolOutput(strings.Repeat("x", 40000))
			if len(events) != 1 || !strings.Contains(events[0], tc.want) {
				t.Fatalf("events = %v", events)
			}
		})
	}
}

func TestOriginalRefusalReportsUnmeasurableMachineStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	must(t, os.WriteFile(filepath.Join(home, "runs"), []byte("not a directory"), 0644))
	ws, _ := setup(t)
	ws.RunDir = t.TempDir()
	ws.ShapeTools = true
	var events []string
	ws.OriginalInfo = func(s string) { events = append(events, s) }
	text := strings.Repeat("x", 40000)
	if ws.toolOutput(text) != Truncate(text) || len(events) != 1 || !strings.Contains(events[0], "cannot be measured") {
		t.Fatalf("events = %v", events)
	}
}

func TestDefaultRunCapRefusesOversizedOriginal(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_ORIGINALS_RUN_MAX", "")
	ws, _ := setup(t)
	ws.RunDir = t.TempDir()
	ws.ShapeTools = true
	var events []string
	ws.OriginalInfo = func(s string) { events = append(events, s) }
	text := strings.Repeat("x", defaultOriginalsRunMax+1)
	if ws.toolOutput(text) != Truncate(text) || len(events) != 1 || !strings.Contains(events[0], "run cap") {
		t.Fatalf("events = %v", events)
	}
}
