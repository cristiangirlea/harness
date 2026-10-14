//go:build windows

package remote

import "testing"

func makeCAFIFO(t *testing.T, path string) {
	t.Helper()
	t.Skip("Unix FIFOs are unavailable on Windows")
}
