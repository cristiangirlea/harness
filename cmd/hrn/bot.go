package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/cristiangirlea/harness/internal/game"
	"github.com/cristiangirlea/harness/internal/watch"
)

func cmdBot(args []string) error {
	if len(args) > 0 && args[0] == "practice" {
		return cmdBotPractice(args[1:])
	}
	if len(args) > 0 && args[0] == "stats" {
		return cmdBotStats(args[1:])
	}
	fs := flag.NewFlagSet("bot", flag.ContinueOnError)
	addr := fs.String("addr", watch.DefaultAddr, "")
	voice := fs.String("voice", "", "")
	var voiceArgs argListFlag
	fs.Var(&voiceArgs, "voice-arg", "")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errors.New("invalid arguments")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q; the address is set with --addr", fs.Arg(0))
	}
	speech, err := voiceProgram(fs, *voice, voiceArgs)
	if err != nil {
		return err
	}
	bind, err := watch.LoopbackAddr(*addr)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		url := "http://" + bind + "/bot"
		serves, status := probeBot(url)
		if status == http.StatusForbidden {
			return fmt.Errorf("%s answered 403: another program holds the port, possibly an hrn watch --public (which refuses this computer's browser); start the bot on another port with --addr", bind)
		}
		if !serves {
			return err
		}
		fmt.Printf("hrn bot: hrn watch already serves http://%s; opening its bot\n", bind)
		if speech != nil {
			fmt.Fprintln(os.Stderr, "hrn bot: --voice is not used; the running hrn watch speaks with its own settings")
		}
		return openWindow(url)
	}
	url := "http://" + ln.Addr().String() + "/bot"
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	fmt.Printf("hrn bot: %s (this computer only; Ctrl+C stops it)\n", url)
	if err := openWindow(url); err != nil {
		fmt.Fprintf(os.Stderr, "hrn bot: no window opened (%v); open %s in a browser\n", err, url)
	}
	return watch.Serve(ctx, watch.Options{Voice: speech}, ln)
}

func cmdBotStats(args []string) error {
	jsonOut, repo, err := botStatsArgs(args)
	if err != nil {
		return err
	}
	if jsonOut {
		return cmdBotStatsJSON(repo)
	}
	records, err := game.LoadRecords()
	if err != nil {
		return err
	}
	source := game.NewGitHubSourceFrom(records)
	if err := game.RebuildFrom(records, source); err != nil {
		var recordErr *game.PracticeRecordError
		if !errors.As(err, &recordErr) {
			return err
		}
		fmt.Fprintf(os.Stderr, "hrn bot stats: practice records: %v\n", err)
	}
	settings, err := game.LoadSettings()
	if err != nil {
		return err
	}
	if !settings.On() {
		fmt.Printf("hrn bot stats: the game is off (enabled is false in %s); the ledger keeps recording\n", filepath.Join(game.Dir(), "settings.json"))
		return nil
	}
	events, unreadable, err := game.Load()
	if err != nil {
		return err
	}
	if unreadable > 0 {
		fmt.Fprintf(os.Stderr, "hrn bot stats: %d ledger lines could not be read and were left out\n", unreadable)
	}
	totals := game.Sum(events)
	level := game.Level(totals.Person)
	fmt.Printf("level %d, %d xp (%d more for level %d)\n", level, totals.Person, 50*(level+1)*(level+1)-totals.Person, level+1)
	streak, days, err := game.NewPractice(source, time.Now()).Streak()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hrn bot stats: streak: %v\n", err)
	} else {
		var squares strings.Builder
		for _, day := range days {
			if day {
				squares.WriteString("\u25a0")
			} else {
				squares.WriteString("\u25a1")
			}
		}
		fmt.Printf("streak: %d days %s\n", streak, squares.String())
	}
	week := game.WeekStart(time.Now())
	xp := totals.Since(week)
	fmt.Printf("this week, from %s UTC: delivery %d xp, practice %d xp\n", week.Format("Monday 2 January 2006"), xp[game.TrackDelivery], xp[game.TrackPractice])
	fmt.Printf("Ada: level %d, %d xp this week\n", totals.AdaLevel, xp[game.TrackAda])
	var badges []string
	for _, badge := range totals.Badges {
		badges = append(badges, game.BadgeName(badge.Key)+" ("+badge.At.UTC().Format("2 January 2006")+")")
	}
	if len(badges) == 0 {
		badges = append(badges, "none yet")
	}
	fmt.Printf("badges: %s\n", strings.Join(badges, ", "))
	familiarity, err := game.ReadFamiliarityFrom(context.Background(), records, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "hrn bot stats: familiarity: %v\n", err)
		return nil
	}
	printFamiliarity(os.Stdout, familiarity)
	return nil
}

