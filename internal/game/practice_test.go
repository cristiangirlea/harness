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
	items  map[string][]Candidate
	merged bool
}

func (f fakePracticeSource) Candidates(kind string) ([]Candidate, error) { return f.items[kind], nil }
func (f fakePracticeSource) Read(ref string) (Candidate, error) {
	return Candidate{Kind: "read", Source: ref, Material: "diff", Reveal: "secret reveal"}, nil
}
func (f fakePracticeSource) Merged(ref string, after time.Time) (bool, error) { return f.merged, nil }

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
	if err = saveRep(r); err != nil {
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
