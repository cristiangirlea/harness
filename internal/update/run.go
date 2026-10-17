package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Source says where the candidate comes from: one of a built file, a ref to
// go install, or a release (the default when the other two are empty).
type Source struct {
	File    string
	Ref     string
	Release *ReleaseSource
}

// Options drive one run. Every function field has a real default; tests
// hand in fakes. Targets, when set, replaces the search over Running and
// Dirs.
type Options struct {
	Source   Source
	DryRun   bool
	Rollback bool

	Running   string   // the executable to start the target list from (default: os.Executable)
	Dirs      []string // folders searched for hrn copies (default: InstallDirs)
	Targets   []string // the binaries to replace, when known
	Home      string   // HRN_HOME, for the watch log
	TempDir   string   // where a download or build lands (default: a fresh temp dir, removed after)
	Version   VersionFunc
	Lister    ProcessLister
	Restarter Restarter
	Build     func(ctx context.Context, ref, dir string) (string, error)
	Now       func() time.Time
}

// Run updates (or rolls back) every target and returns the exit code: 0, or
// 1 when a target or a watch restart failed. An error is for a run that
// could not start at all.
func Run(ctx context.Context, out io.Writer, o Options) (int, error) {
	if err := o.defaults(); err != nil {
		return 2, err
	}
	if n := boolCount(o.Source.File != "", o.Source.Ref != "", o.Source.Release != nil); n > 1 {
		return 2, errors.New("choose one of --file, --ref (or --main) and the release")
	}

	// Targets and what each says.
	targets := o.Targets
	if targets == nil {
		var err error
		if targets, err = FindTargets(o.Running, o.Dirs); err != nil {
			return 2, err
		}
	}
	if len(targets) == 0 {
		return 2, errors.New("no hrn binary found to update")
	}
	// hrn holds the copies that answer version; a stranger named hrn (another
	// tool first on PATH) is listed and left alone, never counted or rolled back.
	versions := map[string]Version{}
	var hrn, strangers []string
	for _, t := range targets {
		v, err := o.Version(ctx, t)
		if err != nil {
			if errors.Is(err, ErrNotHrn) {
				strangers = append(strangers, t)
			} else {
				fmt.Fprintf(out, "%s  version failed: %v\n", t, err)
			}
			continue
		}
		versions[t] = v
		hrn = append(hrn, t)
	}
	if len(hrn) == 0 {
		return 2, errors.New("no hrn binary found to update")
	}

	// The processes, before any file is renamed: a renamed running binary
	// reports its new name afterwards.
	var watches []Watch
	var mcps []Process
	if procs, err := o.Lister.Processes(); err != nil {
		fmt.Fprintf(out, "could not list processes: %v; restart hrn watch by hand\n", err)
	} else {
		watches, mcps = WatchesOf(procs, hrn), MCPServersOf(procs, hrn)
	}

	failed := 0
	done, current := map[string]bool{}, map[string]bool{}
	if o.Rollback {
		for _, t := range hrn {
			if o.DryRun {
				backups, _ := Backups(t)
				if len(backups) == 0 {
					fmt.Fprintf(out, "would fail %s: %v\n", t, ErrNoBackup)
					failed++
					continue
				}
				fmt.Fprintf(out, "would put %s back as %s\n", filepath.Base(backups[0]), t)
				done[t] = true
				continue
			}
			v, err := Rollback(ctx, t, o.Now(), o.Version)
			if err != nil {
				fmt.Fprintf(out, "failed %s: %v\n", t, err)
				failed++
				continue
			}
			fmt.Fprintf(out, "rolled back %s to %s\n", t, v.Line)
			done[t] = true
		}
	} else {
		candidate, want, cleanup, err := o.candidate(ctx, out)
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			return 2, err
		}
		installed := versions[hrn[0]]
		fmt.Fprintf(out, "installed: %s\ncandidate: %s\n", installed.Line, want.Line)
		for _, t := range hrn {
			// The candidate's own copy is new on disk, but a watch it runs may not be.
			if versions[t].Same(want) && !isSameFile(candidate, t) {
				current[t] = true
			}
		}
		if len(current) == len(hrn) {
			fmt.Fprintln(out, "already current")
			return 0, nil
		}
		fmt.Fprintln(out, "targets:")
		for _, t := range hrn {
			fmt.Fprintf(out, "  %s  %s\n", t, versions[t].Line)
		}
		for _, t := range strangers {
			fmt.Fprintf(out, "  %s  not hrn, left alone\n", t)
		}
		for _, t := range hrn {
			old := versions[t]
			if current[t] {
				fmt.Fprintf(out, "  %s already current\n", t)
				continue
			}
			if isSameFile(candidate, t) {
				// --file named this copy: renaming it aside would remove the source.
				fmt.Fprintf(out, "  %s is the candidate itself\n", t)
				done[t] = true
				continue
			}
			if o.DryRun {
				fmt.Fprintf(out, "would rename %s to %s and copy the candidate in\n", t, BackupName(t, old, o.Now()))
				done[t] = true
				continue
			}
			backup, err := Swap(ctx, t, candidate, old, want, o.Now(), o.Version)
			if err != nil {
				fmt.Fprintf(out, "failed %s: %v\n", t, err)
				failed++
				continue
			}
			fmt.Fprintf(out, "updated %s (backup %s)\n", t, filepath.Base(backup))
			done[t] = true
		}
	}

	// The watch, from the binary it was running.
	for _, w := range watches {
		exe := Resolved(w.Exe)
		target := ""
		for _, t := range hrn {
			if samePath(exe, Resolved(t)) {
				target = t
			}
		}
		if current[target] {
			continue
		}
		if !done[target] {
			fmt.Fprintf(out, "watch (pid %d) left running on the old build: %s was not replaced\n", w.PID, target)
			continue
		}
		if o.DryRun {
			fmt.Fprintf(out, "would restart watch (pid %d): hrn %s\n", w.PID, strings.Join(w.Args, " "))
			continue
		}
		pid, log, err := o.Restarter.Restart(ctx, w, target)
		if err != nil {
			fmt.Fprintf(out, "failed to restart watch: %v\n", err)
			failed++
			continue
		}
		fmt.Fprintf(out, "watch restarted on %s (pid %d, log %s)\n", w.Addr, pid, log)
	}
	if len(mcps) > 0 {
		fmt.Fprintln(out, "still on the old build until their host restarts them:")
		for _, p := range mcps {
			fmt.Fprintf(out, "  %s\n", DescribeProcess(p))
		}
	}
	if o.DryRun {
		fmt.Fprintln(out, "dry run: nothing changed")
	}
	if failed > 0 {
		return 1, nil
	}
	return 0, nil
}

