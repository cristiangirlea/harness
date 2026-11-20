package game

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// RepoDays is how many UTC days a repository's delivery strip covers, today last.
const RepoDays = 35

var ownerName = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*/([A-Za-z0-9_.-]+)$`)

// RepoName reports whether s is an owner/name repository; eval folders and bare folder names are not.
// A name may start with a dot (owner/.github), but is never . or .. alone.
func RepoName(s string) bool {
	m := ownerName.FindStringSubmatch(s)
	return m != nil && m[1] != "." && m[1] != ".."
}

// RepoDelivery is one repository's counted delivery experience.
type RepoDelivery struct {
	Week int
	// Days is the delivery experience per UTC day, oldest first, today last.
	Days []int
	// Kinds counts the counted delivery events in the days, by kind.
	Kinds map[string]int
	// Share is Week as a part of the week's delivery in every repository, 0 when that is none.
	Share float64
}

// Repo sums the counted delivery events of one repository, matched without regard to case.
func (t Totals) Repo(repo string, now time.Time) RepoDelivery {
	d := RepoDelivery{Days: make([]int, RepoDays), Kinds: map[string]int{}}
	week := WeekStart(now)
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	first := today.AddDate(0, 0, -(RepoDays - 1))
	all := 0
	for _, e := range t.Counted {
		if e.Track != TrackDelivery {
			continue
		}
		at := e.At.UTC()
		// The week has no upper bound, as in Since, so this figure, the share and TopRepos agree.
		if !at.Before(week) {
			all += e.XP
		}
		if !strings.EqualFold(e.Repo, repo) {
			continue
		}
		if !at.Before(week) {
			d.Week += e.XP
		}
		if at.Before(first) || !at.Before(today.AddDate(0, 0, 1)) {
			continue
		}
		d.Days[int(at.Sub(first)/(24*time.Hour))] += e.XP
		// A correction or a failed routed run brought nothing, so it is not part of what was delivered.
		if e.XP > 0 && e.Cancels == "" {
			d.Kinds[e.Kind]++
		}
	}
	if all > 0 {
		d.Share = float64(d.Week) / float64(all)
	}
	return d
}

// RepoWeek is a repository's delivery experience since a time.
type RepoWeek struct {
	Repo string
	Week int
}

// TopRepos ranks owner/name repositories by delivery since from, most first, then by name.
func (t Totals) TopRepos(from time.Time, n int) []RepoWeek {
	byKey := map[string]*RepoWeek{}
	// A repository spelled two ways is shown as the spelling that brought the most experience.
	spelled := map[string]map[string]int{}
	for _, e := range t.Counted {
		if e.Track != TrackDelivery || e.At.Before(from) || !RepoName(e.Repo) {
			continue
		}
		key := strings.ToLower(e.Repo)
		if byKey[key] == nil {
			byKey[key] = &RepoWeek{}
			spelled[key] = map[string]int{}
		}
		byKey[key].Week += e.XP
		spelled[key][e.Repo] += e.XP
	}
	top := []RepoWeek{}
	for key, r := range byKey {
		if r.Week <= 0 {
			continue
		}
		for name, xp := range spelled[key] {
			if r.Repo == "" || xp > spelled[key][r.Repo] || (xp == spelled[key][r.Repo] && name < r.Repo) {
				r.Repo = name
			}
		}
		top = append(top, *r)
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].Week != top[j].Week {
			return top[i].Week > top[j].Week
		}
		return strings.ToLower(top[i].Repo) < strings.ToLower(top[j].Repo)
	})
	if len(top) > n {
		top = top[:n]
	}
	return top
}
