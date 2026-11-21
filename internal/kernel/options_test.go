package kernel

import (
	"strings"
	"testing"
	"time"
)

func TestWaitLimitMustBeWithinALimitWindow(t *testing.T) {
	for d, ok := range map[time.Duration]bool{0: true, 2 * time.Hour: true, MaxLimitWindow: true, -time.Second: false, MaxLimitWindow + time.Second: false} {
		err := Options{WaitLimit: Duration(d)}.ValidateShape()
		if (err == nil) != ok || err != nil && !strings.Contains(err.Error(), "wait_limit") {
			t.Errorf("wait_limit %s: err = %v", d, err)
		}
	}
}
