package watch

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/workmap"
)

func TestMapSharesConcurrentBuilds(t *testing.T) {
	old := buildWorkMap
	t.Cleanup(func() { buildWorkMap = old })
	var calls atomic.Int32
	start := make(chan struct{})
	buildWorkMap = func(ctx context.Context, _ string, _ []string) ([]workmap.Piece, error) {
		calls.Add(1)
		<-start
		return []workmap.Piece{{Title: "shared"}}, nil
	}
	s := &server{}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.mapPieces(context.Background(), viewer{}) }()
	}
	time.Sleep(30 * time.Millisecond)
	close(start)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("builds: %d", calls.Load())
	}
}
