//go:build linux

package ws

import "testing"

func TestProcStartTicksWithSpacesAndClosingParenthesis(t *testing.T) {
	stat := []byte("123 (a name ) here) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 12345 20")
	if ticks, ok := procStartTicks(stat); !ok || ticks != 12345 {
		t.Errorf("start ticks = %d, %t; want 12345, true", ticks, ok)
	}
}