func printFamiliarity(w io.Writer, f game.Familiarity) {
	showAreas := func(label string, areas []game.AreaExposure) {
		var names []string
		for _, area := range areas {
			name := area.Repo + "/" + area.Area
			if area.Count > 0 {
				name += fmt.Sprintf(" (%d)", area.Count)
			}
			names = append(names, name)
		}
		if len(names) == 0 {
			names = append(names, "none yet")
		}
		fmt.Fprintf(w, "%s: %s\n", label, strings.Join(names, ", "))
	}
	showAreas("areas you know", f.Known)
	showAreas("areas agents changed you have not looked at", f.Unseen)
	if f.Partial {
		fmt.Fprintln(w, "familiarity: time limit reached, some repositories were skipped")
	}
}

// botStatsArgs reads --json and --repo OWNER/NAME, in any order, each at most once.
func botStatsArgs(args []string) (jsonOut bool, repo string, err error) {
	seenRepo := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		isRepo := arg == "--repo" || strings.HasPrefix(arg, "--repo=")
		switch {
		case arg == "--json" && !jsonOut:
			jsonOut = true
		case isRepo && seenRepo:
			return false, "", errors.New("--repo is given once")
		case isRepo:
			seenRepo = true
			if value, ok := strings.CutPrefix(arg, "--repo="); ok {
				repo = value
			} else if i+1 < len(args) {
				i++
				repo = args[i]
			} else {
				return false, "", errors.New("--repo needs a value: OWNER/NAME")
			}
		default:
			return false, "", fmt.Errorf("unexpected argument %q: hrn bot stats takes only --json and --repo OWNER/NAME", arg)
		}
	}
	if seenRepo && !game.RepoName(repo) {
		return false, "", fmt.Errorf("--repo wants OWNER/NAME, got %q", repo)
	}
	if seenRepo && !jsonOut {
		return false, "", errors.New("--repo needs --json")
	}
	return jsonOut, repo, nil
}

type statsRepo struct {
	Name  string         `json:"name"`
	Week  int            `json:"week"`
	Days  []int          `json:"days"`
	Kinds map[string]int `json:"kinds"`
	Share float64        `json:"share"`
}

type statsTop struct {
	Repo string `json:"repo"`
	Week int    `json:"week"`
}

type statsAda struct {
	Level      int `json:"level"`
	XPThisWeek int `json:"xp_this_week"`
	XP         int `json:"xp"`
}

type statsExercise struct {
	Kind     string  `json:"kind"`
	Source   string  `json:"source"`
	Material *string `json:"material,omitempty"`
	Answered bool    `json:"answered"`
}

type statsArea struct {
	Repo  string `json:"repo"`
	Area  string `json:"area"`
	Count int    `json:"count"`
}

type statsFamiliarity struct {
	Known   []statsArea `json:"known"`
	Unseen  []statsArea `json:"unseen"`
	Partial bool        `json:"partial"`
}

func jsonFamiliarity(f game.Familiarity) *statsFamiliarity {
	out := &statsFamiliarity{Known: []statsArea{}, Unseen: []statsArea{}, Partial: f.Partial}
	for _, a := range f.Known {
		out.Known = append(out.Known, statsArea{a.Repo, a.Area, a.Count})
	}
	for _, a := range f.Unseen {
		out.Unseen = append(out.Unseen, statsArea{a.Repo, a.Area, a.Count})
	}
	return out
}

