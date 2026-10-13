//go:build linux

package ws

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
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

func startedAfter(pid int, t time.Time) (after bool, known bool) {
	if pid <= 0 {
		return false, false
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false, false
	}
	ticks, ok := procStartTicks(stat)
	if !ok {
		return false, false
	}
	bootStat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return false, false
	}
	for _, line := range strings.Split(string(bootStat), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "btime" {
			continue
		}
		boot, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return false, false
		}
		// Linux USER_HZ is 100 ticks per second.
		start := time.Unix(boot+int64(ticks/100), int64(ticks%100)*int64(10*time.Millisecond))
		return start.After(t), true
	}
	return false, false
}
