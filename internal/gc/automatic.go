package gc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/runlog"
)

const maxLogBytes = 1 << 20

var appendAutomaticPass = appendPass

// Config holds automatic cleanup settings and explicit scan roots.
type Config struct {
	Enabled        bool     `json:"enabled"`
	Roots          []string `json:"roots"`
	Interval       string   `json:"interval"`
	FreeBelowBytes int64    `json:"free_below_bytes"`
}

// Action records a cleanup decision and its estimated size.
type Action struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Reason string `json:"reason"`
	Kind   string `json:"kind"`
}

// Pass records an automatic cleanup attempt and its outcome.
type Pass struct {
	Time      time.Time `json:"time"`
	Scheduled bool      `json:"scheduled"`
	Started   bool      `json:"started,omitempty"`
	Planned   []Action  `json:"planned,omitempty"`
	Actions   []Action  `json:"actions,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// LoadConfig reads and validates the configured cleanup roots and settings.
func LoadConfig() (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(runlog.Home(), "gc.json"))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("gc.json: %w", err)
	}
	if c.Interval == "" {
		c.Interval = "24h"
	}
	if c.FreeBelowBytes == 0 {
		c.FreeBelowBytes = 50 << 30
	}
	if d, err := time.ParseDuration(c.Interval); err != nil || d <= 0 {
		return c, fmt.Errorf("gc.json: interval must be a positive duration")
	}
	if c.FreeBelowBytes < 0 {
		return c, fmt.Errorf("gc.json: free_below_bytes must be positive")
	}
	for _, root := range c.Roots {
		if !filepath.IsAbs(root) {
			return c, fmt.Errorf("gc.json: root %q must be absolute", root)
		}
		if filepath.Dir(filepath.Clean(root)) == filepath.Clean(root) {
			return c, fmt.Errorf("gc.json: root %q is a filesystem root", root)
		}
		if info, err := os.Stat(root); err != nil {
			return c, fmt.Errorf("gc.json: root %q: %w", root, err)
		} else if !info.IsDir() {
			return c, fmt.Errorf("gc.json: root %q is not a directory", root)
		}
	}
	if c.Enabled && len(c.Roots) == 0 {
		return c, errors.New("gc.json: enabled requires roots")
	}
	return c, nil
}

func readPasses() ([]Pass, error) {
	b, err := os.ReadFile(filepath.Join(runlog.Home(), "gc.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var passes []Pass
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var p Pass
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return nil, fmt.Errorf("gc.log: %w", err)
		}
		passes = append(passes, p)
	}
	return passes, nil
}

// LastPass returns the most recent logged automatic cleanup attempt.
func LastPass() (Pass, error) {
	passes, err := readPasses()
	if err != nil || len(passes) == 0 {
		return Pass{}, err
	}
	return passes[len(passes)-1], nil
}

func appendPass(p Pass) error {
	path := filepath.Join(runlog.Home(), "gc.log")
	line, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if len(line)+1 > maxLogBytes {
		return errors.New("gc log entry exceeds limit")
	}
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for len(old)+len(line)+1 > maxLogBytes {
		i := strings.IndexByte(string(old), '\n')
		if i < 0 {
			old = nil
			break
		}
		old = old[i+1:]
	}
	return writeAtomic(path, append(append(old, line...), '\n'))
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(runlog.Home(), ".gc-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for i := 0; i < 5; i++ {
		err = os.Rename(f.Name(), path)
		if err == nil {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return err
}

// RunAutomatic runs a due scheduled pass or a repository's post-merge pass.
func RunAutomatic(ctx context.Context, now time.Time, repo string) (bool, error) {
	var enabled struct {
		Enabled bool `json:"enabled"`
	}
	b, err := os.ReadFile(filepath.Join(runlog.Home(), "gc.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, &enabled); err != nil {
		return false, fmt.Errorf("gc.json: %w", err)
	}
	if !enabled.Enabled {
		return false, nil
	}
	c, err := LoadConfig()
	if err != nil || !c.Enabled {
		return false, err
	}
	if err := os.MkdirAll(runlog.Home(), 0700); err != nil {
		return false, err
	}
	unlock, err := Lock(now)
	if err != nil {
		var held *LockHeldError
		interval, _ := time.ParseDuration(c.Interval)
		if repo == "" && errors.As(err, &held) && !held.Since.IsZero() && now.Sub(held.Since) < interval {
			return false, nil
		}
		return false, err
	}
	defer unlock()
	if _, err := readPasses(); err != nil {
		return false, err
	}
	if repo == "" {
		interval, _ := time.ParseDuration(c.Interval)
		b, err := os.ReadFile(filepath.Join(runlog.Home(), "gc-last"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if err == nil {
			last, err := time.Parse(time.RFC3339Nano, string(b))
			if err != nil {
				return false, fmt.Errorf("gc-last: %w", err)
			}
			if !now.Before(last) && now.Sub(last) < interval {
				return false, nil
			}
		}
		if err := appendPass(Pass{Time: now, Scheduled: true, Started: true}); err != nil {
			return false, err
		}
	}
	paths := c.Roots
	if repo != "" {
		paths = []string{repo}
	}
	opt := Options{Grace: time.Hour, Idle: 24 * time.Hour, MinIdle: 2 * time.Hour, FreeBelow: c.FreeBelowBytes}
	p := Pass{Time: now, Scheduled: repo == ""}
	wts, scanErr := Scan(ctx, paths, opt)
	if scanErr == nil && repo != "" {
		for i := range wts {
			main, _ := mainCheckout(ctx, repo)
			if wts[i].Batch == "" || samePath(wts[i].Repo) != samePath(main) {
				wts[i].Verdict = Keep
			}
		}
	}
	var caches []Cache
	if scanErr == nil && repo == "" {
		caches, scanErr = Caches(ctx, paths, wts, opt)
	}
	if scanErr == nil {
		sizes := map[string]int64{}
		for _, w := range wts {
			if w.Verdict == Remove {
				sizes[w.Path] = Size(w.Path)
				p.Planned = append(p.Planned, Action{w.Path, sizes[w.Path], w.Reason, "remove"})
			}
		}
		for _, c := range caches {
			if c.Verdict == Remove || c.Verdict == Trim {
				n := c.Size
				if c.Verdict == Trim {
					n = c.TrimSize
				}
				p.Planned = append(p.Planned, Action{c.Path, n, c.Reason, string(c.Verdict)})
			}
		}
		p.Started = true
		if err := appendAutomaticPass(p); err != nil {
			return false, err
		}
		if repo == "" {
			if err := writeAtomic(filepath.Join(runlog.Home(), "gc-last"), []byte(now.Format(time.RFC3339Nano))); err != nil {
				return false, err
			}
		}
		p.Started = false
		removed, errs := Apply(ctx, wts)
		for _, w := range removed {
			p.Actions = append(p.Actions, Action{w.Path, sizes[w.Path], w.Reason, "remove"})
		}
		removedCaches, cacheErrs := ApplyCaches(ctx, caches)
		errs = append(errs, cacheErrs...)
		for _, c := range removedCaches {
			n := c.Size
			if c.Verdict == Trim {
				n = c.TrimSize
			}
			p.Actions = append(p.Actions, Action{c.Path, n, c.Reason, string(c.Verdict)})
		}
		scanErr = errors.Join(errs...)
		if scanErr == nil {
			p.Planned = nil
		}
	}
	if scanErr != nil {
		p.Error = scanErr.Error()
	}
	logErr := appendPass(p)
	if repo == "" && scanErr != nil && logErr == nil {
		logErr = writeAtomic(filepath.Join(runlog.Home(), "gc-last"), []byte(now.Format(time.RFC3339Nano)))
	}
	return true, errors.Join(scanErr, logErr)
}
