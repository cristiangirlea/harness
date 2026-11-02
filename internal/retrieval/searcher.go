package retrieval

import (
	"context"
	"errors"
)

// ErrUnavailable identifies a temporary retrieval availability failure.
var ErrUnavailable = errors.New("retrieval unavailable")

// Searcher fetches a knowledge packet for a search.
type Searcher interface {
	SearchKnowledge(context.Context, Search) (Packet, error)
}
