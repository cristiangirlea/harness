// name: comment-check-main
// summary: Check Go comments against the repository rule and baseline.
// effects: read, write-workspace, exec
// parameters: --root:path=.; --baseline:path=scripts/comment-baseline.txt; --prune:bool=false; --init:bool=false
// example: go test -count=1 ./scripts/comment-check
// hash: 05f1c1c251249354d678bf8d1047285b4db29cb900608360a8db6c661333a05f
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const (
	maxBlockLines  = 5
	maxReportItems = 20
	baselineHeader = "# Existing comment-rule violations: only --prune removes lines; a renamed file may have its path edited.\n"
)

var (
	citeIssueRe     = regexp.MustCompile(`#[0-9]+\b`)
	citeDateRe      = regexp.MustCompile(`\b(?:19|20)[0-9]{2}-[0-9]{2}-[0-9]{2}\b`)
	citeReviewRe    = regexp.MustCompile(`(?i)\b(?:review\s+(?:rounds?|findings?|comments?|feedback)|(?:after|per|from)\s+(?:(?:the|a|this|that)\s+)?(?:code\s+)?review|(?:during|in)\s+(?:the|a|this|that)\s+(?:code\s+)?review|pr\s+review|(?:the\s+)?review(?:er)?\s+(?:said|asked|found|noted|flagged|suggested)|as\s+reviewed|reviewed\s+(?:in|on|by))\b`)
	productReviewRe = regexp.MustCompile(`(?i)\b(?:in\s+a\s+review\s+task|during\s+a\s+code\s+review|files\s+reviewed\s+by|post\s+review\s+comments)\b`)
	colorRe         = regexp.MustCompile(`(?i)^#[0-9a-f]{6}$`)
)

type newEntry struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Lines   int    `json:"lines"`
	Reasons string `json:"reasons"`
}

type staleEntry struct {
	File        string `json:"file"`
	Fingerprint string `json:"fingerprint"`
}

type report struct {
	Files        int          `json:"files"`
	SourceBytes  int64        `json:"source_bytes"`
	CommentBytes int64        `json:"comment_bytes"`
	CommentShare float64      `json:"comment_share"`
	Blocks       int          `json:"blocks"`
	Violations   int          `json:"violations"`
	Baselined    int          `json:"baselined"`
	New          []newEntry   `json:"new"`
	Stale        []staleEntry `json:"stale"`
	Truncated    bool         `json:"truncated"`
}

type violation struct {
	file        string
	line        int
	lines       int
	reasons     string
	fingerprint string
}

type baselineItem struct {
	file        string
	fingerprint string
	lines       int
	reasons     string
}

type options struct {
	root        string
	baseline    string
	baselineSet bool
	prune       bool
	init        bool
}

func parseOptions(args []string) (options, error) {
	opts := options{root: "."}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--prune":
			opts.prune = true
		case "--init":
			opts.init = true
		case "--root", "--baseline":
			flag := args[i]
			i++
			if i == len(args) || args[i] == "" {
				return opts, fmt.Errorf("missing value for %s", flag)
			}
			if flag == "--root" {
				opts.root = args[i]
			} else {
				opts.baseline = args[i]
				opts.baselineSet = true
			}
		default:
			return opts, fmt.Errorf("usage: comment-check [--root DIR] [--baseline PATH] [--prune] [--init]")
		}
	}
	if opts.prune && opts.init {
		return opts, errors.New("cannot use --prune and --init together")
	}
	if !opts.baselineSet {
		opts.baseline = filepath.Join(opts.root, "scripts", "comment-baseline.txt")
	}
	return opts, nil
}

func isSkippedDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "testdata", "vendor", "dist", "node_modules":
		return true
	}
	return false
}

func sourceFiles(root string) ([]string, error) {
	var paths []string
	inside, err := exec.Command("git", "-C", root, "rev-parse", "--is-inside-work-tree").Output()
	if err == nil && strings.TrimSpace(string(inside)) == "true" {
		out, err := exec.Command("git", "-C", root, "ls-files", "-co", "--exclude-standard", "-z", "--", "*.go").Output()
		if err != nil {
			return nil, fmt.Errorf("git ls-files: %w", err)
		}
		paths = strings.Split(string(out), "\x00")
	} else {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				if path != root && isSkippedDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	var files []string
nextFile:
	for _, path := range paths {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			continue
		}
		parts := strings.Split(path, "/")
		for _, dir := range parts[:len(parts)-1] {
			if isSkippedDir(dir) {
				continue nextFile
			}
		}
		files = append(files, path)
	}
	sort.Strings(files)
	return slices.Compact(files), nil
}

