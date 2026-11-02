package remote

import (
	"context"
	"fmt"

	"github.com/cristiangirlea/harness/internal/retrieval"
)

type searcher struct{ client *Client }

// Searcher adapts a client to the retrieval contract.
func Searcher(client *Client) retrieval.Searcher { return searcher{client: client} }

func (s searcher) SearchKnowledge(ctx context.Context, in retrieval.Search) (retrieval.Packet, error) {
	packet, err := s.client.SearchKnowledge(ctx, in)
	if err != nil && RetrievalUnavailable(err) {
		return packet, fmt.Errorf("%w: %w", retrieval.ErrUnavailable, err)
	}
	return packet, err
}

// SearcherFromEnv constructs a searcher only when retrieval is needed.
func SearcherFromEnv() (retrieval.Searcher, error) {
	client, err := NewFromEnv()
	if err != nil {
		return nil, err
	}
	return Searcher(client), nil
}
