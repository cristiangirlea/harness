//go:build unix

package batch

import (
	"os"
	"syscall"
)

func gateTempPermissionsSafe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().Perm()&0o077 == 0 && ok && st.Uid == uint32(os.Geteuid())
}

func sameVolume(a, b string) bool {
	var aStat, bStat syscall.Stat_t
	return syscall.Stat(a, &aStat) == nil && syscall.Stat(b, &bStat) == nil && aStat.Dev == bStat.Dev
}
