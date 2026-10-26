package game

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func ev(t *testing.T, key, kind, track string, xp int, when string) Event {
	t.Helper()
	return Event{Key: key, Kind: kind, Track: track, XP: xp, At: at(t, when), Repo: "example/app"}
}

func keysOf(events []Event) []string {
	var keys []string
	for _, e := range events {
		keys = append(keys, e.Key)
	}
	return keys
}

func TestAppendAddsOnlyTheKeysTheLedgerLacks(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	a := ev(t, "review:r1", kindReview, TrackDelivery, 20, "2026-10-26T10:00:00Z")
	b := ev(t, "ceiling:r1", kindCeiling, TrackDelivery, 5, "2026-10-26T10:00:00Z")
	if n, err := Append([]Event{a, b, a}); err != nil || n != 2 {
		t.Fatalf("first Append = %d, %v; want 2 added", n, err)
	}
	if n, err := Append([]Event{b, a}); err != nil || n != 0 {
		t.Fatalf("second Append = %d, %v; want nothing added", n, err)
	}
	data, err := os.ReadFile(filepath.Join(Dir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(data), "\n"); lines != 2 {
		t.Fatalf("ledger has %d lines, want 2:\n%s", lines, data)
	}
	got, unreadable, err := Load()
	if err != nil || unreadable != 0 {
		t.Fatalf("Load: %v, %d unreadable", err, unreadable)
	}
	if !reflect.DeepEqual(got, []Event{a, b}) {
		t.Fatalf("Load = %+v, want %+v", got, []Event{a, b})
	}
}

func TestAppendWritesNothingWhenNothingIsNew(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if n, err := Append(nil); err != nil || n != 0 {
		t.Fatalf("Append(nil) = %d, %v", n, err)
	}
	if _, err := os.Stat(Dir()); !os.IsNotExist(err) {
		t.Fatalf("game directory made for nothing: %v", err)
	}
}

func TestLoadKeepsTheFirstLineOfAKeyAndCountsBrokenLines(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := `{"key":"review:r1","kind":"review_clean","track":"delivery","xp":20,"at":"2026-10-26T10:00:00Z","repo":"example/app"}
{half a line
{"key":"review:r1","kind":"review_clean","track":"delivery","xp":99,"at":"2026-10-26T11:00:00Z","repo":"example/app"}
{"kind":"review_clean","track":"delivery","xp":20,"at":"2026-10-26T11:00:00Z","repo":"example/app"}
`
	if err := os.WriteFile(filepath.Join(Dir(), "events.jsonl"), []byte(ledger), 0o644); err != nil {
		t.Fatal(err)
	}
	got, unreadable, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if unreadable != 2 {
		t.Errorf("unreadable = %d, want 2 (the half line and the line without a key)", unreadable)
	}
	if len(got) != 1 || got[0].XP != 20 {
		t.Fatalf("Load = %+v, want only the first review:r1", got)
	}
}

func TestLoadOfNoLedgerIsEmpty(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	got, unreadable, err := Load()
	if err != nil || unreadable != 0 || len(got) != 0 {
		t.Fatalf("Load = %v, %d, %v; want nothing", got, unreadable, err)
	}
}

func TestAppendStartsOnANewLineAfterATornWrite(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	torn := `{"key":"review:r1","kind":"review_clean","track":"delivery","xp":20,"at":"2026-10-26T10:00:00Z","repo":"example/app"}
{"key":"ceiling:r`
	if err := os.WriteFile(filepath.Join(Dir(), "events.jsonl"), []byte(torn), 0o644); err != nil {
		t.Fatal(err)
	}
	b := ev(t, "routed:r2", kindRouted, TrackDelivery, 10, "2026-10-26T12:00:00Z")
	if n, err := Append([]Event{b}); err != nil || n != 1 {
		t.Fatalf("Append = %d, %v", n, err)
	}
	got, unreadable, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"review:r1", "routed:r2"}; !reflect.DeepEqual(keysOf(got), want) || unreadable != 1 {
		t.Fatalf("Load = %v with %d unreadable; want %v with the torn line alone unreadable", keysOf(got), unreadable, want)
	}
}

