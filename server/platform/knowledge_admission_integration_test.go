package platform

import (
	"context"
	"errors"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"testing"
)

func TestAdmissionActivation(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	in := knowledgeFixture(p)
	baseline, err := s.ImportKnowledge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ChangeKnowledge(ctx, retrieval.Change{Scope: in.Scope, Operation: "activate", VersionID: baseline})
	if err != nil {
		t.Fatal(err)
	}
	for _, verdict := range []string{"rejected", "pending", "admitted"} {
		in.Entries[0].Content = verdict
		id, err := s.ImportKnowledge(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		a := retrieval.Admission{Scope: in.Scope, VersionID: id, BaselineVersionID: baseline, Verdict: verdict}
		if err = s.RecordAdmission(ctx, a); err != nil {
			t.Fatal(err)
		}
		v, err := s.InspectKnowledge(ctx, in.Scope, id)
		if err != nil || v.Admission == nil || v.Admission.Verdict != verdict {
			t.Fatalf("inspect: %#v %v", v, err)
		}
		active, err := s.ActiveKnowledge(ctx, in.Scope)
		if err != nil {
			t.Fatal(err)
		}
		if verdict == "admitted" {
			if active.VersionID != id || active.PreviousID != baseline {
				t.Fatal(active)
			}
			if s.RecordAdmission(ctx, a) == nil {
				t.Fatal("active candidate accepted")
			}
		} else if active.VersionID != baseline {
			t.Fatal(active)
		}
	}
}

func TestConcurrentAdmissions(t *testing.T) {
	s := testStore(t)
	p, _ := testProject(t, s)
	ctx := context.Background()
	in := knowledgeFixture(p)
	baseline, err := s.ImportKnowledge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ChangeKnowledge(ctx, retrieval.Change{Scope: in.Scope, Operation: "activate", VersionID: baseline}); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 2)
	for i := range ids {
		in.Entries[0].Content = string(rune('a' + i))
		ids[i], err = s.ImportKnowledge(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
	}
	results := make(chan error, 2)
	for _, id := range ids {
		go func(id string) {
			results <- s.RecordAdmission(ctx, retrieval.Admission{Scope: in.Scope, VersionID: id, BaselineVersionID: baseline, Verdict: "admitted"})
		}(id)
	}
	successes := 0
	for range ids {
		if err := <-results; err == nil {
			successes++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successes: %d", successes)
	}
	active, err := s.ActiveKnowledge(ctx, in.Scope)
	if err != nil || (active.VersionID != ids[0] && active.VersionID != ids[1]) || active.PreviousID != baseline {
		t.Fatalf("active: %#v %v", active, err)
	}
}
