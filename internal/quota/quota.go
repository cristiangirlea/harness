// Package quota tells how much of a subscription plan's quota is left, per
// window (five hours, a week), as the plan's own CLI last reported it: codex
// in its session files, claude in the quota events hrn records from its runs.
// Only the numbers are read; nothing else from those files is kept or shown.
package quota

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// Window is one window of a plan's quota.
type Window struct {
	Left       float64 // percent of the window left, 0 to 100
	ResetsAt   time.Time
	ReportedAt time.Time
}

// Quota is what is known of a plan's windows; an unknown window is nil.
type Quota struct {
	FiveHour, Week *Window
}

// lowPercent: below this much left, a window is shown as low.
const lowPercent = 10

// Part is one window described for a person.
type Part struct {
	Text  string
	Low   bool // under lowPercent left
	Stale bool // its reset time has passed, so the figure is no longer current
}

// Parts describes each known window, the five-hour one first.
func (q Quota) Parts(now time.Time) []Part {
	var out []Part
	for _, w := range []struct {
		label string
		w     *Window
	}{{"5h", q.FiveHour}, {"week", q.Week}} {
		if w.w == nil {
			continue
		}
		left := fmt.Sprintf("%s: %.0f%% left", w.label, w.w.Left)
		if !w.w.ResetsAt.After(now) {
			out = append(out, Part{Text: left + " before its reset " + span(now.Sub(w.w.ResetsAt)) + " ago (stale)", Stale: true})
			continue
		}
		out = append(out, Part{Text: left + ", resets in " + span(w.w.ResetsAt.Sub(now)), Low: w.w.Left < lowPercent})
	}
	return out
}

// String is Parts on one line, or "" when nothing is known.
func (q Quota) String(now time.Time) string {
	var texts []string
	for _, p := range q.Parts(now) {
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, " / ")
}

// span is a duration in its two largest units: 3d 4h, 1h 12m, 5m.
func span(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%dd %dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
}

// cacheFor is how long an answer is reused: the Backends card asks on every
// page load, and the files change only as runs go.
const cacheFor = 30 * time.Second

type cached struct {
	at time.Time
	q  Quota
}

var (
	mu    sync.Mutex
	cache = map[string]cached{}
)

// For is the quota of a plan backend (claude, codex) as last reported, cached
// for 30 seconds; other backends have no plan quota and get nothing.
func For(backend string, now time.Time) Quota {
	var key string
	var read func() Quota
	switch backend {
	case "codex":
		root := codexSessions()
		key, read = "codex\x00"+root, func() Quota { return codexFrom(root) }
	case "claude":
		root := runlog.Root()
		key, read = "claude\x00"+root, func() Quota { return claudeFrom(root) }
	default:
		return Quota{}
	}
	mu.Lock()
	defer mu.Unlock()
	if c, ok := cache[key]; ok && now.Sub(c.at) >= 0 && now.Sub(c.at) < cacheFor {
		return c.q
	}
	q := read()
	cache[key] = cached{at: now, q: q}
	return q
}

// newer keeps, per window, the one reported last.
func (q *Quota) newer(o Quota) {
	if o.FiveHour != nil && (q.FiveHour == nil || o.FiveHour.ReportedAt.After(q.FiveHour.ReportedAt)) {
		q.FiveHour = o.FiveHour
	}
	if o.Week != nil && (q.Week == nil || o.Week.ReportedAt.After(q.Week.ReportedAt)) {
		q.Week = o.Week
	}
}

func windowOf(used float64, resets int64, at time.Time) *Window {
	if math.IsNaN(used) || math.IsInf(used, 0) || resets <= 0 {
		return nil
	}
	return &Window{Left: min(100, max(0, 100-used)), ResetsAt: time.Unix(resets, 0), ReportedAt: at}
}

// The tail of a file that is read: enough for the last few records, and a
// bound on what a reader holds of a session it has no business with.
const defaultTailBytes = 64 << 10

var tailBytes int64 = defaultTailBytes

// tail is the last tailBytes of a file, and whether it starts inside a line.
func tail(path string) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("not a regular file")
	}
	// One byte more than the tail says whether it starts on a line's start.
	start := max(0, info.Size()-tailBytes-1)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, false, err
	}
	data, err := io.ReadAll(io.LimitReader(f, tailBytes+1))
	return data, start > 0, err
}

// lines splits data into lines, dropping the first one when it is partial.
func lines(data []byte, partial bool) [][]byte {
	if partial {
		_, rest, ok := bytes.Cut(data, []byte("\n"))
		if !ok {
			return nil
		}
		data = rest
	}
	return bytes.Split(data, []byte("\n"))
}

// codex writes its session files at $CODEX_HOME/sessions/YYYY/MM/DD/*.jsonl.
func codexSessions() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

// maxCodexFiles bounds a reading: the newest files by their dated folders
// and names (a session's file is named for when it started).
const maxCodexFiles = 12

