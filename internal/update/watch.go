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

// Watch describes a running watch's executable, arguments and address.
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

// MCPServersOf lists target MCP processes; only their hosts should restart them.
func MCPServersOf(procs []Process, targets []string) []Process {
	return fromTargets(procs, targets, "mcp")
}

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

func watchLog(home, addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return filepath.Join(home, "watch.log")
	}
	return filepath.Join(home, "watch-"+port+".log")
}

// Restarter replaces watches using caller-supplied hooks.
type Restarter struct {
	Stop  func(ctx context.Context, pid int) error
	Start func(exe string, args []string, stdout, stderr string) (pid int, err error)
	Logs  func(pid int) (stdout, stderr string)
	Home  string
}

// Restart stops w before starting exe with its arguments, returning the pid and reused or fallback log.
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

// StartDetached launches exe independently, appending output to named files.
func StartDetached(exe string, args []string, stdout, stderr string) (int, error) {
	if err := os.MkdirAll(filepath.Dir(stdout), 0o700); err != nil {
		return 0, err
	}
	out, err := os.OpenFile(stdout, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	errf := out
	if stderr != stdout {
		if err := os.MkdirAll(filepath.Dir(stderr), 0o700); err != nil {
			return 0, err
		}
		if errf, err = os.OpenFile(stderr, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
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

// LogsOf returns output files where the platform exposes them.
func LogsOf(pid int) (stdout, stderr string) { return processLogs(pid) }
