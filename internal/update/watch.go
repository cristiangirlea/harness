package update

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cristiangirlea/harness/internal/watch"
)

// Watch is a running `hrn watch`: which binary, its arguments as typed, and
// the address it serves.
type Watch struct {
	PID  int
	Exe  string
	Args []string
	Addr string
}

// WatchesOf picks the hrn watch processes running from one of targets.
func WatchesOf(procs []Process, targets []string) []Watch {
	var out []Watch
	for _, p := range fromTargets(procs, targets, "watch") {
		out = append(out, Watch{PID: p.PID, Exe: p.Exe, Args: p.Args, Addr: watchAddr(p.Args)})
	}
	return out
}

// MCPServersOf picks the hrn mcp processes running from one of targets.
// They are listed, never stopped: their host restarts them.
func MCPServersOf(procs []Process, targets []string) []Process {
	return fromTargets(procs, targets, "mcp")
}

// watchAddr reads --addr from a watch's arguments, else the default.
func watchAddr(args []string) string {
	for i := 1; i < len(args); i++ {
		a := strings.TrimLeft(args[i], "-")
		if a == "addr" && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "addr="); ok {
			return v
		}
	}
	return watch.DefaultAddr
}

// watchLog is the file a restarted watch writes to when the old one's logs
// cannot be found: watch-<port>.log under HRN_HOME.
func watchLog(home, addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return filepath.Join(home, "watch.log")
	}
	return filepath.Join(home, "watch-"+port+".log")
}

// Restarter stops a watch and starts the new binary in its place. The zero
// value has no functions: cmd/hrn fills in Stop(lister), StartDetached and
// processLogs; tests record calls.
type Restarter struct {
	Stop  func(ctx context.Context, pid int) error
	Start func(exe string, args []string, stdout, stderr string) (pid int, err error)
	Logs  func(pid int) (stdout, stderr string)
	Home  string
}

// Restart stops w, then starts exe with w's arguments, detached, writing
// where the old watch wrote when that is known, else to watchLog. It
// returns the new pid and the stdout log.
func (r Restarter) Restart(ctx context.Context, w Watch, exe string) (int, string, error) {
	var stdout, stderr string
	if r.Logs != nil {
		stdout, stderr = r.Logs(w.PID)
	}
	if stdout == "" {
		stdout = watchLog(r.Home, w.Addr)
	}
	if stderr == "" {
		stderr = stdout
	}
	if err := r.Stop(ctx, w.PID); err != nil {
		return 0, "", fmt.Errorf("stop watch (pid %d): %w", w.PID, err)
	}
	pid, err := r.Start(exe, w.Args, stdout, stderr)
	if err != nil {
		return 0, "", fmt.Errorf("start %s %s: %w", exe, strings.Join(w.Args, " "), err)
	}
	return pid, stdout, nil
}

// StartDetached starts exe with args so that it outlives this process, with
// stdout and stderr appended to the named files.
func StartDetached(exe string, args []string, stdout, stderr string) (int, error) {
	if err := os.MkdirAll(filepath.Dir(stdout), 0o755); err != nil {
		return 0, err
	}
	out, err := os.OpenFile(stdout, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	errf := out
	if stderr != stdout {
		if errf, err = os.OpenFile(stderr, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
			return 0, err
		}
		defer errf.Close()
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = out, errf
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	detach(cmd.SysProcAttr)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	cmd.Process.Release()
	return pid, nil
}

// LogsOf says which files a process writes its output to, where the system
// lets another process see that.
func LogsOf(pid int) (stdout, stderr string) { return processLogs(pid) }
