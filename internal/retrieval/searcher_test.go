package retrieval

import (
	"context"
	"errors"
	"testing"
)

type fixtureSearcher struct{}

func (fixtureSearcher) SearchKnowledge(context.Context, Search) (Packet, error) {
	return Packet{}, ErrUnavailable
}

func TestSearcherExposesUnavailable(t *testing.T) {
	var searcher Searcher = fixtureSearcher{}
	_, err := searcher.SearchKnowledge(context.Background(), Search{})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected unavailable, got %v", err)
	}
}
