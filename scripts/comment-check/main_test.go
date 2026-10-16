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

func TestABlockOfSixLinesFailsAndABlankLineSplitsBlocks(t *testing.T) {
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
		"/* block one",
		"   block two",
		"   block three",
		"   block four",
		"   block five",
		"   block six */",
		"var d = 4",
		"",
	}, "\n"))

	r, stderr, code = runCheck(t, "--root", dir)
	if code != 1 || !strings.Contains(stderr, `"error"`) {
		t.Fatalf("expected exit 1 for the two violations with no default baseline, got %d: %s", code, stderr)
	}
	if r.Violations != 2 || len(r.New) != 2 {
		t.Fatalf("expected 2 violations, got %+v", r)
	}
	if r.New[0].File != "bad.go" || r.New[0].Line != 3 || r.New[0].Lines != 6 || r.New[0].Reasons != "long" {
		t.Fatalf("unexpected first violation entry: %+v", r.New[0])
	}
	if r.New[1].File != "bad.go" || r.New[1].Line != 11 || r.New[1].Lines != 6 || r.New[1].Reasons != "long" {
		t.Fatalf("unexpected block comment violation entry: %+v", r.New[1])
	}
}

func TestDirectivesAndTheLeadingKeptHeaderAreExemptWithoutSplittingBlocks(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "script.go", strings.Join([]string{
		"// name: example-main",
		"// summary: Check #12 on 2026-10-16 after the review.",
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
		"// one",
		"// two",
		"//nolint:errcheck // reviewed in #12 on 2026-10-16",
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

	writeFile(t, dir, "after_pkg.go", strings.Join([]string{
		"package main",
		"",
		"// name: not-a-leading-header",
		"// two",
		"// three",
		"//go:noinline",
		"// four",
		"// five",
		"// six",
		"func helper() {}",
		"",
	}, "\n"))

	r, _, code = runCheck(t, "--root", dir)
	if code != 1 || len(r.New) != 1 {
		t.Fatalf("expected 1 long violation when directive is inside a 6-line block with post-package // name:, got code=%d report=%+v", code, r)
	}
	if r.New[0].File != "after_pkg.go" || r.New[0].Line != 3 || r.New[0].Lines != 6 || r.New[0].Reasons != "long" {
		t.Fatalf("unexpected violation: %+v", r.New[0])
	}
}

func TestCitationsAndReviewNarrationFailWhileProductReviewTermsAndRawStringsPass(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "clean.go", strings.Join([]string{
		"package p",
		"",
		"var s = `",
		"// not a comment #12 2026-10-16 after the review",
		"// line 2",
		"// line 3",
		"// line 4",
		"// line 5",
		"// line 6",
		"`",
		"// Color #1e1e1e is dark grey.",
		"var color = \"#1e1e1e\"",
		"// Use the review purpose for code inspection.",
		"var purpose = \"review\"",
		"// Reviewed file hashes bind retrieval evidence to the worktree.",
		"var hashes = 1",
		"",
	}, "\n"))

	r, stderr, code := runCheck(t, "--root", dir)
	if code != 0 {
		t.Fatalf("expected clean exit 0, got %d (%s): %+v", code, stderr, r.New)
	}
	if r.Blocks != 3 || r.Violations != 0 {
		t.Fatalf("expected 3 blocks and 0 violations, got %+v", r)
	}

	writeFile(t, dir, "cite.go", strings.Join([]string{
		"package p",
		"",
		"var a = 1 // see #12 for background",
		"var b = 2 // added on 2026-10-16",
		"var c = 3 // Reviewed by owner",
		"var d = 4 // Fixed after the review",
		"var e = 5 // adjusted per review finding",
		"var f = 6 // what the reviewer asked for",
		"",
	}, "\n"))

	r, _, code = runCheck(t, "--root", dir)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if len(r.New) != 6 {
		t.Fatalf("expected 6 cite violations, got %+v", r.New)
	}
	for i, wantLine := range []int{3, 4, 5, 6, 7, 8} {
		if r.New[i].File != "cite.go" || r.New[i].Line != wantLine || r.New[i].Lines != 1 || r.New[i].Reasons != "cite" {
			t.Fatalf("entry %d: got %+v", i, r.New[i])
		}
	}
}

func TestTestFilesHiddenDirectoriesAndVendorTreesAreSkipped(t *testing.T) {
	dir := t.TempDir()
	bad := "package p\n// #12 reviewed by owner 2026-10-16\n"
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

func TestBaselineInitPruneAndDuplicateFingerprintsTrackViolationsAsAMultiset(t *testing.T) {
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

	_, stderr, code = runCheck(t, "--root", dir, "--init")
	if code != 2 || !strings.Contains(stderr, `"error"`) {
		t.Fatalf("expected exit 2 when baseline exists, got %d: %s", code, stderr)
	}

	r, stderr, code = runCheck(t, "--root", dir)
	if code != 0 {
		t.Fatalf("expected exit 0 with baseline, got %d: %s", code, stderr)
	}
	if r.Baselined != 3 || len(r.New) != 0 || len(r.Stale) != 0 {
		t.Fatalf("unexpected report with baseline: %+v", r)
	}

	basePath := filepath.Join(dir, "scripts", "comment-baseline.txt")
	rawBase, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(rawBase), "\n"), "\n")
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

func TestCRLFAndLFFilesProduceTheSameViolationFingerprint(t *testing.T) {
	lfContent := "package p\n\n// first #12\n// second\nvar x = 1 // trailing #34\n"
	crlfContent := strings.ReplaceAll(lfContent, "\n", "\r\n")

	_, _, lfViolations, err := scanFile("a.go", []byte(lfContent))
	if err != nil {
		t.Fatal(err)
	}
	_, _, crlfViolations, err := scanFile("a.go", []byte(crlfContent))
	if err != nil {
		t.Fatal(err)
	}
	if len(lfViolations) != 2 || len(crlfViolations) != 2 {
		t.Fatalf("expected 2 violations each, got lf=%+v crlf=%+v", lfViolations, crlfViolations)
	}
	for i := range lfViolations {
		if lfViolations[i].fingerprint != crlfViolations[i].fingerprint {
			t.Fatalf("violation %d fingerprint mismatch: lf=%s crlf=%s", i, lfViolations[i].fingerprint, crlfViolations[i].fingerprint)
		}
	}
}

func TestWholeLineCommentsCountTowardCommentBytesAndTrailingCommentsDoNot(t *testing.T) {
	dir := t.TempDir()
	// only the 9-byte whole-line comment of the 38 source bytes counts
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

func TestReportCapsNewAndStaleListsAtTwentyEntriesAndSetsTruncated(t *testing.T) {
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

func TestInvalidFlagsMalformedBaselinesAndUnparsableFilesReturnJSONErrors(t *testing.T) {
	_, stderr, code := runCheck(t, "--unknown")
	if code != 2 || !strings.Contains(stderr, `"error"`) {
		t.Fatalf("expected exit 2 on unknown flag, got %d: %s", code, stderr)
	}

	_, stderr, code = runCheck(t, "--prune", "--init")
	if code != 2 || !strings.Contains(stderr, `"error"`) {
		t.Fatalf("expected exit 2 on --prune --init, got %d: %s", code, stderr)
	}

	dir := t.TempDir()
	writeFile(t, dir, "ok.go", "package p\n")

	_, stderr, code = runCheck(t, "--root", dir, "--baseline", filepath.Join(dir, "missing.txt"))
	if code != 1 || !strings.Contains(stderr, `"error"`) {
		t.Fatalf("expected exit 1 on missing explicit --baseline, got %d: %s", code, stderr)
	}

	badFields := filepath.Join(dir, "bad-fields.txt")
	if err := os.WriteFile(badFields, []byte("ok.go\tabcdef012345\t6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = runCheck(t, "--root", dir, "--baseline", badFields)
	if code != 1 || !strings.Contains(stderr, `invalid baseline line:`) {
		t.Fatalf("expected exit 1 on malformed baseline line, got %d: %s", code, stderr)
	}

	badCount := filepath.Join(dir, "bad-count.txt")
	if err := os.WriteFile(badCount, []byte("ok.go\tabcdef012345\tnan\tlong\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = runCheck(t, "--root", dir, "--baseline", badCount)
	if code != 1 || !strings.Contains(stderr, `invalid baseline line count`) {
		t.Fatalf("expected exit 1 on non-numeric baseline line count, got %d: %s", code, stderr)
	}

	writeFile(t, dir, "broken.go", "package p\nfunc (\n")
	_, stderr, code = runCheck(t, "--root", dir)
	if code != 1 || !strings.Contains(stderr, `"error"`) {
		t.Fatalf("expected exit 1 on unparsable Go file, got %d: %s", code, stderr)
	}
}
