package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/retrieval"
)

func TestKnowledgeMarkdown(t *testing.T) {
	for _, tc := range []struct {
		name, text        string
		min, max, overlap int
		keys              []string
		wantError         string
	}{
		{"nested", "# Parent\nbody\n## Child\nchild\n", 1, 100, 0, []string{"doc.md > Parent", "doc.md > Parent > Child"}, ""},
		{"fence", "# Parent\n```go\n# not a heading\n```\n", 1, 100, 0, []string{"doc.md > Parent"}, ""},
		{"tilde fence", "# Parent\n~~~~\n# not a heading\n~~~\n## still fenced\n~~~~~\n## Child\nbody", 1, 100, 0, []string{"doc.md > Parent", "doc.md > Parent > Child"}, ""},
		{"preamble", "intro\n# Heading\nbody", 1, 100, 0, []string{"doc.md", "doc.md > Heading"}, ""},
		{"indented heading", "   # Heading\nbody\n    # code", 1, 100, 0, []string{"doc.md > Heading"}, ""},
		{"front matter", "---\ntitle: ignored\n...\n# Heading\nbody", 1, 100, 0, []string{"doc.md > Heading"}, ""},
		{"unclosed front matter", "---\n# Heading\nbody", 1, 100, 0, []string{"doc.md", "doc.md > Heading"}, ""},
		{"merge", "# Tiny\nx\n# Next\ny", 5, 100, 0, []string{"doc.md > Tiny"}, ""},
		{"merge then byte split", "# Tiny\n" + strings.Repeat("a", 10000) + "\n# Next\n" + strings.Repeat("b", 10000), 100, 800, 0, []string{"doc.md > Tiny", "doc.md > Tiny (part 2)"}, ""},
		{"split", "# Big\none two three four five six seven eight nine ten", 1, 5, 1, []string{"doc.md > Big", "doc.md > Big (part 2)", "doc.md > Big (part 3)"}, ""},
		{"duplicate", "# Same\none\n# Same\ntwo", 1, 100, 0, []string{"doc.md > Same", "doc.md > Same #2"}, ""},
		{"utf8", "\xff", 1, 100, 0, nil, "UTF-8"},
		{"marker", "# Heading\n[/Reference]", 1, 100, 0, nil, "doc.md:2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, ".git"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(root)
			if err := os.WriteFile("doc.md", []byte(tc.text), 0644); err != nil {
				t.Fatal(err)
			}
			entries, err := markdownEntries([]string{"doc.md"}, "revision", tc.min, tc.max, tc.overlap)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != len(tc.keys) {
				t.Fatalf("entries = %#v", entries)
			}
			for i, e := range entries {
				if e.Key != tc.keys[i] || e.Source != "doc.md@revision" {
					t.Fatalf("entry = %#v", e)
				}
				if len(strings.Fields(e.Content)) > tc.max {
					t.Fatalf("too many words: %q", e.Content)
				}
			}
			if tc.name == "split" && strings.Fields(entries[1].Content)[0] != "three" {
				t.Fatalf("overlap = %q", entries[1].Content)
			}
		})
	}
}

