//go:build !linux && !windows

package ws

func startID(pid int) (string, bool) {
	return "", false
}
