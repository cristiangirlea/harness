package quota

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// A codex token_count line as codex 0.5x writes it, trimmed to what matters;
// limit is the limit_id, windows the primary and secondary objects.
func codexLine(at time.Time, limit, primary, secondary string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":5}},`+
		`"rate_limits":{"limit_id":%q,"limit_name":null,"primary":%s,"secondary":%s,"credits":{"has_credits":false},"plan_type":"prolite"}}}`,
		at.UTC().Format(time.RFC3339Nano), limit, primary, secondary)
}

func window(used float64, minutes int, resets time.Time) string {
	return fmt.Sprintf(`{"used_percent":%v,"window_minutes":%d,"resets_at":%d}`, used, minutes, resets.Unix())
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sameWindow(t *testing.T, name string, got *Window, left float64, resets, reported time.Time) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: no window, want %v%% left", name, left)
	}
	if got.Left != left || !got.ResetsAt.Equal(resets.Truncate(time.Second)) || !got.ReportedAt.Equal(reported) {
		t.Fatalf("%s = %v%% left, resets %v, reported %v; want %v%%, %v, %v", name, got.Left, got.ResetsAt, got.ReportedAt, left, resets.Truncate(time.Second), reported)
	}
}

func TestCodexTailSkipsAPartialFirstRecordAndMalformedLines(t *testing.T) {
	at := time.Date(2026, 10, 15, 17, 46, 33, 0, time.UTC)
	week := at.Add(48 * time.Hour)
	whole := codexLine(at, "codex", window(24, 10080, week), "null")
	// The tail starts inside a record: its first line is dropped even when
	// what is left of it would read as a whole quota record.
	partial := codexLine(at.Add(time.Hour), "codex", window(99, 300, week), "null")
	tail := partial + "\n" + `{"timestamp":` + "\n" + "not json at all\n" +
		`{"timestamp":"2026-10-15T18:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":"lots"}}}}` + "\n" +
		whole + "\n" + `{"timestamp":"2026-10-15T19:00:00Z","type":"response_item","payload":{"type":"message","content":"rate_limits primary"}}` + "\n"
	q := parseCodexTail([]byte(tail), true)
	if q.FiveHour != nil {
		t.Fatalf("five hour = %+v, want none: the partial record is not read", q.FiveHour)
	}
	sameWindow(t, "week", q.Week, 76, week, at)

	// Read from the file's start, the first line is a whole record.
	q = parseCodexTail([]byte(whole+"\n"), false)
	sameWindow(t, "week from the start", q.Week, 76, week, at)
}

func TestCodexWindowsAreKnownByTheirLengthNotTheirSlot(t *testing.T) {
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	five, week := at.Add(90*time.Minute), at.Add(76*time.Hour)
	line := codexLine(at, "codex", window(84, 10080, week), window(38.5, 300, five)) + "\n" +
		codexLine(at, "codex_other_model", window(1, 300, five), "null") + "\n" +
		codexLine(at, "", window(2, 60, five), "null") + "\n"
	q := parseCodexTail([]byte(line), false)
	sameWindow(t, "five hour", q.FiveHour, 61.5, five, at)
	sameWindow(t, "week", q.Week, 16, week, at)
}

func TestCodexReadsTheNewestSessionFilesTailsOnly(t *testing.T) {
	root := t.TempDir()
	tailBytes = 4096
	t.Cleanup(func() { tailBytes = defaultTailBytes })
	old := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	week := now.Add(72 * time.Hour)
	// An older day's session still written to today: its newest record wins.
	writeFile(t, filepath.Join(root, "2026", "10", "14", "rollout-2026-10-14T09-00-00-a.jsonl"),
		codexLine(old, "codex", window(10, 10080, week), "null")+"\n"+codexLine(now.Add(-time.Minute), "codex", window(30, 10080, week), "null")+"\n")
	// Today's file: an early five-hour report, then a long record that pushes
	// it out of the tail, then a weekly one.
	five := now.Add(2 * time.Hour)
	writeFile(t, filepath.Join(root, "2026", "10", "15", "rollout-2026-10-15T10-00-00-b.jsonl"),
		codexLine(now.Add(-3*time.Hour), "codex", window(50, 300, five), "null")+"\n"+
			`{"type":"response_item","payload":{"content":"`+strings.Repeat("x", 3*int(tailBytes))+`"}}`+"\n"+
			codexLine(now.Add(-2*time.Hour), "codex", window(20, 10080, week), "null")+"\n")
	writeFile(t, filepath.Join(root, "2026", "10", "15", "notes.txt"), codexLine(now, "codex", window(99, 300, five), "null"))
	writeFile(t, filepath.Join(root, "not-a-year", "x.jsonl"), codexLine(now, "codex", window(99, 300, five), "null"))

	q := codexFrom(root)
	if q.FiveHour != nil {
		t.Fatalf("five hour = %+v, want none: only tails and .jsonl files under dated folders are read", q.FiveHour)
	}
	sameWindow(t, "week", q.Week, 70, week, now.Add(-time.Minute))

	if q := codexFrom(filepath.Join(root, "missing")); q.FiveHour != nil || q.Week != nil {
		t.Fatalf("missing folder = %+v, want nothing known", q)
	}
}

