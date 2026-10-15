//go:build darwin

package update

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// systemProcesses runs ps: the standard library has no other way to list
// processes on macOS. lstart is a fixed five-word date, so the command and
// its arguments are everything after it. An argument with a space in it
// cannot be told from two, which only matters for --lan=NAME.
func systemProcesses() ([]Process, error) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,lstart=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	var procs []Process
	names := map[int]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		p := Process{PID: pid, PPID: ppid, Exe: f[7], Args: f[8:]}
		if t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.Join(f[2:7], " "), time.Local); err == nil {
			p.Start = t
		}
		names[pid] = filepath.Base(p.Exe)
		procs = append(procs, p)
	}
	for i := range procs {
		procs[i].ParentName = names[procs[i].PPID]
	}
	return procs, nil
}

// processLogs cannot see another process's open files here.
func processLogs(int) (string, string) { return "", "" }

func detach(attr *syscall.SysProcAttr) { attr.Setsid = true }
