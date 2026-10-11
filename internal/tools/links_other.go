//go:build !windows

package tools

// throughLink is Windows' (links_windows.go): elsewhere a mount is not a
// link, and filepath.EvalSymlinks follows the links it can.
func throughLink(string) (string, bool) { return "", false }
