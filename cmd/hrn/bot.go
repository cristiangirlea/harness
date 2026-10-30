package main

import (
	"context"
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
		return openWindow(url)
	}
	url := "http://" + ln.Addr().String() + "/bot"
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	fmt.Printf("hrn bot: %s (this computer only; Ctrl+C stops it)\n", url)
	if err := openWindow(url); err != nil {
		fmt.Fprintf(os.Stderr, "hrn bot: no window opened (%v); open %s in a browser\n", err, url)
	}
	return watch.Serve(ctx, watch.Options{}, ln)
}

func cmdBotStats(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected argument %q: hrn bot stats takes none", args[0])
	}
	if err := game.Rebuild(); err != nil {
		return err
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
	source, err := game.NewGitHubSource()
	if err != nil {
		return err
	}
	streak, days, err := game.NewPractice(source, time.Now()).Streak()
	if err != nil {
		return err
	}
	var squares strings.Builder
	for _, day := range days {
		if day {
			squares.WriteString("\u25a0")
		} else {
			squares.WriteString("\u25a1")
		}
	}
	fmt.Printf("streak: %d days %s\n", streak, squares.String())
	week := game.WeekStart(time.Now())
	xp := totals.Since(week)
	fmt.Printf("this week, from %s UTC: delivery %d xp, practice %d xp\n", week.Format("Monday 2 January 2006"), xp[game.TrackDelivery], xp[game.TrackPractice])
	var badges []string
	for _, badge := range totals.Badges {
		badges = append(badges, game.BadgeName(badge.Key)+" ("+badge.At.UTC().Format("2 January 2006")+")")
	}
	if len(badges) == 0 {
		badges = append(badges, "none yet")
	}
	fmt.Printf("badges: %s\n", strings.Join(badges, ", "))
	familiarity, err := game.ReadFamiliarity(time.Now())
	if err != nil {
		return err
	}
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
		fmt.Printf("%s: %s\n", label, strings.Join(names, ", "))
	}
	showAreas("areas you know", familiarity.Known)
	showAreas("areas agents changed you have not looked at", familiarity.Unseen)
	return nil
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
