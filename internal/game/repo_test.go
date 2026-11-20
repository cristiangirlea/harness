package game

import (
	"reflect"
	"testing"
	"time"
)

func TestRepoNameAcceptsOnlyOwnerSlashName(t *testing.T) {
	for _, s := range []string{"owner/name", "Some-Org/repo.go", "a_b/c-d"} {
		if !RepoName(s) {
			t.Errorf("RepoName(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "name", "owner/", "/name", "a/b/c", "../x", "owner/..", ".owner/name", "owner/na me", `owner\name`, "harness-bot-stats-json"} {
		if RepoName(s) {
			t.Errorf("RepoName(%q) = true, want false", s)
		}
	}
}

func TestRepoSumsDeliveryForOneRepositoryWithTheLedgersRules(t *testing.T) {
	now := at(t, "2026-11-20T15:00:00Z") // a Friday; the week starts Monday 16 November
	events := []Event{
		{Key: "a", Kind: kindReview, Track: TrackDelivery, XP: 20, At: at(t, "2026-11-20T09:00:00Z"), Repo: "Owner/App"},
		{Key: "b", Kind: kindBug, Track: TrackDelivery, XP: 15, At: at(t, "2026-11-16T00:00:00Z"), Repo: "owner/app"},
		{Key: "c", Kind: kindCeiling, Track: TrackDelivery, XP: 5, At: at(t, "2026-11-15T23:59:59Z"), Repo: "owner/app"},
		{Key: "d", Kind: kindCeiling, Track: TrackDelivery, XP: 5, At: at(t, "2026-10-17T12:00:00Z"), Repo: "owner/app"},
		{Key: "e", Kind: kindCeiling, Track: TrackDelivery, XP: 5, At: at(t, "2026-10-16T23:00:00Z"), Repo: "owner/app"},
		{Key: "f", Kind: kindReview, Track: TrackDelivery, XP: 20, At: at(t, "2026-11-19T10:00:00Z"), Repo: "owner/other"},
		{Key: "g", Kind: kindRep, Track: TrackPractice, XP: 30, At: at(t, "2026-11-19T10:00:00Z"), Repo: "owner/app"},
		{Key: "h", Kind: kindReview, Track: TrackDelivery, XP: 20, At: at(t, "2026-11-18T10:00:00Z"), Repo: "owner/app"},
		{Key: "i", Kind: kindRoutedFailed, Track: TrackDelivery, At: at(t, "2026-11-17T10:00:00Z"), Repo: "owner/app"},
		{Key: "h-undo", Kind: "correction", Track: TrackDelivery, At: at(t, "2026-11-18T11:00:00Z"), Repo: "owner/app", Cancels: "h"},
	}
	got := Sum(events).Repo("owner/APP", now)
	if got.Week != 35 {
		t.Errorf("week = %d, want 35 (a and b; c is before Monday, g is practice, h is cancelled)", got.Week)
	}
	if len(got.Days) != 35 {
		t.Fatalf("days has %d entries, want 35", len(got.Days))
	}
	if got.Days[34] != 20 || got.Days[30] != 15 || got.Days[29] != 5 || got.Days[0] != 5 {
		t.Errorf("days = %v; want today 20, Monday 15, Sunday 5, and 17 October (the first day) 5", got.Days)
	}
	total := 0
	for _, d := range got.Days {
		total += d
	}
	if total != 45 {
		t.Errorf("days sum to %d, want 45: 16 October is outside the window", total)
	}
	if want := map[string]int{kindReview: 1, kindBug: 1, kindCeiling: 2}; !reflect.DeepEqual(got.Kinds, want) {
		t.Errorf("kinds = %v, want %v", got.Kinds, want)
	}
	if got.Share < 0.636 || got.Share > 0.637 {
		t.Errorf("share = %v, want 35/55", got.Share)
	}
}

func TestRepoAppliesTheDailyCapsBeforeSumming(t *testing.T) {
	now := at(t, "2026-11-20T15:00:00Z")
	var events []Event
	for i := 0; i < 6; i++ {
		events = append(events, Event{Key: string(rune('a' + i)), Kind: kindReview, Track: TrackDelivery, XP: 20, At: now.Add(-time.Duration(i) * time.Minute), Repo: "owner/app"})
	}
	if got := Sum(events).Repo("owner/app", now); got.Week != 80 || got.Kinds[kindReview] != 4 {
		t.Errorf("week %d, review count %d; want 80 and 4 (four clean reviews a day)", got.Week, got.Kinds[kindReview])
	}
}

func TestRepoOfAnEmptyWeekHasNoShareAndEmptyKinds(t *testing.T) {
	now := at(t, "2026-11-20T15:00:00Z")
	got := Sum(nil).Repo("owner/app", now)
	if got.Week != 0 || got.Share != 0 || got.Kinds == nil || len(got.Kinds) != 0 || len(got.Days) != 35 {
		t.Errorf("empty ledger gave %+v; want zero week and share, an empty kinds map and 35 days", got)
	}
}

func TestTopReposRanksOwnerSlashNameRepositoriesByThisWeeksDelivery(t *testing.T) {
	now := at(t, "2026-11-20T15:00:00Z")
	week := WeekStart(now)
	day := at(t, "2026-11-19T10:00:00Z")
	var events []Event
	add := func(repo string, xp int, when time.Time) {
		events = append(events, Event{Key: repo + when.String() + string(rune('a'+len(events))), Kind: "test", Track: TrackDelivery, XP: xp, At: when, Repo: repo})
	}
	add("o/a", 10, day)
	add("O/A", 5, day)
	add("o/b", 30, day)
	add("o/c", 15, day)
	add("o/d", 15, day)
	add("o/e", 1, day)
	add("o/f", 2, day)
	add("o/old", 100, at(t, "2026-11-10T10:00:00Z"))
	add("folder-name", 500, day)
	add("C:/work/x", 500, day)
	add("", 500, day)
	events = append(events, Event{Key: "practice", Kind: kindRep, Track: TrackPractice, XP: 300, At: day, Repo: "o/practice"})
	got := Sum(events).TopRepos(week, 5)
	want := []RepoWeek{{"o/b", 30}, {"o/a", 15}, {"o/c", 15}, {"o/d", 15}, {"o/f", 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("top = %v, want %v (ties by name; o/a and O/A are one repository)", got, want)
	}
	if got := Sum(nil).TopRepos(week, 5); got == nil || len(got) != 0 {
		t.Errorf("top of an empty ledger = %#v, want an empty, non-nil list", got)
	}
}
