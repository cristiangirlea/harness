package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// comm may contain spaces and ')'; split at the final ')'.
func parseProcStat(line string) (pgid int, state byte, err error) {
	open, close := strings.IndexByte(line, '('), strings.LastIndexByte(line, ')')
	if open < 0 || close <= open {
		return 0, 0, fmt.Errorf("invalid proc stat command name")
	}
	fields := strings.Fields(line[close+1:])
	if len(fields) < 3 || len(fields[0]) != 1 {
		return 0, 0, fmt.Errorf("short or invalid proc stat fields")
	}
	pgid, err = strconv.Atoi(fields[2])
	if err != nil || pgid < 0 {
		return 0, 0, fmt.Errorf("invalid proc stat process group %q", fields[2])
	}
	return pgid, fields[0][0], nil
}

func countRunningGroup(root string, pgid int) (int, error) {
	return countRunningGroupWith(root, pgid, os.ReadFile)
}

func countRunningGroupWith(root string, pgid int, read func(string) ([]byte, error)) (int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, err
	}
	live := 0
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := read(filepath.Join(root, entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) ||
			errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
			continue
		}
		if err != nil {
			return 0, err
		}
		group, state, err := parseProcStat(string(data))
		if err != nil {
			return 0, fmt.Errorf("process %s: %w", entry.Name(), err)
		}
		if group == pgid && state != 'Z' {
			live++
		}
	}
	return live, nil
}
