//go:build linux

package update

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// systemProcesses reads /proc: cmdline for the arguments, the exe link for
// the path, stat for the parent and the start time.
func systemProcesses() ([]Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	boot := bootTime()
	var procs []Process
	names := map[int]string{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		p := Process{PID: pid}
		if raw, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
			parts := bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0})
			if len(parts) > 0 && len(parts[0]) > 0 {
				p.Exe = string(parts[0])
				for _, a := range parts[1:] {
					p.Args = append(p.Args, string(a))
				}
			}
		}
		if exe, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
			p.Exe = strings.TrimSuffix(exe, " (deleted)")
		}
		if comm, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil {
			names[pid] = strings.TrimSpace(string(comm))
		}
		if stat, err := os.ReadFile(filepath.Join(dir, "stat")); err == nil {
			// The command name is in parentheses and may hold spaces; the
			// fields after it start with the state, then the parent pid.
			if i := bytes.LastIndexByte(stat, ')'); i >= 0 {
				f := strings.Fields(string(stat[i+1:]))
				if len(f) > 1 {
					p.PPID, _ = strconv.Atoi(f[1])
				}
				if len(f) > 19 && !boot.IsZero() {
					if ticks, err := strconv.ParseInt(f[19], 10, 64); err == nil {
						p.Start = boot.Add(time.Duration(ticks) * time.Second / clockTicks)
					}
				}
			}
		}
		procs = append(procs, p)
	}
	for i := range procs {
		procs[i].ParentName = names[procs[i].PPID]
	}
	return procs, nil
}

// clockTicks is the kernel's USER_HZ, 100 on every Linux Go runs on.
const clockTicks = 100

func bootTime() time.Time {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			if secs, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64); err == nil {
				return time.Unix(secs, 0)
			}
		}
	}
	return time.Time{}
}

// processLogs says which files a process has as stdout and stderr, when they
// are files: /proc/<pid>/fd/1 and 2 link to them. A terminal or a pipe is
// not a file and yields "".
func processLogs(pid int) (stdout, stderr string) {
	file := func(fd string) string {
		target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", fd))
		if err != nil || !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "/dev/") {
			return ""
		}
		return strings.TrimSuffix(target, " (deleted)")
	}
	return file("1"), file("2")
}

func detach(attr *syscall.SysProcAttr) { attr.Setsid = true }