func TestCodexStopsAfterTheNewestFiles(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	week := now.Add(72 * time.Hour)
	// The only report is in the oldest of more files than are read.
	writeFile(t, filepath.Join(root, "2026", "01", "01", "rollout-old.jsonl"), codexLine(now, "codex", window(5, 10080, week), "null")+"\n")
	for i := range maxCodexFiles {
		writeFile(t, filepath.Join(root, "2026", "10", "15", fmt.Sprintf("rollout-%03d.jsonl", i)), "{}\n")
	}
	if q := codexFrom(root); q.Week != nil {
		t.Fatalf("week = %+v, want none: only the %d newest files are read", q.Week, maxCodexFiles)
	}
}

func writeRun(t *testing.T, id, backend string, events ...agent.Event) {
	t.Helper()
	dir := filepath.Join(runlog.Root(), id)
	writeFile(t, filepath.Join(dir, "task.json"), fmt.Sprintf(`{"id":%q,"backend":%q,"task":{"prompt":"secret prompt"}}`, id, backend))
	var b strings.Builder
	b.WriteString(`{"t":"2026-10-15T10:00:00Z","kind":"text","text":"hello"}` + "\n")
	for _, e := range events {
		fmt.Fprintf(&b, `{"t":%q,"kind":%q,"quota":%s}`+"\n", e.Time.Format(time.RFC3339Nano), e.Kind, quotaJSON(e.Quota))
	}
	writeFile(t, filepath.Join(dir, "events.jsonl"), b.String())
}

func quotaJSON(q *agent.Quota) string {
	w := func(x *agent.QuotaWindow) string {
		if x == nil {
			return "null"
		}
		return fmt.Sprintf(`{"used_percent":%v,"resets_at":%d}`, x.UsedPercent, x.ResetsAt)
	}
	return fmt.Sprintf(`{"five_hour":%s,"week":%s}`, w(q.FiveHour), w(q.Week))
}

func TestClaudeTakesEachWindowFromTheNewestRunThatRecordedIt(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	five, week := now.Add(time.Hour), now.Add(80*time.Hour)
	q := func(fiveUsed, weekUsed float64) *agent.Quota {
		out := &agent.Quota{}
		if fiveUsed >= 0 {
			out.FiveHour = &agent.QuotaWindow{UsedPercent: fiveUsed, ResetsAt: five.Unix()}
		}
		if weekUsed >= 0 {
			out.Week = &agent.QuotaWindow{UsedPercent: weekUsed, ResetsAt: week.Unix()}
		}
		return out
	}
	writeRun(t, "20261015-090000-aaaaaa", "claude", agent.Event{Time: now.Add(-9 * time.Hour), Kind: agent.EventQuota, Quota: q(1, 1)})
	writeRun(t, "20261015-100000-bbbbbb", "claude", agent.Event{Time: now.Add(-8 * time.Hour), Kind: agent.EventQuota, Quota: q(-1, 20)})
	writeRun(t, "20261015-110000-cccccc", "claude",
		agent.Event{Time: now.Add(-3 * time.Hour), Kind: agent.EventQuota, Quota: q(10, -1)},
		agent.Event{Time: now.Add(-2 * time.Hour), Kind: agent.EventQuota, Quota: q(38, -1)})
	// A codex run's quota is not claude's, and a run with nothing is passed over.
	writeRun(t, "20261015-120000-dddddd", "codex", agent.Event{Time: now.Add(-time.Hour), Kind: agent.EventQuota, Quota: q(99, 99)})
	writeRun(t, "20261015-130000-eeeeee", "claude")
	writeFile(t, filepath.Join(runlog.Root(), "20261015-140000-ffffff", "events.jsonl"), "{broken\n")

	got := claudeFrom(runlog.Root())
	sameWindow(t, "five hour", got.FiveHour, 62, five, now.Add(-2*time.Hour))
	sameWindow(t, "week", got.Week, 80, week, now.Add(-8*time.Hour))

	if got := claudeFrom(filepath.Join(t.TempDir(), "none")); got.FiveHour != nil || got.Week != nil {
		t.Fatalf("no runs = %+v, want nothing known", got)
	}
}

