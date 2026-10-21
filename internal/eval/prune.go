package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RemoveRecord returns the record's bytes and deletes it unless dryRun; the batch record is separate.
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
