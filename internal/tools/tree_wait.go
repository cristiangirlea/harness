package tools

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

func waitGroupGone(pgid int, timeout time.Duration, probe func() error, countLive func() (int, error)) error {
	deadline := time.Now().Add(timeout)
	delay := 10 * time.Millisecond
	polls := 0
	for {
		err := probe()
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		permissionDenied := errors.Is(err, syscall.EPERM)
		if err != nil && !permissionDenied {
			return fmt.Errorf("query process group %d: %w", pgid, err)
		}
		live := 1
		exact := countLive != nil && !permissionDenied
		if exact {
			var countErr error
			live, countErr = countLive()
			if countErr != nil {
				// Procfs failure cannot prove exit; keep probing.
				exact, live = false, 1
			}
			if exact && live == 0 {
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			if permissionDenied {
				return fmt.Errorf("process group %d still alive; at least one is owned by another user (after %s)", pgid, timeout)
			}
			if exact {
				return fmt.Errorf("process group %d: %d processes still alive after %s", pgid, live, timeout)
			}
			return fmt.Errorf("process group %d: at least 1 process still alive after %s", pgid, timeout)
		}
		time.Sleep(min(delay, time.Until(deadline)))
		polls++
		if polls >= 3 {
			delay = min(2*delay, 100*time.Millisecond)
		}
	}
}
