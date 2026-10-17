package update

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Process is one running program as the system lists it. Exe is its full
// path when the system gives it, else the image name alone; Args are the
// arguments after the program. Start and ParentName are set where known.
type Process struct {
	PID        int
	PPID       int
	Exe        string
	Args       []string
	Start      time.Time
	ParentName string
}

// ProcessLister lists the running processes. SystemProcesses is the real
// one; tests hand in a slice.
type ProcessLister interface {
	Processes() ([]Process, error)
}

// SystemProcesses reads the process list the way each system allows:
// Toolhelp and the PEB on Windows, /proc on Linux, ps on macOS.
type SystemProcesses struct{}

// Processes lists every process the caller may see.
func (SystemProcesses) Processes() ([]Process, error) { return systemProcesses() }

// DescribeProcess is one line for a listing: the pid, the command, and the
// parent and start time when known.
func DescribeProcess(p Process) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pid %d %s", p.PID, strings.Join(append([]string{"hrn"}, p.Args...), " "))
	var extra []string
	if p.PPID > 0 {
		s := fmt.Sprintf("parent %d", p.PPID)
		if p.ParentName != "" {
			s += " " + p.ParentName
		}
		extra = append(extra, s)
	}
	if !p.Start.IsZero() {
		extra = append(extra, "since "+p.Start.Local().Format("2006-01-02 15:04"))
	}
	if len(extra) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(extra, ", "))
	}
	return b.String()
}

// parsePS reads `ps -axo pid=,ppid=,lstart=,command=`. lstart is a fixed
// five-word date, so the command and its arguments are everything after it;
// an argument with a space in it cannot be told from two, which only matters
// for --lan=NAME. A bare program name is looked up with lookPath, through
// this process's PATH since the other one's is not visible.
func parsePS(out []byte, lookPath func(string) (string, error)) []Process {
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
		if !strings.ContainsRune(p.Exe, '/') {
			if path, err := lookPath(p.Exe); err == nil && filepath.IsAbs(path) {
				p.Exe = path
			}
		}
		if t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.Join(f[2:7], " "), time.Local); err == nil {
			p.Start = t
		}
		names[pid] = filepath.Base(p.Exe)
		procs = append(procs, p)
	}
	for i := range procs {
		procs[i].ParentName = names[procs[i].PPID]
	}
	return procs
}

// fromTargets keeps the processes whose exe is one of targets (resolved
// paths) and whose first argument is sub.
func fromTargets(procs []Process, targets []string, sub string) []Process {
	var out []Process
	resolved := make([]string, len(targets))
	for i, t := range targets {
		resolved[i] = Resolved(t)
	}
	for _, p := range procs {
		if len(p.Args) == 0 || p.Args[0] != sub || !filepath.IsAbs(p.Exe) {
			continue
		}
		exe := Resolved(p.Exe)
		for _, t := range resolved {
			if samePath(exe, t) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// Resolved is the path with symlinks and Windows 8.3 short names expanded,
// or the path itself when that fails. Two paths name one file only when
// their resolved forms agree.
func Resolved(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return path
}

// splitCommandLine takes a Windows command line apart the way
// CommandLineToArgvW does: the program ends at the first space outside
// quotes, with no escapes; in the rest, 2n backslashes before a quote are n
// backslashes and the quote toggles quoting, 2n+1 give n and a literal
// quote, and backslashes elsewhere stand for themselves.
func splitCommandLine(line string) []string {
	var args []string
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i >= len(line) {
		return nil
	}
	// The program.
	if line[i] == '"' {
		end := strings.IndexByte(line[i+1:], '"')
		if end < 0 {
			return []string{line[i+1:]}
		}
		args = append(args, line[i+1:i+1+end])
		i += end + 2
	} else {
		end := strings.IndexAny(line[i:], " \t")
		if end < 0 {
			return []string{line[i:]}
		}
		args = append(args, line[i:i+end])
		i += end
	}
	var cur strings.Builder
	inArg, inQuote := false, false
	for ; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\':
			n := 0
			for i < len(line) && line[i] == '\\' {
				n++
				i++
			}
			if i < len(line) && line[i] == '"' {
				cur.WriteString(strings.Repeat(`\`, n/2))
				if n%2 == 1 {
					cur.WriteByte('"')
				} else {
					inQuote = !inQuote
				}
			} else {
				cur.WriteString(strings.Repeat(`\`, n))
				i--
			}
			inArg = true
		case c == '"':
			inQuote = !inQuote
			inArg = true
		case (c == ' ' || c == '\t') && !inQuote:
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteByte(c)
			inArg = true
		}
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args
}

// waitGone polls the process list until pid is no longer in it, for at most
// the context's deadline.
func waitGone(ctx context.Context, lister ProcessLister, pid int) error {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		procs, err := lister.Processes()
		if err != nil {
			return err
		}
		found := false
		for _, p := range procs {
			if p.PID == pid {
				found = true
				break
			}
		}
		if !found {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("pid %d is still running", pid)
		case <-t.C:
		}
	}
}

// Stop kills pid and waits until the system no longer lists it.
func Stop(lister ProcessLister) func(ctx context.Context, pid int) error {
	return func(ctx context.Context, pid int) error {
		p, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		// A child of this process stays a zombie until it is reaped.
		p.Wait()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return waitGone(ctx, lister, pid)
	}
}
