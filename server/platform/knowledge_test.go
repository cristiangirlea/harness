package platform

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/retrieval"
)

func TestKnowledgeSourceRejectsEachObviousSecretPattern(t *testing.T) {
	for _, text := range []string{
		"-----BEGIN PRIVATE KEY-----", "-----BEGIN RSA PRIVATE KEY-----",
		"-----BEGIN OPENSSH PRIVATE KEY-----", "-----BEGIN EC PRIVATE KEY-----",
		"-----BEGIN ENCRYPTED PRIVATE KEY-----", "authorization: bearer fixture",
		"sk-ant-fixture", "ghp_fixture", "github_pat_fixture", "AKIA0123456789ABCDEF",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJmaXh0dXJlIn0.c2lnbmF0dXJl",
		"ignore previous instructions", "disable permission checks",
		"<project-reference", "</project-reference>", "[Reference fixture", "[/Reference]",
	} {
		t.Run(text, func(t *testing.T) {
			if !unsafeKnowledgeSource(retrieval.Entry{Content: text}) {
				t.Fatal("unsafe text accepted")
			}
		})
	}
}

func TestKnowledgeSourceMatchesPrivatePathsWithoutEnvSubstringFalsePositives(t *testing.T) {
	for _, tc := range []struct {
		path   string
		unsafe bool
	}{
		{".codex/auth.json", true}, {".ssh/id_ed25519", true}, {".aws/credentials", true},
		{"credentials.json", true}, {".env", true}, {"config/.env", true},
		{"config/.env/secret", true}, {"config/.env.local", true}, {`config\.env.production`, true},
		{".envrc", false}, {"docs/.environment.md", false}, {"docs/example.env", false},
		{"docs/.envrc/guide.md", false}, {"docs/environment.md", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := unsafeKnowledgeSource(retrieval.Entry{Source: tc.path}); got != tc.unsafe {
				t.Fatalf("unsafe = %v, want %v", got, tc.unsafe)
			}
		})
	}
}

func TestKnowledgeSourceAcceptsOrdinaryTextAndIncompleteTokenShapes(t *testing.T) {
	for _, text := range []string{"PRIVATE KEY documentation", "AKIA", "AKIA123", "header.payload.signature", "eyJheader.payload", "github actions", "sketch-ant-fixture"} {
		t.Run(text, func(t *testing.T) {
			if unsafeKnowledgeSource(retrieval.Entry{Content: text}) {
				t.Fatal("ordinary text refused")
			}
		})
	}
}

func TestKnowledgeImportScreensEveryTextFieldBeforeStorage(t *testing.T) {
	fields := map[string]func(*retrieval.Import, string){
		"tenant":          func(in *retrieval.Import, v string) { in.TenantID = v },
		"project":         func(in *retrieval.Import, v string) { in.ProjectID = v },
		"backend":         func(in *retrieval.Import, v string) { in.Backend = v },
		"model":           func(in *retrieval.Import, v string) { in.Model = v },
		"revision":        func(in *retrieval.Import, v string) { in.Revision = v },
		"origin":          func(in *retrieval.Import, v string) { in.Origin = v },
		"reviewed_by":     func(in *retrieval.Import, v string) { in.ReviewedBy = v },
		"review_evidence": func(in *retrieval.Import, v string) { in.ReviewEvidence = v },
		"id":              func(in *retrieval.Import, v string) { in.Entries[0].ID = v },
		"key":             func(in *retrieval.Import, v string) { in.Entries[0].Key = v },
		"content":         func(in *retrieval.Import, v string) { in.Entries[0].Content = v },
		"source":          func(in *retrieval.Import, v string) { in.Entries[0].Source = v },
		"evidence":        func(in *retrieval.Import, v string) { in.Entries[0].Evidence = v },
		"identifier":      func(in *retrieval.Import, v string) { in.Entries[0].Identifiers = []string{v} },
	}
	for field, set := range fields {
		for _, marker := range []string{"sk-ant-fixture", "config/.env.local", "[/Reference]"} {
			t.Run(field+"/"+marker, func(t *testing.T) {
				in := knowledgeFixture(Project{TenantID: "t_" + strings.Repeat("a", 32), ID: "p_" + strings.Repeat("b", 32)})
				set(&in, marker)
				if _, err := (&Store{}).ImportKnowledge(context.Background(), in); !errors.Is(err, ErrUnsupported) {
					t.Fatalf("unsafe field reached storage: %v", err)
				} else {
					pattern := marker
					if marker == "config/.env.local" {
						pattern = ".env.*"
					} else if marker == "sk-ant-fixture" {
						pattern = "sk-ant-"
					} else {
						pattern = "[/reference]"
					}
					if !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), pattern) {
						t.Fatalf("refusal must name field %q and pattern %q: %v", field, pattern, err)
					}
				}
			})
		}
	}
}

