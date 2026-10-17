// name: answers-tests
// summary: Check that TCP probes stay within the wait deadline.
// effects: read, exec
// parameters: none
// example: go test -count=1 ./scripts
// hash: 8b3c71c8839b7c2291d158893f9ae7da223128ad74ffd0e5fe77a029aa402ea1
package main

import (
	"testing"
	"time"
)

func TestProbeTimeout(t *testing.T) {
	for _, c := range []struct{ remaining, want time.Duration }{
		{5 * time.Second, 200 * time.Millisecond},
		{50 * time.Millisecond, 50 * time.Millisecond},
		{time.Millisecond, 10 * time.Millisecond},
		{0, 10 * time.Millisecond},
		{-time.Second, 10 * time.Millisecond},
	} {
		if got := probeTimeout(c.remaining); got != c.want {
			t.Errorf("probeTimeout(%v) = %v, want %v", c.remaining, got, c.want)
		}
	}
}
