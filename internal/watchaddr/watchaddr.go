// Package watchaddr holds the shared watch address and log naming convention.
package watchaddr

// DefaultAddr is a loopback address on a port separate from hrn sessions.
const DefaultAddr = "127.0.0.1:8089"

// LogName returns the log file name for a watch port.
func LogName(port string) string { return "watch-" + port + ".log" }