func TestSumAppliesCapsInTheOrderOfAtThenKey(t *testing.T) {
	events := []Event{
		ev(t, "review:f", kindReview, TrackDelivery, 20, "2026-10-26T15:00:00Z"),
		ev(t, "review:b", kindReview, TrackDelivery, 20, "2026-10-26T09:00:00Z"),
		ev(t, "review:e", kindReview, TrackDelivery, 20, "2026-10-26T12:00:00Z"),
		ev(t, "review:a", kindReview, TrackDelivery, 20, "2026-10-26T09:00:00Z"),
		ev(t, "review:d", kindReview, TrackDelivery, 20, "2026-10-26T11:00:00Z"),
		ev(t, "review:c", kindReview, TrackDelivery, 20, "2026-10-26T11:00:00Z"),
	}
	want := []string{"review:a", "review:b", "review:c", "review:d"}
	for _, order := range [][]Event{events, reversed(events)} {
		tot := Sum(order)
		if got := keysOf(tot.Counted); !reflect.DeepEqual(got, want) {
			t.Fatalf("counted %v, want the four earliest by at then key %v", got, want)
		}
		if tot.XP[TrackDelivery] != 80 || tot.Person != 80 {
			t.Fatalf("XP = %v, person %d; want 80 (four reviews a day)", tot.XP, tot.Person)
		}
	}
}

func reversed(events []Event) []Event {
	out := make([]Event, len(events))
	for i, e := range events {
		out[len(events)-1-i] = e
	}
	return out
}

func TestSumDropsCancelledEventsBeforeApplyingCaps(t *testing.T) {
	events := []Event{
		ev(t, "review:a", kindReview, TrackDelivery, 20, "2026-10-26T09:00:00Z"),
		ev(t, "review:b", kindReview, TrackDelivery, 20, "2026-10-26T10:00:00Z"),
		ev(t, "review:c", kindReview, TrackDelivery, 20, "2026-10-26T11:00:00Z"),
		ev(t, "review:d", kindReview, TrackDelivery, 20, "2026-10-26T12:00:00Z"),
		ev(t, "review:e", kindReview, TrackDelivery, 20, "2026-10-26T13:00:00Z"),
	}
	correction := ev(t, "revert:example/app#1", "revert", TrackDelivery, 0, "2026-10-27T09:00:00Z")
	correction.Cancels = "review:a"
	tot := Sum(append([]Event{correction}, events...))
	if tot.XP[TrackDelivery] != 80 {
		t.Fatalf("XP = %d, want 80: the cancelled review frees a place under the cap for review:e", tot.XP[TrackDelivery])
	}
	for _, k := range keysOf(tot.Counted) {
		if k == "review:a" {
			t.Fatal("a cancelled event still counts")
		}
	}
	if got := keysOf(tot.Counted); !reflect.DeepEqual(got[:4], []string{"review:b", "review:c", "review:d", "review:e"}) {
		t.Fatalf("counted %v", got)
	}
}

func TestSumCapsPerUTCDateOfAt(t *testing.T) {
	events := []Event{
		ev(t, "routed:1", kindRouted, TrackDelivery, 10, "2026-10-26T08:00:00Z"),
		ev(t, "routed:2", kindRouted, TrackDelivery, 10, "2026-10-26T09:00:00Z"),
		ev(t, "routed:3", kindRouted, TrackDelivery, 10, "2026-10-26T10:00:00Z"),
		// 27 October on its own clock, 26 October 23:30 in UTC: over the cap of three.
		ev(t, "routed:late", kindRouted, TrackDelivery, 10, "2026-10-27T01:30:00+02:00"),
		// 26 October on its own clock, 27 October 01:30 in UTC: a new day.
		ev(t, "routed:next", kindRouted, TrackDelivery, 10, "2026-10-26T23:30:00-02:00"),
	}
	tot := Sum(events)
	if tot.XP[TrackDelivery] != 40 {
		t.Fatalf("XP = %d, want 40 (three on 26 October UTC, one on 27 October UTC)", tot.XP[TrackDelivery])
	}
	if want := []string{"routed:1", "routed:2", "routed:3", "routed:next"}; !reflect.DeepEqual(keysOf(tot.Counted), want) {
		t.Fatalf("counted %v, want %v", keysOf(tot.Counted), want)
	}
}

func TestFirstUseIsNotCappedPerDay(t *testing.T) {
	var events []Event
	for _, f := range []string{"batch", "eval", "routed", "slot"} {
		events = append(events, ev(t, "first:"+f, kindFirst, TrackDelivery, 25, "2026-10-26T08:00:00Z"))
	}
	if got := Sum(events).XP[TrackDelivery]; got != 100 {
		t.Fatalf("XP = %d, want 100 for four first uses on one day", got)
	}
}

