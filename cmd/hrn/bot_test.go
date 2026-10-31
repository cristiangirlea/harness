package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/game"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/watch"
)

// The bot opens in an app window, which has no address bar or tabs, where a
// browser that has one is installed, and in the default browser otherwise.
func TestTheBotOpensInAnAppWindowWhereABrowserHasOne(t *testing.T) {
	const url = "http://127.0.0.1:8089/bot"
	env := map[string]string{"ProgramFiles(x86)": `C:\Program Files (x86)`, "ProgramFiles": `C:\Program Files`, "LOCALAPPDATA": `C:\Users\me\AppData\Local`}
	getenv := func(k string) string { return env[k] }
	only := func(paths ...string) func(string) bool {
		return func(p string) bool {
			for _, q := range paths {
				if p == q {
					return true
				}
			}
			return false
		}
	}
	noPath := func(string) (string, error) { return "", errors.New("not found") }
	for _, c := range []struct {
		name   string
		goos   string
		exists func(string) bool
		look   func(string) (string, error)
		want   string
	}{
		{"Edge on Windows", "windows", only(`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`), noPath,
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe --app=` + url + " " + botWindow},
		{"Chrome for one user on Windows", "windows", only(`C:\Users\me\AppData\Local\Google\Chrome\Application\chrome.exe`), noPath,
			`C:\Users\me\AppData\Local\Google\Chrome\Application\chrome.exe --app=` + url + " " + botWindow},
		{"neither on Windows", "windows", only(), noPath, "rundll32 url.dll,FileProtocolHandler " + url},
		{"Chrome on a Mac", "darwin", only("/Applications/Google Chrome.app"), noPath, "open -na Google Chrome --args --app=" + url + " " + botWindow},
		{"neither on a Mac", "darwin", only(), noPath, "open " + url},
		{"Chromium on Linux", "linux", only(), func(n string) (string, error) {
			if n == "chromium" {
				return "/usr/bin/chromium", nil
			}
			return "", errors.New("not found")
		}, "/usr/bin/chromium --app=" + url + " " + botWindow},
		{"neither on Linux", "linux", only(), noPath, "xdg-open " + url},
	} {
		if got := strings.Join(windowCommand(c.goos, getenv, c.exists, c.look, url), " "); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// An unset variable never makes a path out of the rest alone.
	asked := []string{}
	windowCommand("windows", func(string) string { return "" }, func(p string) bool { asked = append(asked, p); return false }, noPath, url)
	if len(asked) != 0 {
		t.Errorf("with no ProgramFiles it looked at %q", asked)
	}
}

// hrn bot opens the bot of an hrn watch that already serves the address, and
// leaves anything else that holds the port alone.
func TestHrnBotKnowsAnHrnWatchWithABot(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	srv := httptest.NewServer(watch.New(t.Context(), watch.Options{}))
	defer srv.Close()
	if !servesBot(srv.URL + "/bot") {
		t.Error("an hrn watch with a bot was not recognised")
	}
	other := httptest.NewServer(http.NotFoundHandler())
	defer other.Close()
	if servesBot(other.URL + "/bot") {
		t.Error("something else on the port was taken for hrn watch")
	}
}

func TestHrnBotServesOnLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8089", "192.168.1.10:8089"} {
		if err := cmdBot([]string{"--addr", addr}); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("hrn bot --addr %s: %v; want it refused", addr, err)
		}
	}
	if err := cmdBot([]string{"extra"}); err == nil {
		t.Error("an argument was accepted")
	}
}

func TestBotReportsPublicWatchOnOccupiedPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	want := addr + " answered 403: another program holds the port, possibly an hrn watch --public (which refuses this computer's browser); start the bot on another port with --addr"
	if err := cmdBot([]string{"--addr", addr}); err == nil || err.Error() != want {
		t.Fatalf("hrn bot on occupied port = %v; want %q", err, want)
	}
}

func TestBotDoesNotIdentifyAnUnrelatedForbiddenServerAsPublicWatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unrelated service", http.StatusForbidden)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	want := addr + " answered 403: another program holds the port, possibly an hrn watch --public (which refuses this computer's browser); start the bot on another port with --addr"
	if err := cmdBot([]string{"--addr", addr}); err == nil || err.Error() != want {
		t.Fatalf("hrn bot on unrelated service's port = %v; want %q", err, want)
	}
}

func TestBotStatsPrintsTheLevelThisWeeksExperienceAndTheBadges(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now().UTC()
	if _, err := game.Append([]game.Event{
		{Key: "test:old", Kind: "test", Track: game.TrackDelivery, XP: 200, At: now.AddDate(0, 0, -30), Repo: "example/app"},
		{Key: "review:r1", Kind: "review_clean", Track: game.TrackDelivery, XP: 20, At: now, Repo: "example/app"},
		{Key: "rep:today", Kind: "daily_practice", Track: game.TrackPractice, XP: 30, At: now},
		{Key: "badge:budget-keeper", Kind: "badge", Track: game.TrackBadge, At: now.AddDate(0, 0, -2)},
	}); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdBot([]string{"stats"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"level 2, 250 xp", "200 more for level 3", "this week", "delivery 20 xp", "practice 30 xp", "badges: Budget keeper ("} {
		if !strings.Contains(string(out), want) {
			t.Errorf("hrn bot stats lacks %q:\n%s", want, out)
		}
	}
}

func TestBotStatsSaysNoBadgesYetOnAnEmptyLedger(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	out, err := captureStdout(t, func() error { return cmdBot([]string{"stats"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"level 0, 0 xp", "50 more for level 1", "badges: none yet"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("hrn bot stats lacks %q:\n%s", want, out)
		}
	}
}

func TestBotStatsShowsNothingWhenTheGameIsOffButTheLedgerStillRecords(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	if err := os.MkdirAll(game.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(game.Dir(), "settings.json"), []byte(`{"enabled": false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	id := "20261026-100000-aaaaaa"
	writeJSONFile(t, filepath.Join(runlog.Root(), id, "result.json"), runlog.Record{
		ID: id, Backend: "claude", Started: time.Now().Add(-time.Hour),
		Task:   agent.Task{Prompt: "review the branch", Purpose: "review", Dir: home},
		Result: agent.Result{Backend: "claude", Status: agent.StatusOK, Final: "No defects."},
	})
	out, err := captureStdout(t, func() error { return cmdBot([]string{"stats"}) })
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); strings.Contains(s, "level") || strings.Contains(s, "xp") || !strings.Contains(s, "off") {
		t.Fatalf("with the game off, hrn bot stats printed %q; want only a line saying it is off", s)
	}
	events, _, err := game.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Key != "review:"+id {
		t.Fatalf("ledger %+v, want the review recorded while the game is off", events)
	}
}

func TestBotRefusesArgumentsOtherThanStats(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, args := range [][]string{{"stat"}, {"stats", "extra"}} {
		if err := cmdBot(args); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
			t.Errorf("hrn bot %v: err = %v, want an unexpected argument", args, err)
		}
	}
}
func TestBotPracticeNeverPrintsRevealBeforeAnswer(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := filepath.Join(game.Dir(), "reps")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	date := time.Now().UTC().Format("2006-01-02")
	record := `{"date":"` + date + `","daily":{"kind":"recall","source":"decision","material":"heading","reveal":"private reveal","attempt":0},"pending":"daily"}`
	if err := os.WriteFile(filepath.Join(dir, date+".json"), []byte(record), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdBot([]string{"practice"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "private reveal") || !strings.Contains(string(out), "heading") {
		t.Fatalf("output %s", out)
	}
}

func TestBotPracticeAnsweredRevealAndDisabledGame(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := filepath.Join(game.Dir(), "reps")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	date := time.Now().UTC().Format("2006-01-02")
	record := `{"date":"` + date + `","daily":{"kind":"recall","source":"decision","material":"heading","reveal":"saved reveal","answered_at":"2026-01-01T00:00:00Z"},"pending":"daily"}`
	if err := os.WriteFile(filepath.Join(dir, date+".json"), []byte(record), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(game.Dir(), "settings.json"), []byte(`{"enabled":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdBot([]string{"practice"}) })
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); !strings.Contains(s, "saved reveal") || strings.Contains(s, "level") || strings.Contains(s, "streak") || strings.Contains(s, "xp") {
		t.Fatalf("output %q", s)
	}
}

func TestBotStatsStreakSquares(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := filepath.Join(game.Dir(), "reps")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	date := time.Now().UTC().Format("2006-01-02")
	record := `{"date":"` + date + `","daily":{"answered_at":"2026-01-01T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(dir, date+".json"), []byte(record), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdBot([]string{"stats"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "streak: 1 days "+strings.Repeat("\u25a1", 34)+"\u25a0") {
		t.Fatalf("output %s", out)
	}
}

func TestBotStatsWarnsForUnreadableOldFamiliarityRecord(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := filepath.Join(game.Dir(), "reps")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2000-01-01.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = stderr
	defer func() { os.Stderr = old; stderr.Close() }()
	out, err := captureStdout(t, func() error { return cmdBot([]string{"stats"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "level ") || !strings.Contains(string(out), "badges:") || !strings.Contains(string(out), "areas you know:") {
		t.Fatalf("output: %s", out)
	}
	if _, err := stderr.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stderr.Name())
	if err != nil || !strings.Contains(string(data), "practice records:") {
		t.Fatalf("warning: %q, %v", data, err)
	}
}

func TestBotStatsJSON(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now().UTC()
	if _, err := game.Append([]game.Event{
		{Key: "review:r1", Kind: "review_clean", Track: game.TrackDelivery, XP: 20, At: now},
		{Key: "rep:today", Kind: "daily_practice", Track: game.TrackPractice, XP: 30, At: now},
		{Key: "badge:budget-keeper", Kind: "badge", Track: game.TrackBadge, At: now},
	}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(game.Dir(), "reps")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	date := now.Format("2006-01-02")
	path := filepath.Join(dir, date+".json")
	write := func(answered string) {
		t.Helper()
		record := `{"date":"` + date + `","daily":{"kind":"recall","source":"decision","material":"masked heading","reveal":"secret reveal","answer":"secret answer"` + answered + `}}`
		if err := os.WriteFile(path, []byte(record), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var stderr string
	run := func() ([]byte, error) {
		t.Helper()
		f, err := os.CreateTemp(t.TempDir(), "stderr")
		if err != nil {
			t.Fatal(err)
		}
		old := os.Stderr
		os.Stderr = f
		defer func() { os.Stderr = old; f.Close() }()
		out, runErr := captureStdout(t, func() error { return cmdBot([]string{"stats", "--json"}) })
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(f)
		if err != nil {
			t.Fatal(err)
		}
		stderr = string(b)
		return out, runErr
	}
	read := func() map[string]any {
		t.Helper()
		out, err := run()
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			t.Fatal(err)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			t.Fatalf("extra output: %s", out)
		}
		if bytes.Contains(out, []byte("secret")) {
			t.Fatalf("secret in output: %s", out)
		}
		return obj
	}
	write("")
	obj := read()
	if obj["version"] != float64(1) || obj["enabled"] != true || obj["level"] != float64(1) || obj["xp"] != float64(50) || obj["week_start"] != game.WeekStart(now).Format("2006-01-02") {
		t.Fatalf("stats: %#v", obj)
	}
	week := obj["week"].(map[string]any)
	if week["delivery"] != float64(20) || week["practice"] != float64(30) {
		t.Fatal(week)
	}
	if obj["streak"] != float64(0) || len(obj["days"].([]any)) != 35 || obj["badges"].([]any)[0] != "Budget keeper" {
		t.Fatal(obj)
	}
	ex := obj["exercise"].(map[string]any)
	if ex["kind"] != "recall" || ex["source"] != "decision" || ex["material"] != "masked heading" || ex["answered"] != false {
		t.Fatal(ex)
	}
	fam := obj["familiarity"].(map[string]any)
	if len(fam["known"].([]any)) != 0 || len(fam["unseen"].([]any)) != 0 {
		t.Fatal(fam)
	}
	write(`,"answered_at":"2026-01-01T00:00:00Z"`)
	obj = read()
	ex = obj["exercise"].(map[string]any)
	if ex["answered"] != true || ex["material"] != nil || obj["streak"] != float64(1) || obj["days"].([]any)[34] != true {
		t.Fatal(obj)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	obj = read()
	if obj["exercise"] != nil {
		t.Fatal(obj)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("exercise created: %v", err)
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	obj = read()
	if obj["exercise"] != nil || obj["streak"] != nil || obj["days"] != nil || obj["familiarity"] == nil {
		t.Fatal(obj)
	}
	for _, warning := range []string{"streak:", "exercise:", "familiarity:"} {
		if !strings.Contains(stderr, warning) {
			t.Fatalf("missing %s warning: %s", warning, stderr)
		}
	}
	if err := os.Rename(path, filepath.Join(dir, "2000-01-01.json")); err != nil {
		t.Fatal(err)
	}
	write("")
	obj = read()
	if obj["streak"] == nil || obj["exercise"] == nil || obj["familiarity"] == nil {
		t.Fatal(obj)
	}
	if err := os.WriteFile(filepath.Join(game.Dir(), "settings.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := run()
	if err == nil || len(out) != 0 {
		t.Fatalf("unreadable settings: stdout %q, error %v", out, err)
	}
	if err := os.WriteFile(filepath.Join(game.Dir(), "settings.json"), []byte(`{"enabled":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	obj = read()
	if len(obj) != 2 || obj["version"] != float64(1) || obj["enabled"] != false {
		t.Fatal(obj)
	}
}
