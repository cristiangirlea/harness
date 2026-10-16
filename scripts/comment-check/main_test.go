package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runCheck(t *testing.T, args ...string) (report, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	var r report
	if stdout.Len() > 0 {
		if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
			t.Fatalf("unmarshal stdout %q: %v", stdout.String(), err)
		}
	}
	return r, stderr.String(), code
}

func TestBlockLengthsAndSplit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ok.go", strings.Join([]string{
		"package p",
		"",
		"// one",
		"// two",
		"// three",
		"// four",
		"// five",
		"var a = 1",
		"",
		"// split one",
		"// split two",
		"// split three",
		"",
		"// split four",
		"// split five",
		"// split six",
		"var b = 2",
		"",
	}, "\n"))

	r, stderr, code := runCheck(t, "--root", dir)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", code, stderr)
	}
	if len(r.New) != 0 || r.Violations != 0 || r.Blocks != 3 {
		t.Fatalf("unexpected report: %+v", r)
	}

	writeFile(t, dir, "bad.go", strings.Join([]string{
		"package p",
		"",
		"// one",
		"// two",
		"// three",
		"// four",
		"// five",
		"// six",
		"var c = 3",
		"",
	}, "\n"))

	r, stderr, code = runCheck(t, "--root", dir)
	if code != 1 || !strings.Contains(stderr, `"error"`) {
		t.Fatalf("expected exit 1 with error JSON, got %d: %s", code, stderr)
	}
	if r.Violations != 1 || len(r.New) != 1 {
		t.Fatalf("expected 1 violation, got %+v", r)
	}
	if r.New[0].File != "bad.go" || r.New[0].Line != 3 || r.New[0].Lines != 6 || r.New[0].Reasons != "long" {
		t.Fatalf("unexpected violation entry: %+v", r.New[0])
	}
}

func TestDirectivesAndKeptHeaderExempt(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "script.go", strings.Join([]string{
		"// name: example-main",
		"// summary: Check #12 on 2026-10-16 after review.",
		"// effects: read",
		"// parameters: none",
		"// example: go test ./...",
		"// hash: 0123456789abcdef",
		"package main",
		"",
		"//go:build linux",
		"// +build linux",
		"//line foo.go:1",
		"//export Foo",
		"//nolint:errcheck // reviewed #12 on 2026-10-16",
		"// one",
		"// two",
		"// three",
		"// four",
		"// five",
		"func main() {}",
		"",
	}, "\n"))

	r, stderr, code := runCheck(t, "--root", dir)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s, new: %+v)", code, stderr, r.New)
	}
	if r.Violations != 0 || r.Blocks != 1 {
		t.Fatalf("expected 0 violations and 1 counted block, got %+v", r)
	}
}

func TestRawStringAndCitations(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "clean.go", strings.Join([]string{
		"package p",
		"",
		"var s = `",
		"// not a comment #12 2026-10-16 review",
		"// line 2",
		"// line 3",
		"// line 4",
		"// line 5",
		"// line 6",
		"`",
		"// Color #1e1e1e is dark grey.",
		"var color = \"#1e1e1e\"",
		"",
	}, "\n"))

	r, stderr, code := runCheck(t, "--root", dir)
	if code != 0 {
		t.Fatalf("expected clean exit 0, got %d (%s): %+v", code, stderr, r.New)
	}
	if r.Blocks != 1 || r.Violations != 0 {
		t.Fatalf("expected 1 block and 0 violations, got %+v", r)
	}

	writeFile(t, dir, "cite.go", strings.Join([]string{
		"package p",
		"",
		"var a = 1 // see #12 for background",
		"var b = 2 // added on 2026-10-16",
		"var c = 3 // Reviewed by owner",
		"",
	}, "\n"))

	r, _, code = runCheck(t, "--root", dir)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if len(r.New) != 3 {
		t.Fatalf("expected 3 cite violations, got %+v", r.New)
	}
	for i, wantLine := range []int{3, 4, 5} {
		if r.New[i].File != "cite.go" || r.New[i].Line != wantLine || r.New[i].Lines != 1 || r.New[i].Reasons != "cite" {
			t.Fatalf("entry %d: got %+v", i, r.New[i])
		}
	}
}

func TestIgnoredFilesAndDirs(t *testing.T) {
	dir := t.TempDir()
	bad := "package p\n// #12 reviewed 2026-10-16\n"
	writeFile(t, dir, "main_test.go", bad)
	writeFile(t, dir, ".hidden/x.go", bad)
	writeFile(t, dir, "testdata/x.go", bad)
	writeFile(t, dir, "vendor/x.go", bad)
	writeFile(t, dir, "dist/x.go", bad)
	writeFile(t, dir, "node_modules/x.go", bad)
	writeFile(t, dir, "sub/ok.go", "package sub\n")

	r, stderr, code := runCheck(t, "--root", dir)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (%s)", code, stderr)
	}
	if r.Files != 1 || r.Violations != 0 {
		t.Fatalf("expected 1 file and 0 violations, got %+v", r)
	}
}

