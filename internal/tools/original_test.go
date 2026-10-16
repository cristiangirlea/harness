package tools

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

func TestTheMachineCapCountsOtherRunsOriginals(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
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
  "a/b": 1, "m~n": 2, "e": 1E3, "": "empty key", "~1": "tilde one", "/": "slash"}`)
	for pointer, want := range map[string]string{
		"/items/1": `{"name":"b","n":1.50}`, "/items/0/name": `"a"`, "/items/1/n": "1.50",
		"/a~1b": "1", "/m~0n": "2", "/e": "1E3", "/": `"empty key"`,
		"/~01": `"tilde one"`, "/~1": `"slash"`,
	} {
		out, err := call(t, ws, ReadOriginal(), map[string]any{"handle": handle, "pointer": pointer})
		must(t, err)
		if out != want {
			t.Errorf("pointer %q = %q, want %q", pointer, out, want)
		}
	}
	for pointer, says := range map[string]string{
		"/missing": `no key "missing"`, "/items/2": `no index "2" in the 2-element array at /items`,
		"/items/01": `no index "01"`, "/items/-": `no index "-"`, "/items/0/name/x": "not an object or array",
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

func TestConcurrentIdenticalOriginalWritesLeaveOneCompleteFile(t *testing.T) {
	ws, _ := setup(t)
	ws.RunDir = t.TempDir()
	text := strings.Repeat("same bytes\n", 5000)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			if _, err := ws.storeOriginal(text); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(ws.RunDir, "originals"))
	must(t, err)
	if len(entries) != 1 || entries[0].Name() != originalHandleOf(text)+".txt" {
		t.Fatalf("left %v", entries)
	}
	data, err := os.ReadFile(filepath.Join(ws.RunDir, "originals", entries[0].Name()))
	must(t, err)
	if string(data) != text {
		t.Fatal("the original is incomplete")
	}
}

func TestAShortWriteWithoutAnErrorPublishesNothing(t *testing.T) {
	ws, _ := setup(t)
	ws.RunDir = t.TempDir()
	_, err := ws.storeOriginalWithWriter("complete original", func(w io.Writer, s string) (int, error) {
		return io.WriteString(w, s[:len(s)-1])
	})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write not reported: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(ws.RunDir, "originals"))
	must(t, err)
	if len(entries) != 0 {
		t.Fatalf("a short write left %v", entries)
	}
}

func TestAFailedRenameRemovesTheTemporaryFile(t *testing.T) {
	ws, _ := setup(t)
	ws.RunDir = t.TempDir()
	text := "complete original"
	dir := filepath.Join(ws.RunDir, "originals")
	_, err := ws.storeOriginalWithWriter(text, func(w io.Writer, s string) (int, error) {
		// A directory taking the name after the check makes the rename fail.
		must(t, os.MkdirAll(filepath.Join(dir, originalHandleOf(text)+".txt", "inside"), 0o755))
		return io.WriteString(w, s)
	})
	if err == nil {
		t.Fatal("rename onto a directory succeeded")
	}
	entries, err := os.ReadDir(dir)
	must(t, err)
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("a failed rename left %v", entries)
	}
}

func BenchmarkShapeNearCeiling(b *testing.B) {
	var sb strings.Builder
	for i := 0; sb.Len() < maxCapture-200; i++ {
		fmt.Fprintf(&sb, "2026-10-16T10:00:00Z INFO request %d served in 3ms\n", i)
		if i%5000 == 0 {
			fmt.Fprintf(&sb, "2026-10-16T10:00:01Z WARN request %d retried\n", i)
		}
	}
	text := sb.String()
	b.ReportAllocs()
	b.SetBytes(int64(len(text)))
	for b.Loop() {
		b.StopTimer()
		ws := &Workspace{ShapeTools: true, RunDir: filepath.Join(b.TempDir(), "runs", "run")}
		if err := os.MkdirAll(ws.RunDir, 0o755); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if out := ws.toolOutput(text); !strings.Contains(out, "original: ") {
			b.Fatal("near-ceiling output was not stored")
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
