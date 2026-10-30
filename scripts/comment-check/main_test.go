// name: comment-check-tests
// summary: Check comment rules, baseline comparison and pruning in fixtures.
// effects: read, write-workspace, exec
// parameters: none
// example: go test -count=1 ./scripts/comment-check
// hash: 9c602bb57a904a80d7794e202ee597403ecee93b1a28d58457425947dd114261
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func TestGitFilesExcludeIgnoredFilesAndIncludeTrackedAndUntrackedFiles(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init")
	writeFile(t, dir, "tracked.go", "package p\n")
	git("add", "tracked.go")
	writeFile(t, dir, ".gitignore", "ignored.go\nignored/\ntracked.go\n")
	before, stderr, code := runCheck(t, "--root", dir)
	if code != 0 || before.Files != 1 {
		t.Fatalf("expected tracked file despite ignore rule: code=%d stderr=%s report=%+v", code, stderr, before)
	}
	bad := "package p\n// issue #12\n"
	writeFile(t, dir, "ignored.go", bad)
	writeFile(t, dir, "ignored/nested.go", bad)
	for _, rel := range []string{"main_test.go", ".hidden/x.go", "testdata/x.go", "vendor/x.go", "dist/x.go", "node_modules/x.go"} {
		writeFile(t, dir, rel, bad)
	}
	after, stderr, code := runCheck(t, "--root", dir)
	if code != 0 || !reflect.DeepEqual(before, after) {
		t.Fatalf("ignored files changed report: code=%d stderr=%s before=%+v after=%+v", code, stderr, before, after)
	}
	writeFile(t, dir, "untracked space.go", bad)
	after, stderr, code = runCheck(t, "--root", dir)
	if code != 1 || after.Files != 2 || len(after.New) != 1 || after.New[0].File != "untracked space.go" || after.CommentShare == before.CommentShare {
		t.Fatalf("expected untracked file to change report: code=%d stderr=%s report=%+v", code, stderr, after)
	}
	if err := os.Remove(filepath.Join(dir, "tracked.go")); err != nil {
		t.Fatal(err)
	}
	after, stderr, code = runCheck(t, "--root", dir)
	if code != 1 || after.Files != 1 || len(after.New) != 1 || after.New[0].File != "untracked space.go" {
		t.Fatalf("expected deleted tracked file to be skipped: code=%d stderr=%s report=%+v", code, stderr, after)
	}
}

func TestNonRepositoryRootsStillWalkIgnoredFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".gitignore", "sub/\n")
	writeFile(t, dir, "sub/scratch.go", "package p\n// issue #12\n")
	r, stderr, code := runCheck(t, "--root", dir)
	if code != 1 || r.Files != 1 || len(r.New) != 1 || r.New[0].File != "sub/scratch.go" {
		t.Fatalf("expected non-repository walk: code=%d stderr=%s report=%+v", code, stderr, r)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return out
}

func TestGitFilesCountAnUnmergedPathOnce(t *testing.T) {
	dir := t.TempDir()
	gitOutput(t, dir, "init")
	gitOutput(t, dir, "config", "user.name", "Test")
	gitOutput(t, dir, "config", "user.email", "test@example.com")
	gitOutput(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, dir, "conflict.go", "package p\nvar n = 0\n")
	gitOutput(t, dir, "add", "conflict.go")
	gitOutput(t, dir, "commit", "-m", "base")
	gitOutput(t, dir, "checkout", "-b", "side")
	writeFile(t, dir, "conflict.go", "package p\nvar n = 1\n")
	gitOutput(t, dir, "commit", "-am", "side")
	gitOutput(t, dir, "checkout", "-b", "other", "HEAD~1")
	writeFile(t, dir, "conflict.go", "package p\nvar n = 2\n")
	gitOutput(t, dir, "commit", "-am", "other")
	out, err := exec.Command("git", "-C", dir, "merge", "--no-edit", "side").CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected merge conflict: %v: %s", err, out)
	}
	if out := gitOutput(t, dir, "ls-files", "--unmerged"); bytes.Count(out, []byte{'\n'}) != 3 {
		t.Fatalf("expected three unmerged index entries: %s", out)
	}
	src := "package p\n// issue #12\nvar n = 3\n"
	writeFile(t, dir, "conflict.go", src)
	r, stderr, code := runCheck(t, "--root", dir)
	if code != 1 || r.Files != 1 || r.SourceBytes != int64(len(src)) || r.CommentBytes != int64(len("// issue #12\n")) || r.Blocks != 1 || r.Violations != 1 || len(r.New) != 1 || r.New[0].File != "conflict.go" {
		t.Fatalf("expected unmerged path counted once: code=%d stderr=%s report=%+v", code, stderr, r)
	}
}

