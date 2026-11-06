package platform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestKnowledgeLabelledQueries(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	dir := filepath.Join("testdata", "knowledge-labelled")
	load := func(name string, target any) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, target); err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
	}
	var corpus struct {
		Entries []KnowledgeEntry `json:"entries"`
	}
	var queries []struct {
		ID     string   `json:"id"`
		Query  string   `json:"query"`
		Expect []string `json:"expect"`
	}
	type baseline struct {
		Ranking         string   `json:"ranking"`
		Hit             float64  `json:"hit"`
		MRR             float64  `json:"mrr"`
		NoneWithContext float64  `json:"none_with_context"`
		Hits            []string `json:"hits"`
		AcceptedLosses  []string `json:"accepted_losses"`
	}
	var history []baseline
	load("corpus.json", &corpus)
	load("queries.json", &queries)
	load("baseline.json", &history)
	keys := make(map[string]bool, len(corpus.Entries))
	for _, entry := range corpus.Entries {
		keys[entry.Key] = true
	}
	ids := make(map[string]bool, len(queries))
	for _, query := range queries {
		if ids[query.ID] {
			t.Fatalf("duplicate query id %q", query.ID)
		}
		ids[query.ID] = true
		for _, key := range query.Expect {
			if !keys[key] {
				t.Fatalf("query %q expects unknown corpus key %q", query.ID, key)
			}
		}
	}
	in := knowledgeFixture(p)
	in.Entries = corpus.Entries
	version, err := s.ImportKnowledge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	current := baseline{Ranking: KnowledgeRankingVersion, Hits: []string{}, AcceptedLosses: []string{}}
	hits := make(map[string]bool)
	var answerable, none, noneWithContext int
	for _, query := range queries {
		var packet KnowledgePacket
		// The search's fixed time budget can run out under a loaded test database; this test measures relevance.
		for try := 1; ; try++ {
			packet, err = s.SearchKnowledge(ctx, KnowledgeSearch{
				KnowledgeScope: in.KnowledgeScope, VersionID: version, Query: query.Query,
			})
			if err == nil || try == 3 || !errors.Is(err, context.DeadlineExceeded) {
				break
			}
			t.Logf("query=%s search timed out, try %d", query.ID, try)
		}
		if err != nil {
			t.Fatalf("query %q: %v", query.ID, err)
		}
		if packet.Ranking != current.Ranking {
			t.Fatalf("query %q: packet ranking %q, want %q", query.ID, packet.Ranking, current.Ranking)
		}
		if len(query.Expect) == 0 {
			none++
			if len(packet.Entries) > 0 {
				noneWithContext++
			}
			t.Logf("query=%s with_context=%t entries=%d", query.ID, len(packet.Entries) > 0, len(packet.Entries))
			continue
		}
		answerable++
		rank := 0
		for i, entry := range packet.Entries {
			for _, key := range query.Expect {
				if entry.Key == key {
					rank = i + 1
					break
				}
			}
			if rank > 0 {
				break
			}
		}
		if rank > 0 {
			hits[query.ID] = true
			current.Hits = append(current.Hits, query.ID)
			current.MRR += 1 / float64(rank)
		}
		t.Logf("query=%s hit=%t rank=%d entries=%d", query.ID, rank > 0, rank, len(packet.Entries))
	}
	if answerable > 0 {
		current.Hit = float64(len(current.Hits)) / float64(answerable)
		current.MRR /= float64(answerable)
	}
	if none > 0 {
		current.NoneWithContext = float64(noneWithContext) / float64(none)
	}
	t.Logf("ranking=%s hit=%.6f mrr=%.6f none_with_context=%.6f", current.Ranking, current.Hit, current.MRR, current.NoneWithContext)
	if os.Getenv("HRN_UPDATE_LABELLED") == "1" {
		if len(history) > 0 && history[len(history)-1].Ranking == current.Ranking {
			current.AcceptedLosses = history[len(history)-1].AcceptedLosses
			history[len(history)-1] = current
		} else {
			history = append(history, current)
		}
		data, err := json.MarshalIndent(history, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "baseline.json"), append(data, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if len(history) == 0 || history[len(history)-1].Ranking != KnowledgeRankingVersion {
		data, err := json.MarshalIndent(current, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("baseline must end with ranking %q; append this element:\n%s", KnowledgeRankingVersion, data)
	}
	last := history[len(history)-1]
	accepted := make(map[string]bool, len(last.AcceptedLosses))
	for _, id := range last.AcceptedLosses {
		accepted[id] = true
	}
	previousHits := make(map[string]bool)
	start := len(history) - 2
	if start < 0 {
		start = 0
	}
	for _, record := range history[start:] {
		for _, id := range record.Hits {
			if !previousHits[id] && !hits[id] && !accepted[id] {
				t.Errorf("query %q stopped hitting; deliberate losses must be listed in accepted_losses", id)
			}
			previousHits[id] = true
		}
	}
}
