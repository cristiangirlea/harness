package batch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/ws"
)

func TestBatchSlotLimit(t *testing.T) {
	for _, holder := range []string{"session", "other batch", "batch limited task held"} {
		t.Run(holder, func(t *testing.T) {
			dir := initRepo(t)
			path := filepath.Join(dir, ".hrn", "ws.json")
			writeFile(t, path, `{"max_slots":2}`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			for n := 1; n <= 2; n++ {
				pid := os.Getpid()
				if holder == "session" && n == 1 {
					pid = 0
				}
				if _, err := ws.Take(ctx, ws.TakeOptions{Repo: dir, Max: 2, Holder: holder, PID: pid, NoFetch: true}); err != nil {
					t.Fatal(err)
				}
			}
			var waiting string
			_, err := takeSlot(ctx, &Spec{Parallel: 10}, "limited", &keptSlots{}, "next", "hrn/next", dir, "HEAD", func(e Event) {
				if e.Kind == "wait" {
					waiting = e.Text
					cancel()
				}
			}, func(bool) error { return nil })
			if !errors.Is(err, context.Canceled) {
				t.Errorf("take error = %v; want cancellation while waiting", err)
			}
			if !strings.Contains(waiting, "allows 2 slots") || !strings.Contains(waiting, path) {
				t.Errorf("waiting message = %q; want configured limit and path", waiting)
			}
			if exists(slotPath(dir, 3)) {
				t.Error("batch created slot 3 above the limit")
			}
		})
	}
}

func TestBatchSlotLimitInvalidFile(t *testing.T) {
	dir := initRepo(t)
	path := filepath.Join(dir, ".hrn", "ws.json")
	writeFile(t, path, `{}`)
	_, err := takeSlot(context.Background(), &Spec{Parallel: 10}, "limited", &keptSlots{}, "next", "hrn/next", dir, "HEAD", func(Event) {}, func(bool) error { return nil })
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("invalid settings: %v; want path %s", err, path)
	}
}