func TestPersonCountsDeliveryAndPracticeOnly(t *testing.T) {
	tot := Sum([]Event{
		ev(t, "review:a", kindReview, TrackDelivery, 20, "2026-10-26T09:00:00Z"),
		ev(t, "rep:2026-10-26", "daily_practice", TrackPractice, 30, "2026-10-26T09:00:00Z"),
		ev(t, "ada:setup:e1", "ada_setup", TrackAda, 30, "2026-10-26T09:00:00Z"),
		ev(t, "badge:budget-keeper", kindBadge, TrackBadge, 0, "2026-10-26T09:00:00Z"),
	})
	if tot.Person != 50 {
		t.Fatalf("person = %d, want 50 (delivery 20 + practice 30)", tot.Person)
	}
	if tot.XP[TrackAda] != 30 {
		t.Fatalf("ada = %d, want her 30 kept on her own track", tot.XP[TrackAda])
	}
	if want := []string{"badge:budget-keeper"}; !reflect.DeepEqual(keysOf(tot.Badges), want) {
		t.Fatalf("badges %v, want %v", keysOf(tot.Badges), want)
	}
}

func TestSinceSumsTheCountedExperiencePerTrack(t *testing.T) {
	tot := Sum([]Event{
		ev(t, "review:old", kindReview, TrackDelivery, 20, "2026-10-25T23:59:00Z"),
		ev(t, "review:new", kindReview, TrackDelivery, 20, "2026-10-26T00:00:00Z"),
		ev(t, "rep:2026-10-27", "daily_practice", TrackPractice, 30, "2026-10-27T09:00:00Z"),
	})
	got := tot.Since(at(t, "2026-10-26T00:00:00Z"))
	if got[TrackDelivery] != 20 || got[TrackPractice] != 30 {
		t.Fatalf("Since = %v, want delivery 20 and practice 30", got)
	}
}

func TestLevelIsTheFloorOfTheSquareRootOfXPOverFifty(t *testing.T) {
	for xp, want := range map[int]int{0: 0, 49: 0, 50: 1, 199: 1, 200: 2, 449: 2, 450: 3, 1249: 4, 1250: 5, 4999: 9, 5000: 10, 1 << 40: 148291} {
		if got := Level(xp); got != want {
			t.Errorf("Level(%d) = %d, want %d", xp, got, want)
		}
	}
}

func TestWeekStartsOnMondayInUTC(t *testing.T) {
	for in, want := range map[string]string{
		"2026-10-28T10:00:00+02:00": "2026-10-26T00:00:00Z",
		"2026-10-26T00:00:00Z":      "2026-10-26T00:00:00Z",
		// Monday on its own clock, still Sunday in UTC.
		"2026-10-26T00:30:00+02:00": "2026-10-19T00:00:00Z",
		"2026-11-01T23:59:59Z":      "2026-10-26T00:00:00Z",
	} {
		if got := WeekStart(at(t, in)); !got.Equal(at(t, want)) || got.Location() != time.UTC {
			t.Errorf("WeekStart(%s) = %v, want %s", in, got, want)
		}
	}
}

func TestBadgeNamesReadAsWords(t *testing.T) {
	for key, want := range map[string]string{
		"badge:budget-keeper":   "Budget keeper",
		"badge:cheap-and-right": "Cheap and right",
		"badge:veteran":         "Veteran",
		"badge:some-new-one":    "Some new one",
	} {
		if got := BadgeName(key); got != want {
			t.Errorf("BadgeName(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestSettingsLeaveTheGameOnUnlessEnabledIsFalse(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	s, err := LoadSettings()
	if err != nil || !s.On() {
		t.Fatalf("no settings.json: %+v, %v; want the game on", s, err)
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]bool{`{}`: true, `{"enabled": true}`: true, `{"enabled": false}`: false, `{"enabled": false, "voice": {}}`: false} {
		if err := os.WriteFile(filepath.Join(Dir(), "settings.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := LoadSettings()
		if err != nil || s.On() != want {
			t.Errorf("settings %s: On() = %v, %v; want %v", body, s.On(), err, want)
		}
	}
	if err := os.WriteFile(filepath.Join(Dir(), "settings.json"), []byte(`{"enabled": fals`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(); err == nil {
		t.Error("a settings.json that cannot be parsed must be an error, not a guess")
	}
}
