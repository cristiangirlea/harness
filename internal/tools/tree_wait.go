package tools

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

// waitGroupGone keeps the platform-independent polling policy testable on
// Windows. Linux supplies a live-member count; other Unix systems only probe.
func waitGroupGone(pgid int, timeout time.Duration, probe func() error, countLive func() (int, error)) error {
	deadline := time.Now().Add(timeout)
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
				return fmt.Errorf("query live members of process group %d: %w", pgid, countErr)
			}
			if live == 0 {
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
		time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
	}
}
