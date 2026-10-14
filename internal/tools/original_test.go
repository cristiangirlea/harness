package tools

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/mcp"
)

func TestOriginalRoundTrip(t *testing.T) {
	ws, tl := setup(t)
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
			if got := ws.toolOutput(text); got != Truncate(text) {
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

func TestReadOriginalRejectsInvalidInput(t *testing.T) {
	ws, tl := setup(t)
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