func isDirective(trimmed string) bool {
	for _, prefix := range []string{"//go:", "// +build", "//line ", "//export ", "//nolint"} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func isKeptHeader(trimmed string) bool {
	for _, prefix := range []string{"// name:", "// summary:", "// effects:", "// parameters:", "// example:", "// hash:"} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func hasCitation(s string) bool {
	for _, loc := range citeIssueRe.FindAllStringIndex(s, -1) {
		if !colorRe.MatchString(s[loc[0]:loc[1]]) {
			return true
		}
	}
	return citeDateRe.MatchString(s) || citeReviewRe.MatchString(productReviewRe.ReplaceAllString(s, ""))
}

func fingerprintLines(lines []string) string {
	trimmed := make([]string, len(lines))
	for i, l := range lines {
		trimmed[i] = strings.TrimSpace(l)
	}
	sum := sha256.Sum256([]byte(strings.Join(trimmed, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

func splitRawLines(src []byte) []string {
	var lines []string
	start := 0
	for i, b := range src {
		if b == '\n' {
			lines = append(lines, string(src[start:i+1]))
			start = i + 1
		}
	}
	if start < len(src) {
		lines = append(lines, string(src[start:]))
	}
	return lines
}

type span struct {
	startLine int
	endLine   int
	leading   bool
}

func scanFile(relPath string, src []byte) (int64, int, []violation, error) {
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, relPath, src, parser.SkipObjectResolution); err != nil {
		return 0, 0, nil, err
	}

	rawLines := splitRawLines(src)
	tf := fset.AddFile(relPath, -1, len(src))
	var scanErr error
	var s scanner.Scanner
	s.Init(tf, src, func(_ token.Position, msg string) {
		if scanErr == nil {
			scanErr = errors.New(msg)
		}
	}, scanner.ScanComments)

	seenOnLine := make([]bool, len(rawLines)+2)
	seenCode := false
	var wholeSpans []span
	var trailing []violation
	var blocks int

	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		startLine := tf.PositionFor(pos, false).Line
		endLine := startLine
		if len(lit) > 1 {
			endLine = tf.PositionFor(pos+token.Pos(len(lit)-1), false).Line
		}
		if tok == token.COMMENT {
			endOffset := tf.Offset(pos) + len(lit)
			if strings.HasPrefix(lit, "/*") {
				off := tf.Offset(pos)
				endOffset = off + 2 + strings.Index(string(src[off+2:]), "*/") + 2
				endLine = tf.PositionFor(tf.Pos(endOffset-1), false).Line
			}
			suffix := rawLines[endLine-1]
			if i := strings.Index(suffix, "\n"); i >= 0 {
				suffix = suffix[:i]
			}
			endColumn := tf.PositionFor(tf.Pos(endOffset), false).Column - 1
			if endColumn <= len(suffix) {
				suffix = suffix[endColumn:]
			}
			if !seenOnLine[startLine] && (endLine > startLine || strings.TrimSpace(suffix) == "") {
				if len(wholeSpans) > 0 && wholeSpans[len(wholeSpans)-1].endLine+1 >= startLine {
					if endLine > wholeSpans[len(wholeSpans)-1].endLine {
						wholeSpans[len(wholeSpans)-1].endLine = endLine
					}
				} else {
					wholeSpans = append(wholeSpans, span{
						startLine: startLine,
						endLine:   endLine,
						leading:   !seenCode && len(wholeSpans) == 0,
					})
				}
			} else {
				trimmed := strings.TrimSpace(lit)
				if !isDirective(trimmed) {
					blocks++
					if hasCitation(trimmed) {
						trailing = append(trailing, violation{
							file:        relPath,
							line:        startLine,
							lines:       1,
							reasons:     "cite",
							fingerprint: fingerprintLines(strings.Split(lit, "\n")),
						})
					}
				}
			}
		} else {
			seenCode = true
		}
		for l := startLine; l <= endLine && l < len(seenOnLine); l++ {
			seenOnLine[l] = true
		}
	}
	if scanErr != nil {
		return 0, 0, nil, scanErr
	}

	var commentBytes int64
	var violations []violation
	for _, sp := range wholeSpans {
		var counted []string
		firstLine := 0
		cited := false
		for l := sp.startLine; l <= sp.endLine && l <= len(rawLines); l++ {
			raw := rawLines[l-1]
			trimmed := strings.TrimSpace(raw)
			if isDirective(trimmed) || (sp.leading && isKeptHeader(trimmed)) {
				continue
			}
			if firstLine == 0 {
				firstLine = l
			}
			counted = append(counted, raw)
			commentBytes += int64(len(raw))
			if hasCitation(trimmed) {
				cited = true
			}
		}
		if len(counted) == 0 {
			continue
		}
		blocks++
		var reasons []string
		if cited {
			reasons = append(reasons, "cite")
		}
		if len(counted) > maxBlockLines {
			reasons = append(reasons, "long")
		}
		if len(reasons) > 0 {
			violations = append(violations, violation{
				file:        relPath,
				line:        firstLine,
				lines:       len(counted),
				reasons:     strings.Join(reasons, ","),
				fingerprint: fingerprintLines(counted),
			})
		}
	}

	violations = append(violations, trailing...)
	sort.Slice(violations, func(i, j int) bool {
		return violations[i].line < violations[j].line
	})
	return commentBytes, blocks, violations, nil
}

func loadBaseline(path string, required bool) ([]baselineItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !required && errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var items []baselineItem
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			return nil, fmt.Errorf("invalid baseline line: %q", line)
		}
		n, err := strconv.Atoi(parts[2])
		if err != nil {
			return nil, fmt.Errorf("invalid baseline line count: %q", line)
		}
		items = append(items, baselineItem{
			file:        parts[0],
			fingerprint: parts[1],
			lines:       n,
			reasons:     parts[3],
		})
	}
	return items, nil
}

func writeBaseline(path string, items []baselineItem) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lines := make([]string, len(items))
	for i, it := range items {
		lines[i] = fmt.Sprintf("%s\t%s\t%d\t%s", it.file, it.fingerprint, it.lines, it.reasons)
	}
	sort.Strings(lines)
	var b strings.Builder
	b.WriteString(baselineHeader)
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func writeError(w io.Writer, msg string) {
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args)
	if err != nil {
		writeError(stderr, err.Error())
		return 2
	}
	if opts.init {
		if _, err := os.Stat(opts.baseline); err == nil {
			writeError(stderr, "baseline file already exists")
			return 2
		} else if !errors.Is(err, os.ErrNotExist) {
			writeError(stderr, err.Error())
			return 1
		}
	}

	var files int
	var sourceBytes, commentBytes int64
	var blocks int
	var violations []violation

	paths, err := sourceFiles(opts.root)
	if err != nil {
		writeError(stderr, err.Error())
		return 1
	}
	for _, rel := range paths {
		src, err := os.ReadFile(filepath.Join(opts.root, filepath.FromSlash(rel)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			writeError(stderr, err.Error())
			return 1
		}
		cb, blks, vs, err := scanFile(rel, src)
		if err != nil {
			writeError(stderr, fmt.Sprintf("%s: %v", rel, err))
			return 1
		}
		files++
		sourceBytes += int64(len(src))
		commentBytes += cb
		blocks += blks
		violations = append(violations, vs...)
	}

	if opts.init {
		initItems := make([]baselineItem, len(violations))
		for i, v := range violations {
			initItems[i] = baselineItem{
				file:        v.file,
				fingerprint: v.fingerprint,
				lines:       v.lines,
				reasons:     v.reasons,
			}
		}
		if err := writeBaseline(opts.baseline, initItems); err != nil {
			writeError(stderr, err.Error())
			return 1
		}
	}

	baseItems, err := loadBaseline(opts.baseline, opts.baselineSet && !opts.prune)
	if err != nil {
		writeError(stderr, err.Error())
		return 1
	}

	used := make([]bool, len(baseItems))
	var baselined int
	newList := []newEntry{}
	for _, v := range violations {
		matched := false
		for i, b := range baseItems {
			if !used[i] && b.file == v.file && b.fingerprint == v.fingerprint {
				used[i] = true
				matched = true
				break
			}
		}
		if matched {
			baselined++
		} else {
			newList = append(newList, newEntry{
				File:    v.file,
				Line:    v.line,
				Lines:   v.lines,
				Reasons: v.reasons,
			})
		}
	}

	staleList := []staleEntry{}
	if opts.prune {
		var kept []baselineItem
		for i, b := range baseItems {
			if used[i] {
				kept = append(kept, b)
			}
		}
		if err := writeBaseline(opts.baseline, kept); err != nil {
			writeError(stderr, err.Error())
			return 1
		}
	} else {
		for i, b := range baseItems {
			if !used[i] {
				staleList = append(staleList, staleEntry{
					File:        b.file,
					Fingerprint: b.fingerprint,
				})
			}
		}
	}

	failed := len(newList) > 0 || len(staleList) > 0
	truncated := false
	if len(newList) > maxReportItems {
		newList = newList[:maxReportItems]
		truncated = true
	}
	if len(staleList) > maxReportItems {
		staleList = staleList[:maxReportItems]
		truncated = true
	}

	share := 0.0
	if sourceBytes > 0 {
		share = math.Round(float64(commentBytes)/float64(sourceBytes)*10000) / 10000
	}

	rep := report{
		Files:        files,
		SourceBytes:  sourceBytes,
		CommentBytes: commentBytes,
		CommentShare: share,
		Blocks:       blocks,
		Violations:   len(violations),
		Baselined:    baselined,
		New:          newList,
		Stale:        staleList,
		Truncated:    truncated,
	}
	if err := json.NewEncoder(stdout).Encode(rep); err != nil {
		writeError(stderr, "cannot write report")
		return 1
	}
	if failed {
		writeError(stderr, "comment check failed: bring comments to the rule, or run with --prune for stale entries")
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
