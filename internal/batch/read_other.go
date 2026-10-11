//go:build !windows

package batch

import "os"

// readRecord reads a record file; outside Windows an open reader never stands
// in the way of a rename (see read_windows.go).
func readRecord(path string) ([]byte, error) { return os.ReadFile(path) }
