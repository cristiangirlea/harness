package game

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakePracticeSource struct {
	items    map[string][]Candidate
	merged   bool
	mergedAt time.Time
	fail     bool
}

func (f fakePracticeSource) Candidates(kind string) ([]Candidate, error) {
	if f.fail && kind != "recall" {
		return nil, fmt.Errorf("gh failed")
	}
	return f.items[kind], nil
}
func (f fakePracticeSource) Read(ref string) (Candidate, error) {
	return Candidate{Kind: "read", Source: ref, Material: "diff", Reveal: "secret reveal"}, nil
}
func (f fakePracticeSource) Merged(ref string, after time.Time) (bool, error) {
	return f.merged || (!f.mergedAt.IsZero() && f.mergedAt.After(after)), nil
}

func TestPracticePersistsMasksAndHidesReveal(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	f := fakePracticeSource{items: map[string][]Candidate{"recall": {{Kind: "recall", Source: "repo#decision", Material: "TOKEN=ghp_fakeSecret123", Reveal: "TOKEN=ghp_fakeSecret456"}}}}
	p := NewPractice(f, now)
	a, err := p.Today()
	if err != nil {
		t.Fatal(err)
	}
	if a.Reveal != "" || strings.Contains(a.Material, "ghp_fake") {
		t.Fatalf("unsafe response: %+v", a)
	}
	b, err := p.Today()
	if err != nil || a != b {
		t.Fatalf("reload: %+v %v", b, err)
	}
	raw, err := os.ReadFile(filepath.Join(Dir(), "reps", "2026-10-30.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ghp_fake") {
		t.Fatal("secret stored")
	}
	b, err = p.Swap()
	if err != nil || b.Attempt != a.Attempt+1 {
		t.Fatalf("swap: %+v %v", b, err)
	}
	reveal, err := p.Answer("my answer")
	if err != nil || reveal == "" || strings.Contains(reveal, "ghp_fake") {
		t.Fatalf("reveal: %q %v", reveal, err)
	}
	b, err = p.Today()
	if err != nil || b.Reveal != reveal {
		t.Fatalf("answered: %+v %v", b, err)
	}
	if _, err := p.Swap(); err == nil {
		t.Fatal("swapped after answer")
	}
}

func TestPracticeRebuildReadAndStreak(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	f := fakePracticeSource{items: map[string][]Candidate{"recall": {{Kind: "recall", Source: "decision", Material: "heading", Reveal: "text"}}}}
	p := NewPractice(f, now.AddDate(0, 0, -1))
	if _, err := p.Today(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Answer("answer"); err != nil {
		t.Fatal(err)
	}
	p = NewPractice(f, now)
	streak, days, err := p.Streak()
	if err != nil || streak != 1 || !days[33] || days[34] {
		t.Fatalf("streak %d %v %v", streak, days, err)
	}
	if _, err := p.Read("owner/repo#1"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Answer("answer"); err != nil {
		t.Fatal(err)
	}
	if err := p.Rebuild(); err != nil {
		t.Fatal(err)
	}
	events, _, _ := Load()
	for _, e := range events {
		if e.Key == "read:owner/repo#1" {
			t.Fatal("counted before merge")
		}
	}
	f.merged = true
	p = NewPractice(f, now)
	if err := p.Rebuild(); err != nil {
		t.Fatal(err)
	}
	if err := p.Rebuild(); err != nil {
		t.Fatal(err)
	}
	events, _, _ = Load()
	n := 0
	for _, e := range events {
		if e.Key == "read:owner/repo#1" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("read count %d", n)
	}
}
func TestPracticeFallbackCapsAndBrokenStreak(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	f := fakePracticeSource{items: map[string][]Candidate{"recall": {{Source: "decision", Material: "heading", Reveal: "body"}}}, merged: true}
	p := NewPractice(f, now)
	e, err := p.Today()
	if err != nil || e.Kind != "recall" {
		t.Fatalf("fallback: %+v %v", e, err)
	}
	for i := 1; i <= 3; i++ {
		ref := fmt.Sprintf("owner/repo#%d", i)
		if _, err = p.Read(ref); err != nil {
			t.Fatal(err)
		}
		if _, err = p.Answer("answer"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = p.Read("owner/repo#4"); err == nil {
		t.Fatal("fourth read accepted")
	}
	if _, err = p.Today(); err != nil {
		t.Fatal(err)
	}
	// Select the daily exercise after the reads.
	r, err := loadRep(p.date())
	if err != nil {
		t.Fatal(err)
	}
	r.Pending = "daily"
	if err = saveRep(p.date(), r); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Answer("answer"); err != nil {
		t.Fatal(err)
	}
	events, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := Sum(events).XP[TrackPractice]; got != 90 {
		t.Fatalf("xp %d", got)
	}
	later := NewPractice(f, now.AddDate(0, 0, 2))
	streak, _, err := later.Streak()
	if err != nil || streak != 0 {
		t.Fatalf("broken streak %d %v", streak, err)
	}
}

func TestRecallReadsCommittedPlan(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		if b, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", b, err)
		}
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	plan := "## Decisions log\n### First\nThe reason.\n### Second\nAnother reason.\n## Next\nignored\n"
	if err := os.WriteFile(filepath.Join(dir, "PLAN.md"), []byte(plan), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "PLAN.md")
	run("commit", "-m", "fixture")
	if err := os.WriteFile(filepath.Join(dir, "PLAN.md"), []byte("uncommitted"), 0600); err != nil {
		t.Fatal(err)
	}
	s := GitHubSource{roots: map[string]string{"owner/repo": dir}}
	cs, err := s.Candidates("recall")
	if err != nil || len(cs) != 2 || cs[0].Material != "First" || cs[0].Reveal != "The reason." {
		t.Fatalf("recall %+v %v", cs, err)
	}
}
func TestPracticeLedgerCapsAndRebuildIdempotency(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	f := fakePracticeSource{items: map[string][]Candidate{"recall": {{Source: "decision", Material: "heading", Reveal: "text"}}}}
	p := NewPractice(f, now)
	if _, err := p.Today(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Answer("answer"); err != nil {
		t.Fatal(err)
	}
	if err := p.Rebuild(); err != nil {
		t.Fatal(err)
	}
	events, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Key == "rep:2026-10-30" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("rep count %d", n)
	}
	for i := 0; i < 5; i++ {
		events = append(events, Event{Key: fmt.Sprintf("read:extra%d", i), Kind: kindRead, Track: TrackPractice, XP: 20, At: now})
	}
	events = append(events, Event{Key: "rep:extra", Kind: kindRep, Track: TrackPractice, XP: 30, At: now})
	if got := Sum(events).XP[TrackPractice]; got != 90 {
		t.Fatalf("capped xp %d", got)
	}
}

func TestPracticeDecisionListAndRecordValidation(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	cs := parseDecisions("owner/repo", "## Decisions log\n- 2026-10-26: Use the old design (#645; PR #657). Details.\n  More detail.\n- 2026-10-25: Another choice. Its reason.\n## Next\n")
	if len(cs) != 2 || cs[0].Material != "Use the old design" || !strings.Contains(cs[0].Reveal, "More detail.") || cs[1].Material != "Another choice" {
		t.Fatalf("decisions: %+v", cs)
	}
	dir := filepath.Join(Dir(), "reps")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-10-30.json"), []byte(`{"date":"../escape"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPractice(fakePracticeSource{}, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)).Today(); err == nil {
		t.Fatal("accepted mismatched date")
	}
}

func TestPracticeReadReferenceAndHistory(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	f := fakePracticeSource{items: map[string][]Candidate{"read": {{Source: "owner/repo#1"}, {Source: "owner/repo#2"}}}}
	p := NewPractice(f, now.AddDate(0, 0, -1))
	if _, err := p.Read("bad_owner/repo#1"); err == nil {
		t.Fatal("accepted invalid owner")
	}
	if _, err := p.Read("owner/repo#1"); err != nil {
		t.Fatal(err)
	}
	r, err := loadRep(p.date())
	if err != nil || r.Pending != r.Reads[0].Source {
		t.Fatalf("pending: %+v %v", r, err)
	}
	p = NewPractice(f, now)
	if _, err := p.Read("owner/repo#1"); err == nil {
		t.Fatal("accepted repeated read")
	}
	e, err := p.Today()
	if err != nil || e.Source != "owner/repo#2" {
		t.Fatalf("selection: %+v %v", e, err)
	}
}

func TestPracticeRevealSurvivesRebuildFailure(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	p := NewPractice(fakePracticeSource{items: map[string][]Candidate{"recall": {{Source: "x", Material: "x", Reveal: "revealed"}}}}, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
	if _, err := p.Today(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Dir(), "reps", "bad.json"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	reveal, err := p.Answer("answer")
	if reveal != "revealed" || err == nil {
		t.Fatalf("reveal %q warning %v", reveal, err)
	}
}

func TestPracticeReviewNeedsLaterDatedCommit(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	var p prData
	p.Commits = append(p.Commits, struct {
		OID           string    `json:"oid"`
		CommittedDate time.Time `json:"committedDate"`
	}{"reviewed", now.Add(-time.Hour)}, struct {
		OID           string    `json:"oid"`
		CommittedDate time.Time `json:"committedDate"`
	}{"later", now.Add(-time.Minute)})
	if reviewHasLaterCommit(p, "reviewed", now) {
		t.Fatal("accepted commit before review")
	}
	p.Commits[1].CommittedDate = now.Add(time.Minute)
	if !reviewHasLaterCommit(p, "reviewed", now) || reviewHasLaterCommit(p, "missing", now) || reviewHasLaterCommit(p, "reviewed", time.Time{}) {
		t.Fatal("incorrect review evidence")
	}
}

func TestPracticeMarksSteerSelectionAndGhFallback(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	f := fakePracticeSource{items: map[string][]Candidate{"recall": {{Source: "owner/repo#decision", Material: "decision"}}, "cold read": {{Source: "other/repo#1"}}}}
	old := repFile{Date: "2026-10-29", Daily: &Exercise{Candidate: Candidate{Kind: "recall", Source: "owner/repo#old"}, AnsweredAt: now.Add(-time.Hour), Mark: "missed"}}
	if err := saveRep(old.Date, old); err != nil {
		t.Fatal(err)
	}
	p := NewPractice(f, now)
	e, err := p.Today()
	if err != nil || e.Kind != "recall" {
		t.Fatalf("mark ignored: %+v %v", e, err)
	}
	t.Setenv("HRN_HOME", t.TempDir())
	f.fail = true
	e, err = NewPractice(f, now).Today()
	if err != nil || e.Kind != "recall" {
		t.Fatalf("fallback: %+v %v", e, err)
	}
}

func TestPracticeReadMergeTimeAndBadges(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	f := fakePracticeSource{mergedAt: now.Add(-time.Minute)}
	p := NewPractice(f, now)
	for i, kind := range []string{"cold read", "find the defect", "read"} {
		for n := 0; n < 10; n++ {
			date := now.AddDate(0, 0, -(i*10 + n)).Format("2006-01-02")
			e := Exercise{Candidate: Candidate{Kind: kind, Source: fmt.Sprintf("owner/repo#%d", i*10+n+1)}, AnsweredAt: now}
			if err := saveRep(date, repFile{Date: date, Daily: &e}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The tenth answer is the first one that earns each badge.
	for _, offset := range []int{9, 19, 29} {
		date := now.AddDate(0, 0, -offset).Format("2006-01-02")
		if err := os.Remove(repPath(date)); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Rebuild(); err != nil {
		t.Fatal(err)
	}
	before, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range before {
		if strings.HasPrefix(event.Key, "badge:") {
			t.Fatalf("early badge: %s", event.Key)
		}
	}
	for i, kind := range []string{"cold read", "find the defect", "read"} {
		date := now.AddDate(0, 0, -(i*10 + 9)).Format("2006-01-02")
		e := Exercise{Candidate: Candidate{Kind: kind, Source: fmt.Sprintf("owner/repo#%d", i*10+10)}, AnsweredAt: now}
		if err := saveRep(date, repFile{Date: date, Daily: &e}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Rebuild(); err != nil {
		t.Fatal(err)
	}
	events, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.Key] = true
		if strings.HasPrefix(e.Key, "read:") {
			t.Fatal("counted a merge before the read")
		}
	}
	for _, key := range []string{"badge:cold-reader", "badge:defect-hunter", "badge:fresh-eyes"} {
		if !seen[key] {
			t.Errorf("missing %s", key)
		}
	}
	f.mergedAt = now.Add(time.Minute)
	if err := NewPractice(f, now).Rebuild(); err != nil {
		t.Fatal(err)
	}
	events, _, _ = Load()
	found := false
	for _, e := range events {
		if strings.HasPrefix(e.Key, "read:") {
			found = true
		}
	}
	if !found {
		t.Fatal("merge after read not counted")
	}
}

func TestPracticeFilePermissions(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows does not expose Unix permissions")
	}
	t.Setenv("HRN_HOME", t.TempDir())
	date := "2026-10-30"
	if err := saveRep(date, repFile{Date: date}); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{filepath.Join(Dir(), "reps"): 0700, repPath(date): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s: %v %v", path, info, err)
		}
	}
}
