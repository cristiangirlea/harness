package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/retrieval"
)

type injectedSearcher struct{ err error }

func (s injectedSearcher) SearchKnowledge(context.Context, retrieval.Search) (retrieval.Packet, error) {
	return retrieval.Packet{}, s.err
}

func TestRetrievalUsesTheKnowledgeFactory(t *testing.T) {
	opts := retrievalOptions()
	called := 0
	task := Task{Prompt: "task", Options: opts, RunDir: t.TempDir(), Knowledge: func() (retrieval.Searcher, error) {
		called++
		return injectedSearcher{}, errors.New("bad configuration")
	}}
	_, _, err := prepareRetrieval(context.Background(), "codex", task)
	if called != 1 || err == nil || !strings.Contains(err.Error(), "retrieval transport configuration invalid") {
		t.Fatalf("calls=%d err=%v", called, err)
	}
}

func TestRetrievalFakeSearcherOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		err        error
		cancel     bool
		want       string
	}{
		{"optional unavailable", "optional", retrieval.ErrUnavailable, false, "optional retrieval unavailable"},
		{"required unavailable", "required", retrieval.ErrUnavailable, false, "retrieval request rejected or unavailable"},
		{"optional rejected", "optional", errors.New("rejected"), false, "retrieval request rejected or unavailable"},
		{"canceled", "optional", context.Canceled, true, "retrieval canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := retrievalOptions()
			opts.Retrieval = tc.mode
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			task := Task{Prompt: "task", Options: opts, RunDir: t.TempDir(), Knowledge: func() (retrieval.Searcher, error) { return injectedSearcher{err: tc.err}, nil }}
			_, state, err := prepareRetrieval(ctx, "codex", task)
			if tc.name == "optional unavailable" {
				if err != nil || state.Failure != tc.want {
					t.Fatalf("state=%+v err=%v", state, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("state=%+v err=%v", state, err)
			}
		})
	}
}

func TestRetrievalRequiresKnowledgeForFreshSearch(t *testing.T) {
	_, _, err := prepareRetrieval(context.Background(), "codex", Task{Prompt: "task", Options: retrievalOptions(), RunDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "Knowledge") {
		t.Fatalf("err=%v", err)
	}
}

func TestRetrievalReplayDoesNotConstructKnowledge(t *testing.T) {
	opts := retrievalOptions()
	opts.Retrieval = "optional"
	previous := &retrieval.Record{Mode: "optional", Scope: retrieval.Scope{TenantID: opts.RetrievalTenant, ProjectID: opts.RetrievalProject, Revision: opts.RetrievalRevision, Backend: "codex", Model: opts.Model}, Failure: "optional retrieval unavailable"}
	task := Task{Prompt: "task", Options: opts, RunDir: t.TempDir(), RetrievalState: previous, Knowledge: func() (retrieval.Searcher, error) { t.Fatal("factory called on replay"); return nil, nil }}
	_, state, err := prepareRetrieval(context.Background(), "codex", task)
	if err != nil || !state.Replayed {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestKnowledgeFactoryIsNotSerialized(t *testing.T) {
	task := Task{Knowledge: func() (retrieval.Searcher, error) { return injectedSearcher{}, nil }}
	data, err := json.Marshal(task)
	if err != nil || strings.Contains(string(data), "Knowledge") || strings.Contains(string(data), "knowledge") {
		t.Fatalf("json=%s err=%v", data, err)
	}
}