// codexRecord is the part of a session line the quota is read from; nothing
// else in the line is decoded.
type codexRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Payload   struct {
		Type       string `json:"type"`
		RateLimits *struct {
			LimitID   string       `json:"limit_id"`
			Primary   *codexWindow `json:"primary"`
			Secondary *codexWindow `json:"secondary"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

type codexWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

// parseCodexTail reads the token_count records in a session file's tail. A
// window is known by its length (300 minutes, 10080), not by which of
// primary and secondary carries it: a plan may have only the weekly one.
func parseCodexTail(data []byte, partial bool) Quota {
	var q Quota
	for _, line := range lines(data, partial) {
		if !bytes.Contains(line, []byte(`"token_count"`)) || !bytes.Contains(line, []byte(`"rate_limits"`)) {
			continue
		}
		var l codexRecord
		if json.Unmarshal(line, &l) != nil || l.Payload.Type != "token_count" || l.Payload.RateLimits == nil {
			continue
		}
		// Other limit ids are separate limits (a model's own); the plan's is codex.
		if id := l.Payload.RateLimits.LimitID; id != "" && id != "codex" {
			continue
		}
		var got Quota
		for _, w := range []*codexWindow{l.Payload.RateLimits.Primary, l.Payload.RateLimits.Secondary} {
			if w == nil {
				continue
			}
			switch w.WindowMinutes {
			case 300:
				got.FiveHour = windowOf(w.UsedPercent, w.ResetsAt, l.Timestamp)
			case 7 * 24 * 60:
				got.Week = windowOf(w.UsedPercent, w.ResetsAt, l.Timestamp)
			}
		}
		q.newer(got)
	}
	return q
}

// codexFrom reads the tails of the newest session files under root.
func codexFrom(root string) Quota {
	var q Quota
	if root == "" {
		return q
	}
	files := 0
	// Year, month and day folders, newest first; anything else is passed over.
	var walk func(dir string, depth int) bool
	walk = func(dir string, depth int) bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return true
		}
		slices.Reverse(entries) // ReadDir sorts by name
		for _, e := range entries {
			name := e.Name()
			if depth < 3 {
				if e.IsDir() && digits(name, depth) && !walk(filepath.Join(dir, name), depth+1) {
					return false
				}
				continue
			}
			if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			if data, partial, err := tail(filepath.Join(dir, name)); err == nil {
				q.newer(parseCodexTail(data, partial))
			}
			if files++; files >= maxCodexFiles {
				return false
			}
		}
		return true
	}
	walk(root, 0)
	return q
}

// digits: a year folder has four digits, a month or day folder two.
func digits(name string, depth int) bool {
	want := 2
	if depth == 0 {
		want = 4
	}
	return len(name) == want && strings.Trim(name, "0123456789") == ""
}

// maxClaudeRuns bounds a reading: the newest runs, by id.
const maxClaudeRuns = 40

// claudeFrom reads the quota events of the newest claude runs under root,
// stopping once both windows are known.
func claudeFrom(root string) Quota {
	var q Quota
	entries, err := os.ReadDir(root)
	if err != nil {
		return q
	}
	slices.Reverse(entries) // run ids start with when they started
	runs := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if runs++; runs > maxClaudeRuns {
			break
		}
		dir := filepath.Join(root, e.Name())
		if !claudeRun(dir) {
			continue
		}
		data, partial, err := tail(filepath.Join(dir, "events.jsonl"))
		if err != nil {
			continue
		}
		var got Quota
		for _, line := range lines(data, partial) {
			if !bytes.Contains(line, []byte(`"quota"`)) {
				continue
			}
			var ev agent.Event
			if json.Unmarshal(line, &ev) != nil || ev.Kind != agent.EventQuota || ev.Quota == nil {
				continue
			}
			var this Quota
			if w := ev.Quota.FiveHour; w != nil {
				this.FiveHour = windowOf(w.UsedPercent, w.ResetsAt, ev.Time)
			}
			if w := ev.Quota.Week; w != nil {
				this.Week = windowOf(w.UsedPercent, w.ResetsAt, ev.Time)
			}
			got.newer(this)
		}
		// A newer run's window stands; an older run only fills what is missing.
		if q.FiveHour == nil {
			q.FiveHour = got.FiveHour
		}
		if q.Week == nil {
			q.Week = got.Week
		}
		if q.FiveHour != nil && q.Week != nil {
			break
		}
	}
	return q
}

// claudeRun reports whether the run in dir ran on claude, from its task.json.
func claudeRun(dir string) bool {
	f, err := os.Open(filepath.Join(dir, "task.json"))
	if err != nil {
		return false
	}
	defer f.Close()
	var rec struct {
		Backend string `json:"backend"`
	}
	return json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&rec) == nil && rec.Backend == "claude"
}
