package platform

import (
	"context"
	"fmt"
	"testing"
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