// cmdBotStatsJSON prints the stats; given a repository it adds that repository's delivery and skips familiarity.
func cmdBotStatsJSON(repo string) error {
	settings, err := game.LoadSettings()
	if err != nil {
		return err
	}
	if !settings.On() {
		return json.NewEncoder(os.Stdout).Encode(struct {
			Version int  `json:"version"`
			Enabled bool `json:"enabled"`
		}{1, false})
	}
	events, unreadable, err := game.Load()
	if err != nil {
		return err
	}
	if unreadable > 0 {
		fmt.Fprintf(os.Stderr, "hrn bot stats: %d ledger lines could not be read and were left out\n", unreadable)
	}
	now := time.Now().UTC()
	totals := game.Sum(events)
	week := game.WeekStart(now)
	weekly := totals.Since(week)
	practice := game.NewPractice(nil, now)
	streak, days, err := practice.Streak()
	var streakValue *int
	if err != nil {
		fmt.Fprintf(os.Stderr, "hrn bot stats: streak: %v\n", err)
	} else {
		streakValue = &streak
	}
	var exercise *statsExercise
	e, err := practice.ExistingToday()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hrn bot stats: exercise: %v\n", err)
	} else if e != nil {
		exercise = &statsExercise{Kind: e.Kind, Source: e.Source, Answered: !e.AnsweredAt.IsZero()}
		if !exercise.Answered {
			exercise.Material = &e.Material
		}
	}
	badges := []string{}
	for _, badge := range totals.Badges {
		badges = append(badges, game.BadgeName(badge.Key))
	}
	var familiarity *statsFamiliarity
	// Familiarity walks every run record; a caller asking for one repository wants a quick answer.
	if repo == "" {
		familiarity = readStatsFamiliarity(now)
	}
	result := struct {
		Version   int    `json:"version"`
		Enabled   bool   `json:"enabled"`
		Level     int    `json:"level"`
		XP        int    `json:"xp"`
		WeekStart string `json:"week_start"`
		Week      struct {
			Delivery int `json:"delivery"`
			Practice int `json:"practice"`
		} `json:"week"`
		Streak      *int              `json:"streak"`
		Days        []bool            `json:"days"`
		Badges      []string          `json:"badges"`
		Exercise    *statsExercise    `json:"exercise"`
		Familiarity *statsFamiliarity `json:"familiarity"`
		Ada         *statsAda         `json:"ada,omitempty"`
		Repo        *statsRepo        `json:"repo,omitempty"`
		Top         *[]statsTop       `json:"top,omitempty"`
	}{Version: 1, Enabled: true, Level: game.Level(totals.Person), XP: totals.Person, WeekStart: week.Format("2006-01-02"), Streak: streakValue, Days: days, Badges: badges, Exercise: exercise, Familiarity: familiarity}
	result.Ada = &statsAda{Level: totals.AdaLevel, XPThisWeek: weekly[game.TrackAda], XP: totals.XP[game.TrackAda]}
	result.Week.Delivery, result.Week.Practice = weekly[game.TrackDelivery], weekly[game.TrackPractice]
	if repo != "" {
		d := totals.Repo(repo, now)
		result.Repo = &statsRepo{Name: repo, Week: d.Week, Days: d.Days, Kinds: d.Kinds, Share: d.Share}
		top := []statsTop{}
		for _, r := range totals.TopRepos(week, 5) {
			top = append(top, statsTop{Repo: r.Repo, Week: r.Week})
		}
		result.Top = &top
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

// readStatsFamiliarity is nil when familiarity cannot be read, with the reason on stderr.
func readStatsFamiliarity(now time.Time) *statsFamiliarity {
	f, err := game.ReadFamiliarity(context.Background(), now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hrn bot stats: familiarity: %v\n", err)
		return nil
	}
	if f.UnreadablePractice {
		fmt.Fprintln(os.Stderr, "hrn bot stats: familiarity: practice records unreadable")
	}
	if f.Partial {
		fmt.Fprintln(os.Stderr, "hrn bot stats: familiarity: time limit reached, some repositories were skipped")
	}
	return jsonFamiliarity(f)
}

// Verify bot support because another service or an older watch may own the port.
func servesBot(url string) bool {
	serves, _ := probeBot(url)
	return serves
}

// A 403 may be a public watch rejecting the loopback Host, not an absent bot.
func probeBot(url string) (bool, int) {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return false, 0
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	return resp.StatusCode == http.StatusOK && strings.Contains(string(body), `data-stream="/stream/bot"`), resp.StatusCode
}

// Reap the browser asynchronously to avoid Linux zombies while the bot serves.
func openWindow(url string) error {
	argv := windowCommand(runtime.GOOS, os.Getenv, fileExists, exec.LookPath, url)
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

const botWindow = "--window-size=440,720"

func windowCommand(goos string, getenv func(string) string, exists func(string) bool, lookPath func(string) (string, error), url string) []string {
	app := []string{"--app=" + url, botWindow}
	switch goos {
	case "windows":
		for _, c := range []struct{ base, rest string }{
			{"ProgramFiles(x86)", `\Microsoft\Edge\Application\msedge.exe`},
			{"ProgramFiles", `\Microsoft\Edge\Application\msedge.exe`},
			{"ProgramFiles", `\Google\Chrome\Application\chrome.exe`},
			{"ProgramFiles(x86)", `\Google\Chrome\Application\chrome.exe`},
			{"LOCALAPPDATA", `\Google\Chrome\Application\chrome.exe`},
		} {
			if base := getenv(c.base); base != "" && exists(base+c.rest) {
				return append([]string{base + c.rest}, app...)
			}
		}
		return []string{"rundll32", "url.dll,FileProtocolHandler", url}
	case "darwin":
		for _, name := range []string{"Microsoft Edge", "Google Chrome"} {
			if exists("/Applications/" + name + ".app") {
				return []string{"open", "-na", name, "--args", "--app=" + url, botWindow}
			}
		}
		return []string{"open", url}
	}
	for _, name := range []string{"microsoft-edge", "google-chrome", "chromium", "chromium-browser"} {
		if p, err := lookPath(name); err == nil {
			return append([]string{p}, app...)
		}
	}
	return []string{"xdg-open", url}
}

func cmdBotPractice(args []string) error {
	source, err := game.NewGitHubSource()
	if err != nil {
		return err
	}
	p := game.NewPractice(source, time.Now())
	show := func(e game.Exercise, err error) error {
		if err != nil {
			return err
		}
		fmt.Printf("%s ? %s\n\n%s\n", e.Kind, e.Source, e.Material)
		if !e.AnsweredAt.IsZero() {
			fmt.Println(e.Reveal)
		}

		return nil
	}
	if len(args) == 0 {
		e, err := p.Today()
		return show(e, err)
	}
	switch args[0] {
	case "swap":
		if len(args) != 1 {
			return errors.New("swap takes no arguments")
		}
		e, err := p.Swap()
		return show(e, err)
	case "read":
		if len(args) != 2 {
			return errors.New("read needs owner/repo#number")
		}
		e, err := p.Read(args[1])
		return show(e, err)
	case "mark":
		if len(args) != 2 {
			return errors.New("mark needs got_it, partly or missed")
		}
		return p.Mark(args[1])
	case "answer":
		fs := flag.NewFlagSet("practice answer", flag.ContinueOnError)
		file := fs.String("file", "", "")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("answer takes input from stdin or --file")
		}
		var b []byte
		if *file != "" {
			b, err = os.ReadFile(*file)
		} else {
			b, err = io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
		}
		if err != nil {
			return err
		}
		p = game.NewPractice(source, time.Now())
		reveal, err := p.Answer(string(b))
		var warning *game.RebuildWarning
		if err != nil && !errors.As(err, &warning) {
			return err
		}
		fmt.Println(reveal)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		return nil
	default:
		return fmt.Errorf("unknown practice command %q", args[0])
	}
}
