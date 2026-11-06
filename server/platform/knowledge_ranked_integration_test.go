package platform

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestKnowledgeRankedSearch(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	in := knowledgeFixture(p)
	in.Entries = nil
	for i := range 24 {
		in.Entries = append(in.Entries, KnowledgeEntry{
			Key: fmt.Sprintf("entry-%02d", i), Content: fmt.Sprintf("common words record %d", i),
			Source: "docs/ranked.md", Evidence: "synthetic fixture",
		})
	}
	in.Entries[0].Key = "solstice"
	in.Entries[1].Key = "quartz zircon garnet"
	in.Entries[2].Key = "aaa"
	in.Entries[2].Content += " quartz zircon garnet"
	in.Entries[3].Identifiers = []string{"ERR_COMMON"}
	in.Entries[4].Content += " faintword"
	in.Entries[5].Identifiers = []string{"error", "a"}
	version, err := s.ImportKnowledge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name  string
		query string
		keys  []string
	}{
		{name: "common words", query: "common words"},
		{name: "rare key", query: "common words solstice", keys: []string{"solstice"}},
		{name: "key weight", query: "quartz zircon garnet", keys: []string{"quartz zircon garnet", "aaa"}},
		{name: "exact bypasses floor", query: "common words ERR_COMMON", keys: []string{"entry-03"}},
		{name: "exact first", query: "solstice ERR_COMMON", keys: []string{"entry-03", "solstice"}},
		{name: "plain-word identifiers stay under the floor", query: "a common error in words"},
		{name: "rare below floor", query: "faintword"},
		{name: "unknown terms", query: "unmatchedlexeme"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			packet, err := s.SearchKnowledge(ctx, KnowledgeSearch{KnowledgeScope: in.KnowledgeScope, VersionID: version, Query: tt.query})
			if err != nil {
				t.Fatal(err)
			}
			if len(packet.Entries) != len(tt.keys) {
				t.Fatalf("entries = %#v, want keys %v", packet.Entries, tt.keys)
			}
			for i, key := range tt.keys {
				if packet.Entries[i].Key != key {
					t.Fatalf("entry %d = %q, want %q", i, packet.Entries[i].Key, key)
				}
			}
			wantReason := ""
			if len(tt.keys) == 0 {
				wantReason = "below_threshold"
			}
			if packet.EmptyReason != wantReason {
				t.Fatalf("empty reason = %q, want %q", packet.EmptyReason, wantReason)
			}
			if err := KnowledgeValidatePacket(packet, version, tt.query); err != nil {
				t.Fatal(err)
			}
		})
	}

	in.Entries = in.Entries[:19]
	smallVersion, err := s.ImportKnowledge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := s.SearchKnowledge(ctx, KnowledgeSearch{
		KnowledgeScope: in.KnowledgeScope, VersionID: smallVersion,
		Query: "please find the common words in this verbose request for project guidance",
	})
	if err != nil || len(packet.Entries) == 0 || packet.Text == "" || packet.EmptyReason != "" {
		t.Fatalf("small version: %#v %v", packet, err)
	}
}

func padKnowledgeRankedFixture(in KnowledgeImport, count int) KnowledgeImport {
	for i := len(in.Entries); i < count; i++ {
		in.Entries = append(in.Entries, KnowledgeEntry{
			Key: fmt.Sprintf("filler-%02d", i), Content: fmt.Sprintf("unrelated filler record %d", i),
			Source: "docs/filler.md", Evidence: "synthetic fixture",
		})
	}
	return in
}

func TestKnowledgeRankedSearchAtMinimumVersionSize(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	in := padKnowledgeRankedFixture(knowledgeFixture(p), 20)
	in.Entries[0].Key = "solstice"
	version, err := s.ImportKnowledge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	query := "solstice"
	packet, err := s.SearchKnowledge(ctx, KnowledgeSearch{KnowledgeScope: in.KnowledgeScope, VersionID: version, Query: query})
	if err != nil {
		t.Fatal(err)
	}
	if len(packet.Entries) != 1 || packet.Entries[0].Key != "solstice" {
		t.Fatalf("unique key at 20 entries: %#v", packet)
	}
	if err := KnowledgeValidatePacket(packet, version, query); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeRankedVerboseTasksFindRelevantReferences(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	in := padKnowledgeRankedFixture(knowledgeFixture(p), 24)
	version, err := s.ImportKnowledge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"Please investigate why our build is failing after yesterday changes and suggest the checked command to run safely", "Please locate src/build.go and explain how ERR_BUILD relates to the failing step without modifying files"} {
		packet, err := s.SearchKnowledge(ctx, KnowledgeSearch{KnowledgeScope: in.KnowledgeScope, VersionID: version, Query: query})
		if err != nil {
			t.Fatal(err)
		}
		if len(packet.Entries) == 0 || packet.Text == "" {
			t.Fatalf("verbose task missed: %q: %#v", query, packet)
		}
		if strings.Contains(query, "ERR_BUILD") && packet.Entries[0].Key != "exact" {
			t.Fatalf("exact symbol priority lost: %#v", packet.Entries)
		}
		if err := KnowledgeValidatePacket(packet, version, query); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKnowledgeRankedSearchPunctuation(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	query := "don't foo-bar src/x.go key:value O'Brien"
	in := padKnowledgeRankedFixture(knowledgeFixture(p), 24)
	in.Entries[0].Content = query
	version, err := s.ImportKnowledge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := s.SearchKnowledge(ctx, KnowledgeSearch{KnowledgeScope: in.KnowledgeScope, VersionID: version, Query: query})
	if err != nil {
		t.Fatal(err)
	}
	if len(packet.Entries) == 0 {
		t.Fatal("punctuated query missed its matching entry")
	}
	if err := KnowledgeValidatePacket(packet, version, query); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeRankedSearchMaximumCorpusAndQuery(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	in := padKnowledgeRankedFixture(knowledgeFixture(p), 100)
	for i := range in.Entries {
		in.Entries[i].Content = fmt.Sprintf("word%03d word%03d", i, i+100)
	}
	version, err := s.ImportKnowledge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	words := make([]string, 256)
	for i := range words {
		words[i] = fmt.Sprintf("word%03d", i)
	}
	query := strings.Join(words, " ") + " "
	if len(query) != 2048 {
		t.Fatalf("query bytes = %d, want 2048", len(query))
	}
	started := time.Now()
	packet, err := s.SearchKnowledge(ctx, KnowledgeSearch{KnowledgeScope: in.KnowledgeScope, VersionID: version, Query: query})
	duration := time.Since(started)
	t.Logf("100 entries, 2048-byte query: %s", duration)
	if err != nil {
		t.Fatal(err)
	}
	if duration >= 2*time.Second {
		t.Fatalf("search exceeded fixed 2s budget: %s", duration)
	}
	if len(packet.Entries) == 0 {
		t.Fatal("long query missed matching entries")
	}
	if err := KnowledgeValidatePacket(packet, version, query); err != nil {
		t.Fatal(err)
	}
}
