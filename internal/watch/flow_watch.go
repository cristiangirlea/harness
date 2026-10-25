package watch

import (
	"context"
	"os"
	"time"
)

// watchFlowFile checks often so a first change shows at once, but wakes at most once per poll:
// every wake costs a full rebuild of the flow.
func watchFlowFile(ctx context.Context, path string, poll time.Duration) <-chan struct{} {
	changed := make(chan struct{}, 1)
	previous, _ := os.Stat(path)
	every := max(poll/4, 50*time.Millisecond)
	go func() {
		tick := time.NewTicker(every)
		defer tick.Stop()
		var lastWake time.Time
		pending := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				current, err := os.Stat(path)
				if err != nil && !os.IsNotExist(err) {
					continue
				}
				different := (previous == nil) != (current == nil)
				if previous != nil && current != nil {
					different = previous.Size() != current.Size() || !previous.ModTime().Equal(current.ModTime())
				}
				previous = current
				pending = pending || different
				if pending && time.Since(lastWake) >= poll {
					select {
					case changed <- struct{}{}:
					default:
					}
					pending, lastWake = false, time.Now()
				}
			}
		}
	}()
	return changed
}
