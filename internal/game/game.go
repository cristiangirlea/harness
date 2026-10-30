// Package game keeps the experience ledger: events derived from hrn's records, summed with daily caps.
package game

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/cristiangirlea/harness/internal/runlog"
)

const (
	TrackDelivery = "delivery"
	TrackPractice = "practice"
	TrackAda      = "ada"
	TrackBadge    = "badge"
)

const (
	kindReview       = "review_clean"
	kindBug          = "bug_squashed"
	kindCeiling      = "inside_ceiling"
	kindRouted       = "routed_passed"
	kindRoutedFailed = "routed_failed"
	kindScript       = "script_reused"
	kindFirst        = "first_use"
	kindBadge        = "badge"
)

// rules gives each kind its experience and its cap per UTC day; 0 means no cap.
var rules = map[string]struct{ xp, cap int }{
	kindReview:       {20, 4},
	kindBug:          {15, 3},
	kindCeiling:      {5, 10},
	kindRouted:       {10, 3},
	kindRoutedFailed: {0, 0},
	kindScript:       {5, 5},
	kindFirst:        {25, 0},
	kindRep:          {30, 1},
	kindRead:         {20, 3},
}

// Event is one line of events.jsonl, written once and never edited.
type Event struct {
	Key   string    `json:"key"`
	Kind  string    `json:"kind"`
	Track string    `json:"track"`
	XP    int       `json:"xp"`
	At    time.Time `json:"at"`
	Repo  string    `json:"repo"`
	// Cancels is the key of an event this correction takes back.
	Cancels string `json:"cancels,omitempty"`
}

// Dir is $HRN_HOME/game.
func Dir() string { return filepath.Join(runlog.Home(), "game") }

func ledgerPath() string { return filepath.Join(Dir(), "events.jsonl") }

// Load reads the ledger; the first line of a key wins, and broken lines are counted, not fatal.
func Load() (events []Event, unreadable int, err error) {
	f, err := os.Open(ledgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	seen := map[string]bool{}
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var e Event
			if json.Unmarshal(line, &e) != nil || e.Key == "" {
				unreadable++
			} else if !seen[e.Key] {
				seen[e.Key] = true
				events = append(events, e)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				readErr = nil
			}
			return events, unreadable, readErr
		}
	}
}

// Append adds the events whose key the ledger lacks and returns how many it added.
func Append(events []Event) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	old, _, err := Load()
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	for _, e := range old {
		seen[e.Key] = true
	}
	var b bytes.Buffer
	n := 0
	for _, e := range events {
		if seen[e.Key] {
			continue
		}
		seen[e.Key] = true
		e.At = e.At.UTC()
		if err := json.NewEncoder(&b).Encode(e); err != nil {
			return 0, err
		}
		n++
	}
	if n == 0 {
		return 0, nil
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return 0, err
	}
	if f, err := os.Open(ledgerPath()); err == nil {
		info, statErr := f.Stat()
		if statErr == nil && info.Size() > 0 {
			var last [1]byte
			_, statErr = f.ReadAt(last[:], info.Size()-1)
			if statErr == nil && last[0] != '\n' {
				data := append([]byte{'\n'}, b.Bytes()...)
				b.Reset()
				b.Write(data)
			}
		}
		if err := errors.Join(statErr, f.Close()); err != nil {
			return 0, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	f, err := os.OpenFile(ledgerPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	written, err := f.Write(b.Bytes())
	if err == nil && written != b.Len() {
		err = io.ErrShortWrite
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return 0, err
	}
	return n, nil
}

// Totals is the ledger summed: cancelled events dropped, then caps per kind and UTC date.
type Totals struct {
	// Counted holds the events that count, in the order of at, then key.
	Counted []Event
	XP      map[string]int
	// Person is the delivery and practice experience; Ada's and badges are apart.
	Person int
	// Badges are the badge events not cancelled, in the order of at, then key.
	Badges []Event
}

// Sum applies the read-time rules, so the order of the lines never changes the totals.
func Sum(events []Event) Totals {
	t := Totals{XP: map[string]int{}}
	counts := map[string]int{}
	for _, e := range ordered(events) {
		if cap := rules[e.Kind].cap; cap > 0 {
			key := e.Kind + ":" + e.At.UTC().Format("2006-01-02")
			if counts[key] >= cap {
				continue
			}
			counts[key]++
		}
		t.Counted = append(t.Counted, e)
		t.XP[e.Track] += e.XP
		if e.Track == TrackBadge {
			t.Badges = append(t.Badges, e)
		}
	}
	t.Person = t.XP[TrackDelivery] + t.XP[TrackPractice]
	return t
}

func ordered(events []Event) []Event {
	seen, cancelled := map[string]bool{}, map[string]bool{}
	var out []Event
	for _, e := range events {
		if seen[e.Key] {
			continue
		}
		seen[e.Key] = true
		out = append(out, e)
		if e.Cancels != "" {
			cancelled[e.Cancels] = true
		}
	}
	kept := out[:0]
	for _, e := range out {
		if !cancelled[e.Key] {
			kept = append(kept, e)
		}
	}
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].At.Equal(kept[j].At) {
			return kept[i].Key < kept[j].Key
		}
		return kept[i].At.Before(kept[j].At)
	})
	return kept
}

// Since sums the counted experience per track from a time on.
func (t Totals) Since(from time.Time) map[string]int {
	xp := map[string]int{}
	for _, e := range t.Counted {
		if !e.At.Before(from) {
			xp[e.Track] += e.XP
		}
	}
	return xp
}

// Level is floor(sqrt(xp / 50)).
func Level(xp int) int {
	if xp <= 0 {
		return 0
	}
	n := xp / 50
	l := int(math.Sqrt(float64(n)))
	for l+1 <= n/(l+1) {
		l++
	}
	for l > 0 && l > n/l {
		l--
	}
	return l
}

// WeekStart is the Monday 00:00 UTC on or before now.
func WeekStart(now time.Time) time.Time {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
}

// BadgeName is how a badge's key reads in the terminal.
func BadgeName(key string) string {
	name := []rune(strings.ReplaceAll(strings.TrimPrefix(key, "badge:"), "-", " "))
	if len(name) > 0 {
		name[0] = unicode.ToUpper(name[0])
	}
	return string(name)
}

// Settings is settings.json; a missing file or field leaves the game on.
type Settings struct {
	Enabled *bool `json:"enabled,omitempty"`
}

func settingsPath() string { return filepath.Join(Dir(), "settings.json") }

// LoadSettings reads settings.json; one that cannot be parsed is an error, not a guess.
func LoadSettings() (Settings, error) {
	var s Settings
	data, err := os.ReadFile(settingsPath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &s)
	}
	if err != nil {
		return s, fmt.Errorf("%s: %w", settingsPath(), err)
	}
	return s, nil
}

// On reports whether the game is shown; the ledger records either way.
func (s Settings) On() bool { return s.Enabled == nil || *s.Enabled }
