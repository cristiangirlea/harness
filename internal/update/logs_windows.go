//go:build windows

package update

// Peer standard handles are unavailable here, so restarted watches use HRN_HOME logs.
func processLogs(int) (string, string) { return "", "" }
