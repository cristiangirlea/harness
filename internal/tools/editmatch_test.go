package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func editMismatch(t *testing.T, content, old string) string {
	t.Helper()
	ws, tl := setup(t)
	p := filepath.Join(ws.Root, "match.txt")
	must(t, os.WriteFile(p, []byte(content), 0o644))
	_, err := call(t, ws, tl["edit_file"], map[string]any{"path": "match.txt", "old_string": old, "new_string": "replacement"})
	if err == nil {
		t.Fatal("expected an exact-match failure")
	}
	data, readErr := os.ReadFile(p)
	must(t, readErr)
	if string(data) != content {
		t.Fatal("failed edit changed the file")
	}
	return err.Error()
}

func TestEditIndentationMismatchNamesClosestLinesAndWhitespace(t *testing.T) {
	out := editMismatch(t, "header\n    alpha\n    beta\nfooter", "  alpha\n  beta\n")
	for _, want := range []string{"closest lines are 2-3:", "2\t    alpha\n3\t    beta", "lines 2-3 match old_string except for whitespace (indentation, tabs or trailing spaces)", "first whitespace difference at line 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
}

func TestEditTabsMismatchNamesClosestLinesAndWhitespace(t *testing.T) {
	out := editMismatch(t, "header\n\talpha\t beta\nfooter", "  alpha  beta")
	if !strings.Contains(out, "closest lines are 2-2:") || !strings.Contains(out, "except for whitespace") || !strings.Contains(out, "first whitespace difference at line 2") {
		t.Fatal(out)
	}
}

func TestEditCRLFMismatchNamesFirstDifferentWord(t *testing.T) {
	out := editMismatch(t, "header\r\nalpha green\r\nbeta\r\n", "alpha blue\nbeta")
	want := `first difference at line 2: file has "alpha green", old_string has "alpha blue"`
	if !strings.Contains(out, want) || strings.Contains(out, `\r`) {
		t.Fatal(out)
	}
}

func TestEditStaleCopyNamesChangedMiddleLine(t *testing.T) {
	out := editMismatch(t, "header\nalpha\nvalue = new\nomega\nfooter", "alpha\nvalue = old\nomega")
	if !strings.Contains(out, "closest lines are 2-4:") || !strings.Contains(out, `first difference at line 3: file has "value = new", old_string has "value = old"`) {
		t.Fatal(out)
	}
}

func TestEditFirstDifferenceIncludesEarlierWhitespaceChanges(t *testing.T) {
	out := editMismatch(t, "  alpha\nvalue = new\nomega", "alpha\nvalue = old\nomega")
	if !strings.Contains(out, `first difference at line 1: file has "  alpha", old_string has "alpha"`) {
		t.Fatal(out)
	}
}

func TestEditUnrelatedLinesReportNoSimilarLines(t *testing.T) {
	if out := editMismatch(t, "xxx\nyyy", "aaa\nbbb"); out != "old_string not found in file; no similar lines found" {
		t.Fatal(out)
	}
}

func TestEditClosestExcerptBoundsLinesAndBytes(t *testing.T) {
	for _, width := range []int{10, 80, 5000} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			line := strings.Repeat("a", width)
			content := strings.Repeat(line+"\n", 300)
			if width == 5000 {
				content = strings.TrimSuffix(strings.Repeat(line+"\n", 200), "\n")
			}
			out := editMismatch(t, content, strings.Repeat(line+"b\n", 200))
			if len(out) >= 5*1024 {
				t.Fatalf("whole error has %d bytes", len(out))
			}
			_, excerpt, ok := strings.Cut(out, ":\n")
			if !ok {
				t.Fatal(out)
			}
			excerpt, _, ok = strings.Cut(excerpt, "first difference at line ")
			if !ok {
				t.Fatal("missing first difference")
			}
			lines := strings.Split(strings.TrimSuffix(excerpt, "\n"), "\n")
			if len(lines)-1 > 40 || len(excerpt) > 4096+len("(... 200 more lines)\n") || !strings.HasPrefix(lines[len(lines)-1], "(... ") {
				t.Fatalf("excerpt has %d lines and %d bytes", len(lines), len(excerpt))
			}
			if width == 10 && !strings.HasSuffix(excerpt, "(... 160 more lines)\n") {
				t.Fatal(excerpt)
			}
		})
	}
}

func TestClosestLinesClipQuotedLinesAroundDifferenceOnRuneBoundaries(t *testing.T) {
	for _, fill := range []string{"é", "\x00", "\t"} {
		t.Run(fmt.Sprintf("%q", fill), func(t *testing.T) {
			line := strings.Repeat(fill, 2500)
			out := closestLines(line+"VALUE_FILE"+line, line+"VALUE_OLD"+line)
			_, quotes, ok := strings.Cut(out, "file has ")
			if !ok {
				t.Fatal("missing quoted difference")
			}
			parts := strings.Split(quotes, ", old_string has ")
			if len(parts) != 2 {
				t.Fatal("missing old_string quote")
			}
			for i, quoted := range parts {
				decoded, err := strconv.Unquote(quoted)
				if err != nil || len(quoted) > 200 || !utf8.ValidString(decoded) || !strings.HasPrefix(decoded, "...") || !strings.HasSuffix(decoded, "...") || !strings.Contains(decoded, []string{"FILE", "OLD"}[i]) {
					t.Fatalf("invalid clipped quote of %d bytes", len(quoted))
				}
			}
			if len(out) >= 5*1024 {
				t.Fatalf("whole error has %d bytes", len(out))
			}
		})
	}
}