func TestGitSubdirectoryRootsUseRelativePathsAndExcludeIgnoredFiles(t *testing.T) {
	dir := t.TempDir()
	gitOutput(t, dir, "init")
	bad := "package p\n// issue #12\n"
	writeFile(t, dir, "sub/tracked.go", bad)
	gitOutput(t, dir, "add", "sub/tracked.go")
	writeFile(t, dir, ".gitignore", "sub/ignored.go\n")
	writeFile(t, dir, "sub/ignored.go", bad)
	writeFile(t, dir, "outside.go", bad)
	r, stderr, code := runCheck(t, "--root", filepath.Join(dir, "sub"))
	if code != 1 || r.Files != 1 || len(r.New) != 1 || r.New[0].File != "tracked.go" {
		t.Fatalf("expected subdirectory-relative tracked path only: code=%d stderr=%s report=%+v", code, stderr, r)
	}
}

func TestGitFilesIncludeUntrackedNonASCIINames(t *testing.T) {
	dir := t.TempDir()
	gitOutput(t, dir, "init")
	gitOutput(t, dir, "config", "core.quotePath", "true")
	name := "\u00fc.go"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("package p\n// issue #12\n"), 0o644); err != nil {
		t.Skipf("filesystem cannot create non-ASCII filename: %v", err)
	}
	out, err := exec.Command("git", "-C", dir, "ls-files", "--others", "--exclude-standard", "-z").CombinedOutput()
	if err != nil {
		t.Skipf("git cannot list non-ASCII filename %q: %v: %s", name, err, out)
	}
	if !bytes.Contains(out, []byte(name+"\x00")) {
		t.Skipf("git cannot represent non-ASCII filename %q: %q", name, out)
	}
	r, stderr, code := runCheck(t, "--root", dir)
	if code != 1 || r.Files != 1 || len(r.New) != 1 || r.New[0].File != name {
		t.Fatalf("expected untracked non-ASCII path: code=%d stderr=%s report=%+v", code, stderr, r)
	}
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

func TestCitationPrecision(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"after review", true}, {"per review", true}, {"from review", true},
		{"review feedback", true}, {"PR review", true},
		{"in a review task", false}, {"in a review task, after review we changed", true}, {"during a code review", false},
		{"files reviewed by", false}, {"post review comments", false},
		{"#000000", false}, {"#000000 border", false}, {"#000000 border, see #12", true}, {"#fff", false}, {"#a1b2c3", false}, {"#123", true},
	} {
		t.Run(tc.text, func(t *testing.T) {
			if got := hasCitation(tc.text); got != tc.want {
				t.Fatalf("hasCitation(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestBlockCommentBeforeCodeIsNotWholeLine(t *testing.T) {
	src := "package p\n/* #123 */ var x = 1\n"
	bytes, blocks, violations, err := scanFile("x.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if bytes != 0 || blocks != 1 || len(violations) != 1 || violations[0].reasons != "cite" {
		t.Fatalf("unexpected result: bytes=%d blocks=%d violations=%+v", bytes, blocks, violations)
	}
}

func TestBlockCommentBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		src        string
		wantBytes  int64
		wantReason string
	}{
		{"long before code", "package p\n/* one\ntwo\nthree\nfour\nfive\n*/ var x = 1\n", 40, "long"},
		{"opener star", "package p\n/*/ #12 */\n", 11, "cite"},
		{"tab before comment", "package p\n\t/* #12 */ var x = 1\n", 0, "cite"},
		{"end of file", "package p\n/* #12 */", 9, "cite"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bytes, blocks, violations, err := scanFile("x.go", []byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			if bytes != tc.wantBytes || blocks != 1 || len(violations) != 1 || violations[0].reasons != tc.wantReason {
				t.Fatalf("bytes=%d blocks=%d violations=%+v", bytes, blocks, violations)
			}
		})
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
