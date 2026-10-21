package watch

import (
	"context"
	"os"
	"time"
)

func watchFlowFile(ctx context.Context, path string, poll time.Duration) <-chan struct{} {
	changed := make(chan struct{}, 1)
	previous, _ := os.Stat(path)
	every := max(poll/4, 50*time.Millisecond)
	go func() {
		tick := time.NewTicker(every)
		defer tick.Stop()
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
				if different {
					select {
					case changed <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	return changed
}