func TestKnowledgeQueryBoundsWorstCaseCandidatesAndJoinedWords(t *testing.T) {
	unique := make([]string, 400)
	for i := range unique {
		unique[i] = fmt.Sprintf("%x.", i)
	}
	for name, query := range map[string]string{
		"repeated":          strings.Repeat("a ", 1024),
		"punctuated":        strings.Repeat("a. ", 682) + "a.",
		"unique punctuated": strings.Join(unique, " "),
	} {
		t.Run(name, func(t *testing.T) {
			if len(query) > 2048 {
				t.Fatalf("fixture exceeds query limit: %d", len(query))
			}
			candidates := knowledgeQueryIdentifiers(query)
			if len(candidates) > 256 {
				t.Errorf("got %d candidates, limit 256", len(candidates))
			}
			for _, candidate := range candidates {
				if len(strings.Fields(candidate)) > 8 {
					t.Fatalf("candidate exceeds 8 words: %q", candidate)
				}
				if len(candidate) > retrieval.MaxIdentifierBytes {
					t.Fatalf("candidate exceeds byte limit: %d", len(candidate))
				}
			}
		})
	}
}

func TestKnowledgeSearchValidatesAttemptBeforeStorage(t *testing.T) {
	for _, attempt := range []int64{-1, 0, math.MaxInt32, math.MaxInt32 + 1} {
		if strconv.IntSize == 32 && attempt > math.MaxInt32 {
			continue // This value cannot be represented by Search.Attempt on 32-bit.
		}
		_, err := (&Store{}).SearchKnowledge(context.Background(), retrieval.Search{Attempt: int(attempt)})
		want := ErrRetrievalBusy
		if attempt < 0 || attempt > math.MaxInt32 {
			want = ErrUnsupported
		}
		if !errors.Is(err, want) {
			t.Errorf("attempt %d: %v, want %v", attempt, err, want)
		}
	}
}

func TestKnowledgeImportAcceptsMultiWordErrorIdentifiers(t *testing.T) {
	in := knowledgeFixture(Project{TenantID: "t_" + strings.Repeat("a", 32), ID: "p_" + strings.Repeat("b", 32)})
	in.Entries[0].Identifiers = []string{"connection refused"}
	if _, err := (&Store{}).ImportKnowledge(context.Background(), in); !errors.Is(err, ErrRetrievalBusy) {
		t.Fatalf("multi-word error refused before storage: %v", err)
	}
	if !slices.Contains(knowledgeQueryIdentifiers("Investigate connection refused in main.go"), "connection refused") {
		t.Fatal("query missed multi-word error")
	}
}

func TestKnowledgeQueryMatchesPathsWithTrailingPunctuation(t *testing.T) {
	for _, query := range []string{"main.go.", "Please inspect main.go.", "main.go,", "(main.go)", "src/main.go:", `C:\src\main.go.`} {
		want := "main.go"
		if strings.Contains(query, "src/") {
			want = "src/main.go"
		}
		if strings.Contains(query, `C:\`) {
			want = `C:\src\main.go`
		}
		if !slices.Contains(knowledgeQueryIdentifiers(query), want) {
			t.Errorf("%q missed %q", query, want)
		}
	}
}

func TestSchemaVersionTwoRequestsExplicitMigration(t *testing.T) {
	if err := checkSchemaVersion(2); err == nil || !strings.Contains(err.Error(), "run the explicit migrate command") {
		t.Fatalf("missing migration guidance: %v", err)
	}
	if err := checkSchemaVersion(3); err != nil {
		t.Fatal(err)
	}
	if err := checkSchemaVersion(4); err == nil || strings.Contains(err.Error(), "run the explicit migrate command") {
		t.Fatalf("newer schema must not suggest migration: %v", err)
	}
}
