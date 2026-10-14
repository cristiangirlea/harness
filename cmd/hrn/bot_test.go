package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
