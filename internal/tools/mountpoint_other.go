//go:build !windows

package tools

// throughMountPoint is Windows' (mountpoint_windows.go): elsewhere a mount
// is not a link, and filepath.EvalSymlinks already follows every link.
func throughMountPoint(string) (string, bool) { return "", false }