func TestKnowledgeMarkdownRequestAndBounds(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	base := retrieval.Import{Scope: retrieval.Scope{Revision: "revision", Model: "model"}, ReviewedBy: "reviewer", Origin: "reviewed_source", ReviewEvidence: "review"}
	b, _ := json.Marshal(base)
	if err := os.WriteFile("base.json", b, 0644); err != nil {
		t.Fatal(err)
	}
	text := "# " + strings.Repeat("界", 100) + "\nUse `ERR_BUILD` in src/build.go and settings.json.\n"
	if err := os.WriteFile("doc.md", []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runKnowledge([]string{"markdown", "-request", "base.json", "-min-words", "1", "doc.md"}, &out); err != nil {
		t.Fatal(err)
	}
	var got retrieval.Import
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != base.Model || got.ReviewedBy != base.ReviewedBy || got.ReviewEvidence != base.ReviewEvidence || got.Origin != base.Origin || got.Revision != base.Revision {
		t.Fatalf("base lost: %#v", got)
	}
	e := got.Entries[0]
	if len(e.Key) > 128 || len(e.Key) < 125 || !utf8.ValidString(e.Key) || e.Key[len(e.Key)-9] != '~' {
		t.Fatalf("key = %q (%d)", e.Key, len(e.Key))
	}
	hash := sha256.Sum256([]byte("doc.md > " + strings.Repeat("界", 100)))
	if !strings.HasSuffix(e.Key, fmt.Sprintf("~%x", hash[:4])) {
		t.Fatalf("hash suffix = %q", e.Key)
	}
	if strings.Join(e.Identifiers, ",") != "ERR_BUILD,src/build.go,settings.json" {
		t.Fatalf("identifiers = %#v", e.Identifiers)
	}
	if err := os.WriteFile("doc.md", []byte(strings.ReplaceAll(text, "\n", "\r\n")), 0644); err != nil {
		t.Fatal(err)
	}
	crlf, err := markdownEntries([]string{"doc.md"}, "revision", 1, 800, 40)
	if err != nil || crlf[0].Content != e.Content {
		t.Fatalf("CRLF: %#v %v", crlf, err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside"), 0644); err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	resolvedOutside, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedOutside)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{rel, resolvedOutside} {
		if _, err := markdownEntries([]string{name}, "revision", 1, 800, 40); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("outside accepted: %v", err)
		}
	}
}

func TestKnowledgeMarkdownRefusals(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	if err := os.WriteFile("doc.md", []byte("# Heading\nbody"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ min, max, overlap int }{{0, 10, 0}, {5, 4, 0}, {1, 4, -1}, {1, 4, 4}} {
		if _, err := markdownEntries([]string{"doc.md"}, "revision", tc.min, tc.max, tc.overlap); err == nil {
			t.Fatalf("bad limits accepted: %#v", tc)
		}
	}
	if _, err := markdownEntries(nil, "revision", 1, 100, 0); err == nil {
		t.Fatal("no files accepted")
	}
	if _, err := markdownEntries([]string{"doc.md"}, strings.Repeat("r", 512), 1, 100, 0); err == nil {
		t.Fatal("long source accepted")
	}
	for _, tc := range []struct{ text, want string }{
		{strings.Repeat("x", (4<<20)+1), "4 MiB"},
		{strings.Repeat("# Same\nbody\n", 101), "100 entries"},
	} {
		if err := os.WriteFile("doc.md", []byte(tc.text), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := markdownEntries([]string{"doc.md"}, "revision", 1, 100, 0); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("error = %v, want %s", err, tc.want)
		}
	}
	for _, base := range []string{`{"revision":""}`, `{"revision":"revision","entries":[{"key":"existing"}]}`} {
		if err := os.WriteFile("base.json", []byte(base), 0644); err != nil {
			t.Fatal(err)
		}
		if err := runKnowledgeMarkdown([]string{"-request", "base.json", "doc.md"}, &bytes.Buffer{}); err == nil {
			t.Fatal("bad base accepted")
		}
	}
	t.Chdir(t.TempDir())
	if _, err := markdownEntries([]string{"doc.md"}, "revision", 1, 100, 0); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("missing root: %v", err)
	}
}

func TestMarkdownChunks(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		max, overlap  int
	}{
		{"paragraphs", "one two\n\nthree four\n\nfive six\n\nseven eight", 5, 1},
		{"bytes", strings.Repeat("界 ", 5000), 10000, 3},
		{"single word bytes", strings.Repeat("界", 12000), 100, 0},
		{"many overlap words", strings.Repeat("word ", 100), 5, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks := markdownChunks(tc.content, tc.max, tc.overlap)
			if len(chunks) < 2 {
				t.Fatalf("not split: %#v", chunks)
			}
			for i, chunk := range chunks {
				if !markdownFits(chunk, tc.max) || !utf8.ValidString(chunk) {
					t.Fatalf("invalid chunk %d (%d bytes)", i, len(chunk))
				}
				if i > 0 && tc.overlap > 0 && !strings.HasPrefix(chunk, strings.Join(lastMarkdownWords(chunks[i-1], tc.overlap), " ")) {
					t.Fatalf("missing overlap in part %d", i+1)
				}
			}
		})
	}
}

func TestMarkdownIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"`ERR_BUILD` src/build.go `ERR_BUILD` settings.json", []string{"ERR_BUILD", "src/build.go", "settings.json"}},
		{"`go test ./...` and `bad @ value`", []string{"go test ./...", "./..."}},
		{"```\n`NOT_INLINE` src/code.go\n```\n`INLINE`", []string{"src/code.go", "INLINE"}},
		{"``NOT_SINGLE`` `OK`", []string{"OK"}},
		{"`" + strings.Repeat("x", 257) + "`", []string{}},
	} {
		got := markdownIdentifiers(tc.text)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Fatalf("identifiers for %q = %#v, want %#v", tc.text, got, tc.want)
		}
	}
	var text strings.Builder
	for i := range 40 {
		fmt.Fprintf(&text, "`ID_%d` ", i)
	}
	got := markdownIdentifiers(text.String())
	if len(got) != 32 || got[31] != "ID_31" {
		t.Fatalf("identifier cap = %#v", got)
	}
}