func TestEditFinalNewlineMismatchNamesLineEndingDifference(t *testing.T) {
	out := editMismatch(t, "x\ny", "y\n")
	if !strings.Contains(out, "match old_string except for the final newline or line endings") || !strings.Contains(out, "first line ending difference at line 2") || strings.Contains(out, "indentation") {
		t.Fatal(out)
	}
}

func TestEditMixedLineEndingsNameFirstLineEndingDifference(t *testing.T) {
	out := editMismatch(t, "x\r\ny\nz\r\n", "x\ny\nz\n")
	if !strings.Contains(out, "match old_string except for the final newline or line endings") || !strings.Contains(out, "first line ending difference at line 2") || strings.Contains(out, "indentation") {
		t.Fatal(out)
	}
}

func TestEditCRLFOldStringAgainstLFFileNamesFirstLineEndingDifference(t *testing.T) {
	out := editMismatch(t, "x\ny\n", "x\r\ny\r\n")
	if !strings.Contains(out, "match old_string except for the final newline or line endings") || !strings.Contains(out, "first line ending difference at line 1") {
		t.Fatal(out)
	}
}

func TestEditQuotesInvalidUTF8QuicklyAndPreservesDifference(t *testing.T) {
	line := strings.Repeat("\x80", 50_000)
	start := time.Now()
	fileQuote, oldQuote := editDifferenceQuotes(line+"X"+line, line+"Y"+line)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("quoting took %s", elapsed)
	}
	for i, quoted := range []string{fileQuote, oldQuote} {
		decoded, err := strconv.Unquote(quoted)
		if err != nil || len(quoted) > 200 || !strings.Contains(decoded, []string{"X", "Y"}[i]) || !strings.Contains(decoded, "\x80") {
			t.Fatalf("invalid clipped quote: %s", quoted)
		}
	}
}

func TestClosestLinesChargeBlankPairsAgainstWorkBound(t *testing.T) {
	content := strings.Repeat(" \n", 1_000_000)
	old := strings.Repeat("\t\n", 2000) + "z"
	done := make(chan string, 1)
	go func() { done <- closestLines(content, old) }()
	select {
	case out := <-done:
		if out != "old_string not found in file; the file is too large to search for the closest lines" {
			t.Fatal(out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blank-line search exceeded three seconds")
	}
}

func TestClosestLinesSearchOrdinaryFilesWithinWorkBound(t *testing.T) {
	line := strings.Repeat("a", 40)
	out := closestLines(strings.Repeat(line+"\n", 2000), strings.Repeat(line+"b\n", 60))
	if !strings.Contains(out, "closest lines are 1-60:") {
		t.Fatal(out)
	}
}

func TestClosestLinesChooseEarliestTieAndIgnoreShortEndWindows(t *testing.T) {
	out := closestLines("alpha\nx\nalpha\ny\nalpha", "alpha\nz")
	if !strings.Contains(out, "closest lines are 1-2:") {
		t.Fatal(out)
	}
}

func TestClosestLinesUseWholeShortFile(t *testing.T) {
	out := closestLines("alpha", "alpha\nbeta")
	if !strings.Contains(out, "closest lines are 1-1:") || !strings.Contains(out, `first difference at line 2: file has "", old_string has "beta"`) {
		t.Fatal(out)
	}
}

func TestClosestLinesSimilarityDoesNotOverlapPrefixAndSuffix(t *testing.T) {
	if out := closestLines("a", "aaa"); out != "old_string not found in file; no similar lines found" {
		t.Fatal(out)
	}
	if out := closestLines("abXYcd", "abZWcd"); !strings.Contains(out, "closest lines are 1-1:") {
		t.Fatal(out)
	}
}

func TestClosestLinesEmptyPairsMatchButEmptyAndTextDoNot(t *testing.T) {
	if out := closestLines(" \nabc", "\t\nxyz"); !strings.Contains(out, "closest lines are 1-2:") {
		t.Fatal(out)
	}
	if out := closestLines("", "abc"); !strings.Contains(out, "no similar lines found") {
		t.Fatal(out)
	}
}

func TestEditClosestSearchStopsAtWorkBound(t *testing.T) {
	start := time.Now()
	out := editMismatch(t, strings.Repeat(strings.Repeat("a", 200)+"\n", 30000), strings.Repeat(strings.Repeat("a", 199)+"b\n", 50))
	if out != "old_string not found in file; the file is too large to search for the closest lines" {
		t.Fatal(out)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("search took %s", elapsed)
	}
}

func TestEditAmbiguousMatchAndNormalEditStayUnchanged(t *testing.T) {
	ws, tl := setup(t)
	p := filepath.Join(ws.Root, "match.txt")
	must(t, os.WriteFile(p, []byte("alpha\nalpha\n"), 0o644))
	_, err := call(t, ws, tl["edit_file"], map[string]any{"path": "match.txt", "old_string": "alpha", "new_string": "beta"})
	if err == nil || err.Error() != "old_string matches 2 times; add more context or set replace_all" {
		t.Fatalf("unexpected error: %v", err)
	}
	out, err := call(t, ws, tl["edit_file"], map[string]any{"path": "match.txt", "old_string": "alpha\nalpha", "new_string": "beta"})
	must(t, err)
	if out != "edited match.txt (1 replacement(s))" {
		t.Fatal(out)
	}
	data, err := os.ReadFile(p)
	must(t, err)
	if string(data) != "beta\n" {
		t.Fatalf("got %q", data)
	}
}
