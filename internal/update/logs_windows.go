//go:build windows

package update

// processLogs cannot see another process's standard handles here; the
// restarted watch logs under HRN_HOME instead.
func processLogs(int) (string, string) { return "", "" }
