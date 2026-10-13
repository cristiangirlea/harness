//go:build !linux && !windows

package ws

import "time"

func startedAfter(pid int, t time.Time) (after bool, known bool) {
	return false, false
}