func TestBaselineInitPruneAndMultiset(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.go", strings.Join([]string{
		"package p",
		"",
		"// duplicate #12",
		"var a = 1",
		"",
		"// duplicate #12",
		"var b = 2",
		"",
		"// second #34",
		"var c = 3",
		"",
	}, "\n"))

	r, stderr, code := runCheck(t, "--root", dir, "--init")
	if code != 0 {
		t.Fatalf("--init failed with %d: %s", code, stderr)
	}
	if r.Violations != 3 || r.Baselined != 3 || len(r.New) != 0 || len(r.Stale) != 0 {
		t.Fatalf("unexpected report after --init: %+v", r)
	}

	// --init refuses when baseline already exists.
	_, stderr, code = runCheck(t, "--root", dir, "--init")
	if code != 2 || !strings.Contains(stderr, `"error"`) {
		t.Fatalf("expected exit 2 when baseline exists, got %d: %s", code, stderr)
	}

	// Passing check with the baseline in place.
	r, stderr, code = runCheck(t, "--root", dir)
	if code != 0 {
		t.Fatalf("expected exit 0 with baseline, got %d: %s", code, stderr)
	}
	if r.Baselined != 3 || len(r.New) != 0 || len(r.Stale) != 0 {
		t.Fatalf("unexpected report with baseline: %+v", r)
	}

	// Remove one of the two identical blocks from baseline: identical blocks need two entries.
	basePath := filepath.Join(dir, "scripts", "comment-baseline.txt")
	rawBase, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(rawBase), "\n"), "\n")
	// Keep header + one duplicate entry + the #34 entry (drop the first duplicate entry).
	if len(lines) != 4 {
		t.Fatalf("expected 4 baseline lines, got %d: %v", len(lines), lines)
	}
	oneDupBaseline := strings.Join([]string{lines[0], lines[2], lines[3]}, "\n") + "\n"
	if err := os.WriteFile(basePath, []byte(oneDupBaseline), 0o644); err != nil {
		t.Fatal(err)
	}
	r, _, code = runCheck(t, "--root", dir)
	if code != 1 || r.Baselined != 2 || len(r.New) != 1 || r.New[0].Line != 6 {
		t.Fatalf("expected second identical block to fail as new: code=%d report=%+v", code, r)
	}

	// Restore full 3-entry baseline, then edit the #34 block: edited block is new and old entry stale.
	if err := os.WriteFile(basePath, rawBase, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a.go", strings.Join([]string{
		"package p",
		"",
		"// duplicate #12",
		"var a = 1",
		"",
		"// duplicate #12",
		"var b = 2",
		"",
		"// edited #99",
		"var c = 3",
		"",
	}, "\n"))
	r, _, code = runCheck(t, "--root", dir)
	if code != 1 || r.Baselined != 2 || len(r.New) != 1 || len(r.Stale) != 1 {
		t.Fatalf("expected 1 new and 1 stale, got code=%d report=%+v", code, r)
	}

	// --prune removes only the stale entry and never adds the new #99 entry.
	r, _, code = runCheck(t, "--root", dir, "--prune")
	if code != 1 || r.Baselined != 2 || len(r.New) != 1 || len(r.Stale) != 0 {
		t.Fatalf("expected --prune to clear stale but keep new failing: code=%d report=%+v", code, r)
	}
	prunedBase, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatal(err)
	}
	prunedLines := strings.Split(strings.TrimRight(string(prunedBase), "\n"), "\n")
	if len(prunedLines) != 3 {
		t.Fatalf("expected header + 2 kept entries after --prune, got %v", prunedLines)
	}
}

func TestByteCounts(t *testing.T) {
	dir := t.TempDir()
	// Line 1: "package p\n" (10 bytes)
	// Line 2: "// whole\n" (9 bytes, whole-line comment)
	// Line 3: "var x = 1 // trail\n" (19 bytes, trailing comment -> not in comment_bytes)
	// Total source_bytes = 38, comment_bytes = 9, comment_share = round(9/38, 4) = 0.2368
	content := "package p\n// whole\nvar x = 1 // trail\n"
	writeFile(t, dir, "bytes.go", content)

	r, stderr, code := runCheck(t, "--root", dir)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d: %s", code, stderr)
	}
	if r.Files != 1 || r.SourceBytes != 38 || r.CommentBytes != 9 || r.CommentShare != 0.2368 {
		t.Fatalf("unexpected byte metrics: %+v", r)
	}
}

func TestOutputCapAt20WithTruncated(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("package p\n")
	for i := 1; i <= 25; i++ {
		fmt.Fprintf(&b, "var v%d = %d // issue #%d\n", i, i, i)
	}
	writeFile(t, dir, "many.go", b.String())

	r, _, code := runCheck(t, "--root", dir)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if r.Violations != 25 || len(r.New) != 20 || !r.Truncated {
		t.Fatalf("expected 25 violations capped at 20 with truncated=true, got %+v", r)
	}
}

func TestUnparsableFileAndUsageError(t *testing.T) {
	_, stderr, code := runCheck(t, "--unknown")
	if code != 2 || !strings.Contains(stderr, `"error"`) {
		t.Fatalf("expected exit 2 on unknown flag, got %d: %s", code, stderr)
	}

	dir := t.TempDir()
	writeFile(t, dir, "broken.go", "package p\nfunc (\n")
	_, stderr, code = runCheck(t, "--root", dir)
	if code != 1 || !strings.Contains(stderr, `"error"`) {
		t.Fatalf("expected exit 1 on unparsable Go file, got %d: %s", code, stderr)
	}
}
