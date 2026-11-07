// name: fix-test-check-tests
// summary: Check closing fixes, test patches, exemptions and input errors.
// effects: read, write-workspace, exec
// parameters: none
// example: go test -count=1 ./scripts/fix-test-check
// hash: b683226a2658db6167aad78bde74b6664817f0af6b25964ae2e14e75a1724cdc
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestRules(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		labels     []string
		files      []changedFile
		ok         bool
	}{
		{"markdown", "Fixes #12", nil, []changedFile{{Filename: "README.md"}}, true},
		{"no test", "Fixes #12", nil, []changedFile{{Filename: "a.go"}}, false},
		{"new test", "Fixes #12", nil, []changedFile{{Filename: "a.go"}, {Filename: "a_test.go", Patch: "@@ -0,0 +1 @@\n+func TestA(t *testing.T) {}"}}, true},
		{"new fuzz", "Fixes #12", nil, []changedFile{{Filename: "a.go"}, {Filename: "a_test.go", Patch: "+func FuzzA(f *testing.F) {}"}}, true},
		{"existing test", "Fixes #12", nil, []changedFile{{Filename: "a.go"}, {Filename: "a_test.go", Patch: "@@ -2 +2 @@ func TestA(t *testing.T) {\n-old\n+new"}}, true},
		{"existing fuzz deletion", "Fixes #12", nil, []changedFile{{Filename: "a.go"}, {Filename: "a_test.go", Patch: "@@ -2 +2 @@ func FuzzA(f *testing.F) {\n-old"}}, true},
		{"label", "Fixes #12", []string{"no-repro"}, []changedFile{{Filename: "a.go"}}, true},
		{"no keyword", "Update #12", nil, []changedFile{{Filename: "a.go"}}, true},
		{"helper", "Fixes #12", nil, []changedFile{{Filename: "a.go"}, {Filename: "a_test.go", Patch: "@@ -2 +2 @@ func helper() {\n-old\n+new"}}, false},
		{"empty hunk", "Fixes #12", nil, []changedFile{{Filename: "a.go"}, {Filename: "a_test.go", Patch: "@@ -2 +2 @@ func TestA(t *testing.T) {\n context\n@@ -8 +8 @@ func helper() {\n+new"}}, false},
		{"missing test patch", "Fixes #12", nil, []changedFile{{Filename: "a.go"}, {Filename: "a_test.go", Changes: 8}}, true},
		{"mode-only test change", "Fixes #12", nil, []changedFile{{Filename: "a.go"}, {Filename: "a_test.go", Status: "modified"}}, false},
		{"missing non-test patch", "Fixes #12", nil, []changedFile{{Filename: "a.go"}, {Filename: "a.txt"}}, false},
		{"removed code", "Fixes #12", nil, []changedFile{{Filename: "a.go", Status: "removed"}}, false},
		{"renamed code", "Fixes #12", nil, []changedFile{{Filename: "a.go", Status: "renamed"}}, false},
		{"test only", "Fixes #12", nil, []changedFile{{Filename: "a_test.go", Patch: "helper"}}, true},
		{"exact label", "Fixes #12", []string{"NO-REPRO"}, []changedFile{{Filename: "a.go"}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := check(input{Body: tt.body, Labels: tt.labels, Files: tt.files})
			if got.OK != tt.ok || got.Reason == "" {
				t.Fatalf("got %+v, want ok=%v", got, tt.ok)
			}
			if !tt.ok && (!reflect.DeepEqual(got.Closes, []json.Number{"12"}) || !strings.Contains(got.Reason, "no-repro") || !strings.Contains(got.Reason, "test")) {
				t.Fatalf("failure guidance: %+v", got)
			}
		})
	}
}

func TestClosingKeywords(t *testing.T) {
	for _, word := range []string{"fix", "fixes", "fixed", "close", "closes", "closed", "resolve", "resolves", "resolved"} {
		got := check(input{Body: strings.ToUpper(word) + "\n#12; fixes #34; fixes #12", Files: []changedFile{{Filename: "a.go"}}})
		if got.OK || !reflect.DeepEqual(got.Closes, []json.Number{"12", "34"}) {
			t.Fatalf("%s: %+v", word, got)
		}
	}
	if !check(input{Body: "prefixes #12", Files: []changedFile{{Filename: "a.go"}}}).OK {
		t.Fatal("matched inside a word")
	}
}

