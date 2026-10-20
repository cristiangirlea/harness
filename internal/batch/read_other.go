//go:build !windows

package batch

import "os"

// Open readers do not block rename outside Windows.
func readRecord(path string) ([]byte, error) { return os.ReadFile(path) }
