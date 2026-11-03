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
	maps := defaultMapSettings()
	var calls atomic.Int32
	start := make(chan struct{})
	maps.build = func(ctx context.Context, _ string, _ []string) ([]workmap.Piece, error) {
		calls.Add(1)
		<-start
		return []workmap.Piece{{Title: "shared"}}, nil
	}
	s := &server{mapConfig: maps}
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

func TestSlowBuildIsNotDuplicated(t *testing.T) {
	maps := defaultMapSettings()
	maps.poll = 5 * time.Millisecond
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) {
		calls.Add(1)
		close(started)
		<-release
		return nil, nil
	}
	s := &server{mapConfig: maps}
	s.mapResult(context.Background(), false)
	<-started
	time.Sleep(20 * time.Millisecond)
	s.mapResult(context.Background(), false)
	if calls.Load() != 1 {
		t.Fatalf("builds: %d", calls.Load())
	}
	close(release)
	s.mapCache.mu.Lock()
	done := s.mapCache.done
	s.mapCache.mu.Unlock()
	if done != nil {
		<-done
	}
}

func TestMapSettingsAreIndependentPerServer(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	first := defaultMapSettings()
	first.poll = time.Millisecond
	first.build = func(ctx context.Context, _ string, _ []string) ([]workmap.Piece, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return []workmap.Piece{{Title: "first"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	second := defaultMapSettings()
	second.build = func(context.Context, string, []string) ([]workmap.Piece, error) {
		return []workmap.Piece{{Title: "second"}}, nil
	}
	a, b := &server{mapConfig: first}, &server{mapConfig: second}
	a.mapResult(context.Background(), false)
	<-started
	pieces, err := b.sharedMap(context.Background())
	if err != nil || len(pieces) != 1 || pieces[0].Title != "second" {
		t.Fatalf("second server: %v %v", pieces, err)
	}
	close(release)
	pieces, err = a.sharedMap(context.Background())
	if err != nil || len(pieces) != 1 || pieces[0].Title != "first" {
		t.Fatalf("first server: %v %v", pieces, err)
	}
}
