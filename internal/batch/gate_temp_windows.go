package batch

import (
	"os"
	"path/filepath"
	"strings"
)

func gateTempPermissionsSafe(os.FileInfo) bool { return true }

func sameVolume(a, b string) bool {
	return strings.EqualFold(filepath.VolumeName(a), filepath.VolumeName(b))
}
