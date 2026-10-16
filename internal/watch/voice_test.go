package watch

import (
	"context"
	"fmt"
	"html"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

func TestVoiceJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, node, "--test", "testdata/voice.test.cjs").CombinedOutput(); err != nil {
		t.Fatalf("voice JavaScript tests: %v\n%s", err, out)
	}
}

func agentResultOK(minutes int) agent.Result {
	return agent.Result{Status: agent.StatusOK, Duration: time.Duration(minutes) * time.Minute}
}

// The lines to record are numbered and unique, mix Ada's own sentences, made
// by the same functions she speaks with, with everyday ones, and are short
// enough to read in one breath. A | would break metadata.csv's id|text.
func TestTheVoiceLinesAreAdasOwnAndEveryday(t *testing.T) {
	lines := voiceLines()
	if len(lines) < 450 {
		t.Fatalf("%d lines; want at least 450, about half an hour of speech", len(lines))
	}
	seen := map[string]bool{}
	for i, l := range lines {
		if l.N != i+1 {
			t.Fatalf("line %d is numbered %d", i+1, l.N)
		}
		if seen[l.Text] {
			t.Errorf("line %d repeats %q", l.N, l.Text)
		}
		seen[l.Text] = true
		if n := len(strings.Fields(l.Text)); n == 0 || n > 30 {
			t.Errorf("line %d has %d words: %q", l.N, n, l.Text)
		}
		if strings.ContainsAny(l.Text, "|\r\n") || strings.HasPrefix(l.Text, "#") {
			t.Errorf("line %d cannot go into metadata.csv as it is: %q", l.N, l.Text)
		}
	}
	_, done := runEnding("Claude", "harness", agentResultOK(4))
	for _, want := range []string{
		done, // "Claude has finished in harness. It took four minutes."
		"Good morning. The kettle's on, and the post has just arrived.",
		"I'll keep quiet until you unmute me.",
		"Nothing is running.",
	} {
		if !seen[want] {
			t.Errorf("the lines lack %q", want)
		}
	}
	// Both kinds come early, so a first session already covers both.
	firstAda, firstEveryday := false, false
	for _, l := range lines[:20] {
		firstAda = firstAda || strings.Contains(l.Text, " in ") && (strings.Contains(l.Text, "Claude") || strings.Contains(l.Text, "Codex"))
		firstEveryday = firstEveryday || l.Text == "Good morning. The kettle's on, and the post has just arrived."
	}
	if !firstAda || !firstEveryday {
		t.Errorf("the first 20 lines should mix Ada's and everyday ones: %+v", lines[:20])
	}
}

// A line's number is how a take finds its text (0042.wav is line 42), across
// sessions and in download mode, so recorded lines must keep their numbers and
// words. testdata/voice-lines.golden is the contract: every line in it must
// read the same at the same number, and new lines may only follow it. A change
// to everyday lines or to Ada's wording in bot.go that moves or rewords a line
// fails here, and someone decides: re-record those lines, or keep the old text.
// HRN_WRITE_VOICE_GOLDEN=1 writes the file afresh, for a deliberate new start.
func TestRecordedLinesKeepTheirNumbers(t *testing.T) {
	const golden = "testdata/voice-lines.golden"
	lines := voiceLines()
	if os.Getenv("HRN_WRITE_VOICE_GOLDEN") == "1" {
		var b strings.Builder
		for _, l := range lines {
			fmt.Fprintf(&b, "%d|%s\n", l.N, l.Text)
		}
		if err := os.WriteFile(golden, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) < len(rows) {
		t.Fatalf("%d lines, but %d were promised: lines may be added at the end, never taken away", len(lines), len(rows))
	}
	for i, row := range rows {
		n, text, _ := strings.Cut(row, "|")
		if n != strconv.Itoa(i+1) || lines[i].Text != text {
			t.Errorf("line %s was %q and is now %q: recorded takes would lose their text", n, text, lines[i].Text)
		}
	}
}

// The page shows each line exactly as it was made, through the template's
// escaping and the redaction of secret values, and Ada's replies recorded here
// are the ones her page says.
func TestThePageShowsTheLinesAsTheyAreAndAdasRepliesAreHers(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	req := httptest.NewRequest("GET", "http://127.0.0.1:8089/voice", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	New(context.Background(), Options{}).ServeHTTP(rec, req)
	items := regexp.MustCompile(`<li data-n="(\d+)">([^<]*)</li>`).FindAllStringSubmatch(rec.Body.String(), -1)
	lines := voiceLines()
	if len(items) != len(lines) {
		t.Fatalf("the page shows %d lines, want %d", len(items), len(lines))
	}
	for i, m := range items {
		if m[1] != strconv.Itoa(lines[i].N) || html.UnescapeString(m[2]) != lines[i].Text {
			t.Errorf("line %d shows as %s %q, want %q", lines[i].N, m[1], html.UnescapeString(m[2]), lines[i].Text)
		}
	}
	page := mustAsset("assets/bot.js") + mustAsset("assets/watch.tmpl")
	for _, r := range adaReplies {
		if !strings.Contains(page, r) {
			t.Errorf("Ada no longer says %q on her page: change it in voice.go too, and re-record it", r)
		}
	}
}

// Recording uses the microphone and the takes are private: the page is for
// this computer only, even under --lan, and only it and Ada may ask for the
// microphone.
func TestTheVoicePageIsForThisComputerOnly(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	h := New(context.Background(), Options{LAN: []Endpoint{{Addr: "192.168.1.20:8089", Interface: "Wi-Fi", URL: "http://192.168.1.20:8089/"}}})
	local := httptest.NewRequest("GET", "http://127.0.0.1:8089/voice", nil)
	local.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, local)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Permissions-Policy"), "microphone=(self)") {
		t.Fatalf("from this computer: %d, Permissions-Policy %q", rec.Code, rec.Header().Get("Permissions-Policy"))
	}
	for _, want := range []string{`id="voice-lines"`, `data-n="1"`, `src="/static/voice.js"`, "Choose a folder", "Just record me"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the voice page lacks %q", want)
		}
	}

	phone := httptest.NewRequest("GET", "http://192.168.1.20:8089/voice", nil)
	phone.RemoteAddr = "192.168.1.50:6000"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, phone)
	if rec.Code != 403 || strings.Contains(rec.Body.String(), `id="voice-lines"`) {
		t.Errorf("from the network: %d; want it refused, with no lines", rec.Code)
	}
	if strings.Contains(rec.Header().Get("Permissions-Policy"), "microphone=(self)") {
		t.Error("the refusal still allows the microphone")
	}

	// Ada's page links to it on this computer, not on a phone.
	for _, c := range []struct {
		host, remote string
		link         bool
	}{{"127.0.0.1:8089", "127.0.0.1:5000", true}, {"192.168.1.20:8089", "192.168.1.50:6000", false}} {
		req := httptest.NewRequest("GET", "http://"+c.host+"/bot", nil)
		req.RemoteAddr = c.remote
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := strings.Contains(rec.Body.String(), `href="/voice"`); got != c.link {
			t.Errorf("Ada's page from %s links to /voice: %v, want %v", c.remote, got, c.link)
		}
	}
	for _, name := range []string{"voice.js", "voice-worklet.js"} {
		req := httptest.NewRequest("GET", "http://127.0.0.1:8089/static/"+name, nil)
		req.RemoteAddr = "127.0.0.1:5000"
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
			t.Errorf("/static/%s: %d %s", name, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}