// candidate gets the new binary from the source and reads its version. The
// cleanup removes a temp dir this run made.
func (o *Options) candidate(ctx context.Context, out io.Writer) (path string, v Version, cleanup func(), err error) {
	switch {
	case o.Source.File != "":
		path = o.Source.File
		info, err := os.Stat(path)
		if err != nil {
			return "", Version{}, nil, err
		}
		if !info.Mode().IsRegular() {
			return "", Version{}, nil, fmt.Errorf("--file %s is not a regular file", path)
		}
	default:
		dir := o.TempDir
		if dir == "" {
			if dir, err = os.MkdirTemp("", "hrn-update-"); err != nil {
				return "", Version{}, nil, err
			}
			cleanup = func() { os.RemoveAll(dir) }
		} else {
			cleanup = func() {
				entries, _ := os.ReadDir(dir)
				for _, e := range entries {
					os.RemoveAll(filepath.Join(dir, e.Name()))
				}
			}
		}
		if o.Source.Ref != "" {
			fmt.Fprintf(out, "building cmd/hrn@%s\n", o.Source.Ref)
			if path, err = o.Build(ctx, o.Source.Ref, dir); err != nil {
				return "", Version{}, cleanup, err
			}
		} else {
			src := o.Source.Release
			if src == nil {
				src = &ReleaseSource{}
			}
			var tag string
			if path, tag, err = src.Fetch(ctx, dir); err != nil {
				return "", Version{}, cleanup, err
			}
			fmt.Fprintf(out, "source: release %s (%s)\n", tag, filepath.Base(path))
		}
	}
	// The candidate is run with version and nothing else, after the
	// checksum (for a release) has passed.
	if v, err = o.Version(ctx, path); err != nil {
		return "", Version{}, cleanup, fmt.Errorf("candidate: %w", err)
	}
	return path, v, cleanup, nil
}

func (o *Options) defaults() error {
	if o.Running == "" && o.Targets == nil {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		o.Running = exe
	}
	if o.Dirs == nil && o.Targets == nil {
		home, _ := os.UserHomeDir()
		o.Dirs = InstallDirs(os.Getenv, runtime.GOOS, home)
	}
	if o.Version == nil {
		o.Version = RunVersion
	}
	if o.Lister == nil {
		o.Lister = SystemProcesses{}
	}
	if o.Restarter.Stop == nil {
		o.Restarter.Stop = Stop(o.Lister)
	}
	if o.Restarter.Start == nil {
		o.Restarter.Start = StartDetached
	}
	if o.Restarter.Logs == nil {
		o.Restarter.Logs = LogsOf
	}
	if o.Restarter.Home == "" {
		o.Restarter.Home = o.Home
	}
	if o.Build == nil {
		o.Build = Builder{}.BuildRef
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return nil
}

func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// isSameFile reports whether two paths name one file, through symlinks and
// case-insensitive file systems alike.
func isSameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}
