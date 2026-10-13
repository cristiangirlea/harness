//go:build linux

package ws

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// procStartTicks reads field 22 after the last ')' because comm (field 2)
// may itself contain spaces and closing parentheses.
func procStartTicks(stat []byte) (uint64, bool) {
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 {
		return 0, false
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	return ticks, err == nil
}

func startID(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", false
	}
	ticks, ok := procStartTicks(stat)
	if !ok {
		return "", false
	}
	return "linux:" + strconv.FormatUint(ticks, 10), true
}
