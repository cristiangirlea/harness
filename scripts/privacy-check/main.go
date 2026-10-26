// name: privacy-check-main
// summary: Check tracked text files for personal folders, local work paths and narration of a person.
// effects: read, exec
// parameters: --root:path=.
// example: go test -count=1 ./scripts/privacy-check
// hash: cd42c8a76e989b0cb0a6f9de36b638ca6b84ec9256831ee1cb1c05d35ed9ee05
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const maxFindings = 20

var (
	// A drive letter (C:\Users, c:/users) or a mount of one (/c/Users in Git Bash, /mnt/c/Users in WSL).
	userFolderRe = regexp.MustCompile(`(?i)(?:\b[a-z]:|(?:^|[\s"'` + "`" + `(=])(?:/mnt)?/[a-z])(?:\\+|/)users(?:\\+|/)([^\\/\s"'` + "`" + `<>%$:;,()\[\]{}]+)`)
	workdirRe    = regexp.MustCompile(`(?i)_{2}workdir`)
	ownerRe      = regexp.MustCompile(`(?i)\bthe\s+owner\b`)
	// Placeholder user folders that examples and tests may use.
	placeholderUsers = map[string]bool{"dev": true, "dev2": true, "me": true, "u": true, "x": true, "user": true,
		"runneradmin": true, "public": true, "default": true}
)

type finding struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Rule string `json:"rule"`
}

type report struct {
	Files     int       `json:"files"`
	Findings  []finding `json:"findings"`
	Truncated bool      `json:"truncated"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	root := "."
	switch {
	case len(args) == 2 && args[0] == "--root" && args[1] != "":
		root = args[1]
	case len(args) != 0:
		return fail(stderr, "usage: privacy-check [--root DIR]")
	}
	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	out, err := cmd.Output()
	if err != nil {
		return fail(stderr, "git ls-files: "+err.Error())
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	slices.Sort(files)
	r, err := check(root, slices.Compact(files))
	if err != nil {
		return fail(stderr, err.Error())
	}
	data, _ := json.Marshal(r)
	fmt.Fprintln(stdout, string(data))
	if len(r.Findings) > 0 {
		return 1
	}
	return 0
}

func fail(stderr io.Writer, msg string) int {
	data, _ := json.Marshal(map[string]string{"error": msg})
	fmt.Fprintln(stderr, string(data))
	return 2
}

// check reads each listed file under root; files that are gone or binary are skipped.
func check(root string, files []string) (report, error) {
	r := report{Findings: []finding{}}
	var all []finding
	for _, rel := range files {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return r, err
		}
		if bytes.IndexByte(src, 0) >= 0 {
			continue
		}
		r.Files++
		all = append(all, scan(rel, src)...)
	}
	if len(all) > maxFindings {
		all, r.Truncated = all[:maxFindings], true
	}
	r.Findings = append(r.Findings, all...)
	return r, nil
}

func scan(rel string, src []byte) []finding {
	var out []finding
	for i, line := range strings.Split(string(src), "\n") {
		for _, m := range userFolderRe.FindAllStringSubmatch(line, -1) {
			// A name may hold dots (first.last), but one ending a sentence is not part of it.
			if !placeholderUsers[strings.ToLower(strings.TrimRight(m[1], "."))] {
				out = append(out, finding{rel, i + 1, "user-folder"})
				break
			}
		}
		if workdirRe.MatchString(line) {
			out = append(out, finding{rel, i + 1, "local-work-path"})
		}
	}
	// Over the whole text: Markdown is hard-wrapped, so the two words may sit on two lines.
	if strings.HasSuffix(rel, ".md") {
		for _, m := range ownerRe.FindAllIndex(src, -1) {
			out = append(out, finding{rel, 1 + bytes.Count(src[:m[0]], []byte("\n")), "narrates-a-person"})
		}
	}
	if strings.HasSuffix(rel, ".go") {
		out = append(out, goComments(rel, src)...)
	}
	return out
}

// goComments flags the phrase in Go comments only: strings and identifiers may use the word technically.
// Comments on consecutive lines are read as one text, so a phrase wrapped across two of them is found.
func goComments(rel string, src []byte) []finding {
	var out []finding
	fset := token.NewFileSet()
	var s scanner.Scanner
	s.Init(fset.AddFile(rel, -1, len(src)), src, func(token.Position, string) {}, scanner.ScanComments)
	var text strings.Builder
	first, last := 0, -1
	flush := func() {
		if ownerRe.MatchString(text.String()) {
			out = append(out, finding{rel, first, "narrates-a-person"})
		}
		text.Reset()
	}
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			flush()
			return out
		}
		if tok != token.COMMENT {
			continue
		}
		start := fset.Position(pos).Line
		if start != last+1 {
			flush()
			first = start
		}
		last = start + strings.Count(lit, "\n")
		text.WriteString(strings.TrimPrefix(lit, "//"))
		text.WriteString(" ")
	}
}
