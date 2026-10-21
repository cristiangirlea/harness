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
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/cristiangirlea/harness/internal/watch"
)

func cmdBot(args []string) error {
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