func TestDescribeSaysWhatIsLeftAndMarksLowAndStaleWindows(t *testing.T) {
	now := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	q := Quota{
		FiveHour: &Window{Left: 62.4, ResetsAt: now.Add(72 * time.Minute), ReportedAt: now.Add(-time.Minute)},
		Week:     &Window{Left: 80, ResetsAt: now.Add(76*time.Hour + 5*time.Minute), ReportedAt: now.Add(-time.Minute)},
	}
	if got, want := q.String(now), "5h: 62% left, resets in 1h 12m / week: 80% left, resets in 3d 4h"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
	parts := q.Parts(now)
	if len(parts) != 2 || parts[0].Low || parts[0].Stale || parts[1].Low || parts[1].Stale {
		t.Fatalf("parts = %+v, want two plain windows", parts)
	}

	q.FiveHour = &Window{Left: 9.6, ResetsAt: now.Add(30 * time.Second), ReportedAt: now}
	q.Week = &Window{Left: 3, ResetsAt: now.Add(-10 * time.Minute), ReportedAt: now.Add(-time.Hour)}
	parts = q.Parts(now)
	if len(parts) != 2 || !parts[0].Low || parts[0].Stale || parts[0].Text != "5h: 10% left, resets in under a minute" {
		t.Fatalf("low part = %+v, want low, not stale", parts[0])
	}
	if parts[1].Low || !parts[1].Stale || parts[1].Text != "week: 3% left before its reset 10m ago (stale)" {
		t.Fatalf("stale part = %+v, want stale and no longer low", parts[1])
	}

	if got := (Quota{}).String(now); got != "" {
		t.Fatalf("nothing known = %q, want empty", got)
	}
	if got := (Quota{Week: q.Week}).String(now); !strings.HasPrefix(got, "week:") {
		t.Fatalf("week only = %q, want only the week", got)
	}
}

