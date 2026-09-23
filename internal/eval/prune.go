package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RemoveRecord deletes a saved eval record and returns how many bytes it held;
// with dryRun it only reports the size. The batch it ran as has its own record.
func RemoveRecord(id string, dryRun bool) (int64, error) {
	if id == "" || strings.ContainsAny(id, `/\`) {
		return 0, fmt.Errorf("invalid eval id %q", id)
	}
	path := filepath.Join(dir(), id+".json")
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !dryRun {
		if err := os.Remove(path); err != nil {
			return 0, err
		}
	}
	return info.Size(), nil
}
