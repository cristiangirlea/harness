package update

import (
	"bytes"
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

// Source selects a local file, go install ref, or release (the default).
type Source struct {
	File    string
	Ref     string
	Release *ReleaseSource
}

// Options configures update hooks and optional explicit targets.
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

// Run updates or rolls back targets: 0 succeeds, 1 means target/restart failure, 2 plus error means startup failure.
func Run(ctx context.Context, out io.Writer, o Options) (int, error) {
	if err := o.defaults(); err != nil {
		return 2, err
	}
	if n := boolCount(o.Source.File != "", o.Source.Ref != "", o.Source.Release != nil); n > 1 {
		return 2, errors.New("choose one of --file, --ref (or --main) and the release")
	}

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
	// Leave unrelated programs named hrn untouched and uncounted.
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

	// Renaming changes process paths, so capture them first.
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
			// The candidate file may still run an old watch process.
			if versions[t].Same(want) && (!want.Modified || sameBytes(candidate, t)) && !isSameFile(candidate, t) {
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
				// Renaming --file's own copy would remove the source.
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
	// Fetch must verify the release checksum before execution.
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

// A modified build's version line cannot identify its edits; unreadable files must not be skipped.
func sameBytes(a, b string) bool {
	fa, err := os.Open(a)
	if err != nil {
		return false
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false
	}
	defer fb.Close()
	var ba, bb [32 * 1024]byte
	for {
		na, ea := io.ReadFull(fa, ba[:])
		nb, eb := io.ReadFull(fb, bb[:])
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false
		}
		if ea != nil || eb != nil {
			return ea == eb && (ea == io.EOF || ea == io.ErrUnexpectedEOF)
		}
	}
}

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