func TestForKnowsOnlyThePlanBackendsAndCachesTheAnswer(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	now := time.Now()
	week := now.Add(48 * time.Hour)
	path := filepath.Join(codexHome, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"), "rollout-x.jsonl")
	writeFile(t, path, codexLine(now, "codex", window(40, 10080, week), "null")+"\n")
	if q := For("codex", now); q.Week == nil || q.Week.Left != 60 {
		t.Fatalf("codex = %+v, want 60%% left this week", q.Week)
	}
	writeFile(t, path, codexLine(now, "codex", window(90, 10080, week), "null")+"\n")
	if q := For("codex", now.Add(10*time.Second)); q.Week == nil || q.Week.Left != 60 {
		t.Fatalf("codex within 30s = %+v, want the cached 60%%", q.Week)
	}
	if q := For("codex", now.Add(31*time.Second)); q.Week == nil || q.Week.Left != 10 {
		t.Fatalf("codex after 30s = %+v, want the new 10%%", q.Week)
	}
	for _, name := range []string{"api", "agy", "gemini", "opencode", "nope"} {
		if q := For(name, now); q.FiveHour != nil || q.Week != nil {
			t.Fatalf("%s = %+v, want nothing", name, q)
		}
	}
}

func TestLimitSaysWhenARememberedLimitLiftsInTheWindowsWords(t *testing.T) {
	now := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	if got := Limit(now.Add(2*time.Hour+5*time.Minute), now); got.Text != "at its limit, resets in 2h 5m" || !got.Low || got.Stale {
		t.Fatalf("Limit = %+v", got)
	}
	for _, until := range []time.Time{{}, now, now.Add(-time.Minute)} {
		if got := Limit(until, now); got != (Part{}) {
			t.Fatalf("Limit(%v) = %+v, want nothing", until, got)
		}
	}
}

func TestClaudeKeepsTheLatestReportWhateverRunStartedLast(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	five, week := now.Add(time.Hour), now.Add(80*time.Hour)
	used := func(p float64) *agent.Quota {
		return &agent.Quota{FiveHour: &agent.QuotaWindow{UsedPercent: p, ResetsAt: five.Unix()}, Week: &agent.QuotaWindow{UsedPercent: p, ResetsAt: week.Unix()}}
	}
	// A started first but reported last; B started later and reported early.
	writeRun(t, "20261015-100000-aaaaaa", "claude", agent.Event{Time: now, Kind: agent.EventQuota, Quota: used(80)})
	writeRun(t, "20261015-110000-bbbbbb", "claude", agent.Event{Time: now.Add(-55 * time.Minute), Kind: agent.EventQuota, Quota: used(30)})
	got := claudeFrom(runlog.Root())
	sameWindow(t, "five hour", got.FiveHour, 20, five, now)
	sameWindow(t, "week", got.Week, 20, week, now)
}

func TestClaudeReadsOnlyTheNewestRuns(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	week := &agent.Quota{Week: &agent.QuotaWindow{UsedPercent: 5, ResetsAt: now.Add(time.Hour).Unix()}}
	writeRun(t, "20261014-000000-aaaaaa", "claude", agent.Event{Time: now, Kind: agent.EventQuota, Quota: week})
	for i := range maxClaudeRuns {
		writeRun(t, fmt.Sprintf("20261015-%06d-codexx", i), "codex")
	}
	if q := claudeFrom(runlog.Root()); q.Week != nil {
		t.Fatalf("week = %+v, want none: only the %d newest runs are read", q.Week, maxClaudeRuns)
	}
}

func TestClaudeReadsAnEventsTailThatStartsInsideALine(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	week := now.Add(time.Hour)
	quota := func(at time.Time, used float64) string {
		return fmt.Sprintf(`{"t":%q,"kind":"quota","quota":{"week":{"used_percent":%v,"resets_at":%d}}}`, at.Format(time.RFC3339Nano), used, week.Unix())
	}
	// The tail starts at the space inside a long line, whose rest would read
	// as a newer whole report if the partial line were not dropped.
	cut, last := quota(now.Add(time.Minute), 99), quota(now, 40)+"\n"
	tailBytes = int64(len(cut) + 1 + len(last))
	t.Cleanup(func() { tailBytes = defaultTailBytes })
	long := `{"t":"2026-10-15T10:00:00Z","kind":"text","text":"` + strings.Repeat("x", 3*int(tailBytes)) + " " + cut
	dir := filepath.Join(runlog.Root(), "20261015-100000-aaaaaa")
	writeFile(t, filepath.Join(dir, "task.json"), `{"backend":"claude"}`)
	writeFile(t, filepath.Join(dir, "events.jsonl"), quota(now.Add(-2*time.Hour), 10)+"\n"+long+"\n"+last)
	sameWindow(t, "week", claudeFrom(runlog.Root()).Week, 60, week, now)
}

func TestNonRegularFilesAreNotOpened(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	week := now.Add(72 * time.Hour)
	day := filepath.Join(root, "2026", "10", "15")
	target := filepath.Join(t.TempDir(), "target.jsonl")
	writeFile(t, target, codexLine(now, "codex", window(90, 10080, week), "null")+"\n")
	if err := os.MkdirAll(filepath.Join(day, "rollout-dir.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(day, "rollout-link.jsonl")); err != nil {
		t.Logf("no symlink here: %v", err)
	}
	if q := codexFrom(root); q.Week != nil {
		t.Fatalf("week = %+v, want none: a folder or link named .jsonl is not read", q.Week)
	}
	if _, _, err := tail(filepath.Join(day, "rollout-dir.jsonl")); err == nil {
		t.Fatal("tail opened a folder")
	}
	if _, err := os.Lstat(filepath.Join(day, "rollout-link.jsonl")); err == nil {
		if _, _, err := tail(filepath.Join(day, "rollout-link.jsonl")); err == nil {
			t.Fatal("tail followed a link")
		}
	}

	t.Setenv("HRN_HOME", t.TempDir())
	run := filepath.Join(runlog.Root(), "20261015-100000-aaaaaa")
	if err := os.MkdirAll(filepath.Join(run, "task.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if claudeRun(run) {
		t.Fatal("a folder named task.json reads as a claude run")
	}
}

func TestAFolderIsListedOnlyUpToItsEntryBudget(t *testing.T) {
	dir := t.TempDir()
	for i := range 7 {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%d", i)), "")
	}
	entryBudget = 3
	t.Cleanup(func() { entryBudget = maxEntries })
	if got := readDir(dir); len(got) != 3 {
		t.Fatalf("listed %d entries, want the budget of 3", len(got))
	}
	entryBudget = maxEntries
	got := readDir(dir)
	if len(got) != 7 || got[0].Name() != "f0" || got[6].Name() != "f6" {
		t.Fatalf("listed %d entries, want all 7 by name", len(got))
	}
}

func TestARefreshInProgressAnswersWithWhatIsKnown(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now()
	key := "claude\x00" + runlog.Root()
	known := Quota{Week: &Window{Left: 55}}
	mu.Lock()
	cache[key] = cached{at: now.Add(-time.Hour), q: known, refreshing: true}
	mu.Unlock()
	t.Cleanup(func() { mu.Lock(); delete(cache, key); mu.Unlock() })
	if q := For("claude", now); q.Week == nil || q.Week.Left != 55 {
		t.Fatalf("during a refresh = %+v, want the known 55%%", q.Week)
	}
}
