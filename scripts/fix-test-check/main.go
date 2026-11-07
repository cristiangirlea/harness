// name: fix-test-check-main
// summary: Require a changed test for closing fixes that change Go code.
// effects: read
// parameters: file:path=required
// example: go test -count=1 ./scripts/fix-test-check
// hash: 17962eef79bef1f79bcb53c9b430dd2a9194f998ab7b5d0564dad39205820caf
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

var closing = regexp.MustCompile(`(?i)\b(fix(es|ed)?|close[sd]?|resolve[sd]?):?\s+(?:[\w.-]+/[\w.-]+)?#(\d+)`)

type changedFile struct {
	Filename string `json:"filename"`
	Status   string `json:"status"`
	Changes  int    `json:"changes"`
	Patch    string `json:"patch"`
}

type input struct {
	Body         string        `json:"body"`
	Labels       []string      `json:"labels"`
	Files        []changedFile `json:"files"`
	ChangedFiles int           `json:"changed_files"`
}

type report struct {
	OK     bool          `json:"ok"`
	Reason string        `json:"reason"`
	Closes []json.Number `json:"closes,omitzero"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fail := func(msg string) int {
		data, _ := json.Marshal(map[string]string{"error": msg})
		fmt.Fprintln(stderr, string(data))
		return 2
	}
	if len(args) != 1 || args[0] == "" {
		return fail("usage: fix-test-check FILE")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return fail("read input: " + err.Error())
	}
	var in input
	if err := json.Unmarshal(data, &in); err != nil {
		return fail("decode input: " + err.Error())
	}
	r := check(in)
	data, _ = json.Marshal(r)
	fmt.Fprintln(stdout, string(data))
	if !r.OK {
		return 1
	}
	return 0
}

func check(in input) report {
	matches := closing.FindAllStringSubmatch(in.Body, -1)
	for _, label := range in.Labels {
		if label == "no-repro" {
			return report{OK: true, Reason: "Maintainer no-repro exemption."}
		}
	}
	if len(in.Files) < in.ChangedFiles {
		return failure("The file list was truncated; ask a maintainer for the no-repro label.", matches)
	}
	if len(matches) == 0 {
		return report{OK: true, Reason: "No closing keyword."}
	}
	code, test := false, false
	for _, file := range in.Files {
		if strings.Contains("/"+file.Filename, "/testdata/") {
			continue
		}
		if strings.HasSuffix(file.Filename, "_test.go") {
			if file.Status == "removed" {
				continue
			}
			// An omitted patch counts only with changed lines; renames and mode changes have none.
			test = test || (file.Patch == "" && file.Changes > 0) || changedTest(file.Patch)
		} else if strings.HasSuffix(file.Filename, ".go") {
			code = true
		}
	}
	if !code {
		return report{OK: true, Reason: "No changed non-test Go code."}
	}
	if test {
		return report{OK: true, Reason: "A test was added or changed."}
	}
	return failure("No added or changed func Test or func Fuzz in a _test.go file; add or change one, or ask a maintainer for the no-repro label.", matches)
}

func failure(reason string, matches [][]string) report {
	r := report{Reason: reason, Closes: []json.Number{}}
	seen := make(map[string]bool)
	for _, match := range matches {
		n := strings.TrimLeft(match[3], "0")
		if n == "" {
			n = "0"
		}
		if !seen[n] && len(n) <= 20 {
			r.Closes = append(r.Closes, json.Number(n))
			seen[n] = true
			if len(r.Closes) == 20 {
				break
			}
		}
	}
	return r
}

func changedTest(patch string) bool {
	inTest := false
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "+func Test") || strings.HasPrefix(line, "+func Fuzz") {
			return true
		}
		if strings.HasPrefix(line, "@@ ") {
			_, context, ok := strings.Cut(line[3:], "@@")
			inTest = ok && (strings.Contains(context, "func Test") || strings.Contains(context, "func Fuzz"))
		} else if inTest && (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")) {
			return true
		}
	}
	return false
}