func TestReferenceLimit(t *testing.T) {
	got := check(input{Body: strings.Repeat("fixes #12 ", 30) + "fixes #" + strings.Repeat("9", 10000), Files: []changedFile{{Filename: "a.go"}}})
	if got.OK || !reflect.DeepEqual(got.Closes, []json.Number{"12"}) {
		t.Fatal("oversized references must be omitted")
	}
	body := ""
	for n := 1; n <= 30; n++ {
		body += "fixes #" + strconv.Itoa(n) + " "
	}
	got = check(input{Body: body, Files: []changedFile{{Filename: "a.go"}}})
	if len(got.Closes) != 20 || got.Closes[19] != "20" {
		t.Fatalf("got %v; want the first 20 issue numbers", got.Closes)
	}
	got = check(input{Body: "fixes #" + strings.Repeat("9", 10000), Files: []changedFile{{Filename: "a.go"}}})
	data, err := json.Marshal(got)
	if err != nil || len(data) > 512 || !bytes.Contains(data, []byte(`"closes":[]`)) {
		t.Fatal("oversized reference must still produce a bounded failure with closes")
	}
}

func TestReviewRules(t *testing.T) {
	for _, tt := range []struct {
		name, data, reason string
		ok                 bool
	}{
		{"colon", `{"body":"Fixes: #12","files":[{"filename":"a.go"}]}`, "No added or changed func Test or func Fuzz in a _test.go file; add or change one, or ask a maintainer for the no-repro label.", false},
		{"cross repository", `{"body":"Fixes owner/repo#12","files":[{"filename":"a.go"}]}`, "", false},
		{"colon cross repository", `{"body":"Fixes: some-owner/repo.name#12","files":[{"filename":"a.go"}]}`, "", false},
		{"removed test", `{"body":"Fixes #12","files":[{"filename":"a.go"},{"filename":"a_test.go","status":"removed","changes":8}]}`, "", false},
		{"removed test patch", `{"body":"Fixes #12","files":[{"filename":"a.go"},{"filename":"a_test.go","status":"removed","changes":8,"patch":"@@ -2 +0,0 @@ func TestA(t *testing.T) {\n-old"}]}`, "", false},
		{"pure test rename", `{"body":"Fixes #12","files":[{"filename":"a.go"},{"filename":"a_test.go","status":"renamed","changes":0}]}`, "", false},
		{"edited test rename", `{"body":"Fixes #12","files":[{"filename":"a.go"},{"filename":"a_test.go","status":"renamed","changes":8}]}`, "", true},
		{"truncated", `{"body":"Fixes #12","changed_files":2,"files":[{"filename":"README.md"}]}`, "truncated", false},
		{"truncated with test", `{"body":"Fixes #12","changed_files":3,"files":[{"filename":"a.go"},{"filename":"a_test.go"}]}`, "truncated", false},
		{"truncated no keyword", `{"body":"Update","changed_files":2,"files":[]}`, "truncated", false},
		{"truncated exemption", `{"body":"Fixes #12","labels":["no-repro"],"changed_files":2,"files":[]}`, "", true},
		{"complete files", `{"body":"Fixes #12","changed_files":1,"files":[{"filename":"README.md"}]}`, "", true},
		{"root testdata", `{"body":"Fixes #12","files":[{"filename":"testdata/a.go"}]}`, "", true},
		{"nested testdata", `{"body":"Fixes #12","files":[{"filename":"internal/testdata/nested/a.go"}]}`, "", true},
		{"testdata lookalike", `{"body":"Fixes #12","files":[{"filename":"mytestdata/a.go"}]}`, "", false},
		{"testdata fixture is not test evidence", `{"body":"Fixes #12","files":[{"filename":"a.go"},{"filename":"testdata/a_test.go"}]}`, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(path, []byte(tt.data), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			code := run([]string{path}, &out, &stderr)
			var got report
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("%v: %s %s", err, &out, &stderr)
			}
			if got.OK != tt.ok || (code == 0) != tt.ok || stderr.Len() != 0 {
				t.Fatalf("code=%d report=%+v stderr=%s", code, got, &stderr)
			}
			if tt.reason != "" && !strings.Contains(got.Reason, tt.reason) {
				t.Fatalf("reason %q must contain %q", got.Reason, tt.reason)
			}
			if !tt.ok && !strings.Contains(got.Reason, "no-repro") {
				t.Fatal("missing maintainer guidance")
			}
		})
	}
}

func TestRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.json")
	for _, tt := range []struct {
		name, data string
		args       []string
		code       int
	}{
		{"pass", `{"body":"Update"}`, []string{path}, 0},
		{"fail", `{"body":"Fixes #12","files":[{"filename":"a.go"}]}`, []string{path}, 1},
		{"invalid", `{`, []string{path}, 2},
		{"trailing", `{} {}`, []string{path}, 2},
		{"missing", `{}`, []string{path + ".missing"}, 2},
		{"usage", `{}`, nil, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tt.data), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			if code := run(tt.args, &out, &stderr); code != tt.code {
				t.Fatalf("code=%d: %s %s", code, &out, &stderr)
			}
			data := out.Bytes()
			if tt.code == 2 {
				data = stderr.Bytes()
				if out.Len() != 0 {
					t.Fatal("error on stdout")
				}
			} else if stderr.Len() != 0 {
				t.Fatal(stderr.String())
			}
			if !json.Valid(data) {
				t.Fatalf("invalid JSON: %s", data)
			}
		})
	}
}
