package watch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHelperVoice is not a test: started as `-test.run=^TestHelperVoice$ -- MODE DIR`,
// the test binary stands in for a speech program, since the reduced environment
// would not carry a switch variable.
func TestHelperVoice(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 || len(os.Args) < i+3 {
		return
	}
	mode, dir := os.Args[i+1], os.Args[i+2]
	in, _ := io.ReadAll(os.Stdin)
	_, marked := os.LookupEnv("HRN_TEST_VOICE_MARKER")
	wd, _ := os.Getwd()
	report, _ := json.Marshal(map[string]any{"stdin": string(in), "args": os.Args[1:], "marked": marked, "dir": wd})
	_ = os.WriteFile(filepath.Join(dir, "report.json"), report, 0o644)
	f, _ := os.OpenFile(filepath.Join(dir, "count"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	f.WriteString("x")
	f.Close()
	sentence := strings.TrimSuffix(string(in), "\n")
	switch mode {
	case "wav":
		os.Stdout.Write(fakeWAV(sentence))
	case "fail":
		os.Stdout.WriteString("HELPER-OUTPUT " + sentence)
		os.Stderr.WriteString("HELPER-OUTPUT " + sentence)
		os.Exit(3)
	case "notwav":
		os.Stdout.WriteString("HELPER-OUTPUT, which is not a sound but is long enough to be one")
	case "big":
		os.Stdout.Write(fakeWAV(strings.Repeat("HELPER-OUTPUT ", 200)))
	case "slow":
		os.Stdout.WriteString("HELPER-OUTPUT")
		time.Sleep(20 * time.Second)
		os.Stdout.Write(fakeWAV(sentence))
	case "gate":
		waitForFile(filepath.Join(dir, "go"))
		os.Stdout.Write(fakeWAV(sentence))
	case "orphan":
		exe, _ := os.Executable()
		c := exec.Command(exe, "-test.run=^TestHelperVoice$", "--", "linger", dir)
		c.Stdout = os.Stdout
		_ = c.Start()
	case "linger":
		waitForFile(filepath.Join(dir, "stop"))
		_ = os.WriteFile(filepath.Join(dir, "gone"), nil, 0o644)
	}
	os.Exit(0)
}

func waitForFile(path string) {
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
}

func fakeWAV(sentence string) []byte {
	head := "RIFF\x00\x00\x00\x00WAVEfmt " + strings.Repeat("\x00", 28)
	return []byte(head + sentence)
}

type voiceFixture struct {
	srv  *httptest.Server
	dir  string
	exe  string
	args []string
}

func newVoiceFixture(t *testing.T, mode string) *voiceFixture {
	t.Helper()
	t.Setenv("HRN_HOME", t.TempDir())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	args := []string{"-test.run=^TestHelperVoice$", "--", mode, dir}
	prog, err := NewVoiceProgram(exe, args)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(t.Context(), Options{Voice: prog}))
	t.Cleanup(srv.Close)
	return &voiceFixture{srv: srv, dir: dir, exe: exe, args: args}
}

func (f *voiceFixture) say(t *testing.T, q string) (*http.Response, []byte) {
	t.Helper()
	return f.ask(t, q, map[string]string{"X-Hrn-Voice": "1"})
}

func (f *voiceFixture) ask(t *testing.T, q string, header map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest("GET", f.srv.URL+"/bot/voice?q="+url.QueryEscape(q), nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func (f *voiceFixture) starts(t *testing.T) int {
	t.Helper()
	return startsIn(f.dir)
}

func startsIn(dir string) int {
	data, _ := os.ReadFile(filepath.Join(dir, "count"))
	return len(data)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(20 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func newTestSpeaker(t *testing.T, mode string) (*speaker, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	prog, err := NewVoiceProgram(exe, []string{"-test.run=^TestHelperVoice$", "--", mode, dir})
	if err != nil {
		t.Fatal(err)
	}
	return newSpeaker(prog), dir
}

func lower[T any](t *testing.T, v *T, to T) {
	old := *v
	*v = to
	t.Cleanup(func() { *v = old })
}

func TestTheLocalVoiceSpeaksASentenceThroughTheProgram(t *testing.T) {
	t.Setenv("HRN_TEST_VOICE_MARKER", "1")
	f := newVoiceFixture(t, "wav")
	resp, body := f.say(t, "Claude has finished in harness.")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/wav" || string(body) != string(fakeWAV("Claude has finished in harness.")) {
		t.Fatalf("%d %q: %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", resp.Header.Get("Cache-Control"))
	}
	data, err := os.ReadFile(filepath.Join(f.dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var seen struct {
		Stdin  string
		Args   []string
		Marked bool
		Dir    string
	}
	if err := json.Unmarshal(data, &seen); err != nil {
		t.Fatal(err)
	}
	if seen.Stdin != "Claude has finished in harness.\n" {
		t.Errorf("stdin %q", seen.Stdin)
	}
	if !slices.Equal(seen.Args, f.args) {
		t.Errorf("args %q; want %q", seen.Args, f.args)
	}
	if seen.Marked {
		t.Error("the program saw a variable outside the fixed set")
	}
	want, _ := filepath.EvalSymlinks(filepath.Dir(f.exe))
	if got, _ := filepath.EvalSymlinks(seen.Dir); !strings.EqualFold(got, want) {
		t.Errorf("started in %q; want %q", got, want)
	}
	if _, body := f.say(t, "Claude has finished in harness."); string(body) != string(fakeWAV("Claude has finished in harness.")) || f.starts(t) != 1 {
		t.Errorf("a cached sentence started the program again: %d starts", f.starts(t))
	}
}

func TestTwoRequestsForOneSentenceStartTheProgramOnce(t *testing.T) {
	f := newVoiceFixture(t, "gate")
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if resp, _ := f.say(t, "Same sentence."); resp.StatusCode != 200 {
				t.Errorf("status %d", resp.StatusCode)
			}
		})
	}
	waitUntil(t, "the program to start", func() bool { return f.starts(t) == 1 })
	time.Sleep(300 * time.Millisecond) // the second request arrives while the first is still running
	if err := os.WriteFile(filepath.Join(f.dir, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if n := f.starts(t); n != 1 {
		t.Errorf("%d starts; want 1", n)
	}
}

func TestAFailedSpeechProgramAnswers502AndIsNotCached(t *testing.T) {
	lower(t, &speechTimeout, 500*time.Millisecond)
	lower(t, &speechMaxOutput, int64(1024))
	for _, mode := range []string{"fail", "notwav", "big", "slow"} {
		t.Run(mode, func(t *testing.T) {
			f := newVoiceFixture(t, mode)
			for range 2 {
				began := time.Now()
				resp, body := f.say(t, "Sentence.")
				if resp.StatusCode != http.StatusBadGateway {
					t.Fatalf("%d: %q", resp.StatusCode, body)
				}
				if strings.Contains(string(body), "HELPER-OUTPUT") || strings.Contains(string(body), f.exe) || strings.Contains(string(body), filepath.Base(f.exe)) {
					t.Errorf("the answer shows the output or the path: %q", body)
				}
				if d := time.Since(began); d > 10*time.Second {
					t.Errorf("took %v", d)
				}
			}
			if n := f.starts(t); n != 2 {
				t.Errorf("%d starts; want 2, as errors are not cached", n)
			}
		})
	}
}

func TestTheSpeechCacheDropsTheOldestPastItsBounds(t *testing.T) {
	t.Run("entries", func(t *testing.T) {
		lower(t, &speechCacheEntries, 2)
		f := newVoiceFixture(t, "wav")
		for _, q := range []string{"a", "b", "c", "c", "b", "a"} {
			f.say(t, q)
		}
		if n := f.starts(t); n != 4 {
			t.Errorf("%d starts; want 4 (a dropped, then asked again)", n)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		lower(t, &speechCacheBytes, 2*len(fakeWAV("a")))
		f := newVoiceFixture(t, "wav")
		for _, q := range []string{"a", "b", "c", "c", "b", "a"} {
			f.say(t, q)
		}
		if n := f.starts(t); n != 4 {
			t.Errorf("%d starts; want 4 (a dropped, then asked again)", n)
		}
	})
}

func TestTheVoiceRouteRefusesWhatItMustNotSpeak(t *testing.T) {
	t.Run("unconfigured", func(t *testing.T) {
		t.Setenv("HRN_HOME", t.TempDir())
		srv := httptest.NewServer(New(t.Context(), Options{}))
		defer srv.Close()
		if code, _ := get(t, srv.URL+"/bot/voice?q=hello"); code != 404 {
			t.Errorf("%d; want 404", code)
		}
	})
	f := newVoiceFixture(t, "wav")
	for name, q := range map[string]string{
		"empty":     "  ",
		"too long":  strings.Repeat("a", 601),
		"not utf-8": "caf\xe9",
		"control":   "one\ntwo",
		"delete":    "one\x7f",
	} {
		if resp, _ := f.say(t, q); resp.StatusCode != 400 {
			t.Errorf("%s: %d; want 400", name, resp.StatusCode)
		}
	}
	if resp, _ := f.say(t, strings.Repeat("a", 600)); resp.StatusCode != 200 {
		t.Errorf("600 bytes: %d; want 200", resp.StatusCode)
	}
	prog, err := NewVoiceProgram(f.exe, f.args)
	if err != nil {
		t.Fatal(err)
	}
	public := &server{opt: Options{Voice: prog, Public: &PublicAccess{}}, speech: newSpeaker(prog)}
	r := httptest.NewRequest("GET", "/bot/voice?q=hello", nil)
	r.Header.Set("X-Hrn-Voice", "1")
	w := httptest.NewRecorder()
	public.speakBot(w, r)
	if w.Code != 403 {
		t.Errorf("public: %d; want 403", w.Code)
	}
	lan := New(t.Context(), Options{Voice: prog, LAN: []Endpoint{{Addr: "192.168.1.20:8089"}}})
	r = httptest.NewRequest("GET", "/bot/voice?q=hello", nil)
	r.Host, r.RemoteAddr = "192.168.1.20:8089", "192.168.1.50:6000"
	r.Header.Set("X-Hrn-Voice", "1")
	w = httptest.NewRecorder()
	lan.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Errorf("lan: %d; want 403", w.Code)
	}
	if n := f.starts(t); n != 1 {
		t.Errorf("%d starts; want only the 600-byte sentence", n)
	}
}

func TestNewVoiceProgramRefusesWhatCannotBeRunSafely(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cases := map[string]struct {
		path string
		args []string
	}{
		"empty":       {"", nil},
		"relative":    {"speak", nil},
		"missing":     {filepath.Join(dir, "missing"), nil},
		"directory":   {dir, nil},
		"NUL in path": {exe + "\x00", nil},
		"NUL in arg":  {exe, []string{"a\x00b"}},
	}
	if runtime.GOOS == "windows" {
		for _, name := range []string{"speak.bat", "speak.CMD"} {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, []byte("@echo off\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cases[name] = struct {
				path string
				args []string
			}{p, nil}
		}
	}
	for name, c := range cases {
		if _, err := NewVoiceProgram(c.path, c.args); err == nil || !strings.Contains(err.Error(), "--voice") {
			t.Errorf("%s: err = %v; want a refusal naming --voice", name, err)
		}
	}
	if _, err := NewVoiceProgram(exe, []string{"--fast"}); err != nil {
		t.Errorf("a real program: %v", err)
	}
}

func TestOnlyAdasOwnPageCanAskForASentence(t *testing.T) {
	f := newVoiceFixture(t, "wav")
	for name, h := range map[string]map[string]string{
		"no header":                     {},
		"another page on this computer": {"X-Hrn-Voice": "1", "Sec-Fetch-Site": "same-site"},
		"another site":                  {"X-Hrn-Voice": "1", "Sec-Fetch-Site": "cross-site"},
	} {
		if resp, _ := f.ask(t, "Hello.", h); resp.StatusCode != 403 {
			t.Errorf("%s: %d; want 403", name, resp.StatusCode)
		}
	}
	if n := f.starts(t); n != 0 {
		t.Fatalf("%d starts; want 0", n)
	}
	for name, h := range map[string]map[string]string{
		"same origin":       {"X-Hrn-Voice": "1", "Sec-Fetch-Site": "same-origin"},
		"no Sec-Fetch-Site": {"X-Hrn-Voice": "1"},
	} {
		if resp, _ := f.ask(t, "Hello.", h); resp.StatusCode != 200 {
			t.Errorf("%s: %d; want 200", name, resp.StatusCode)
		}
	}
}

func TestNewVoiceProgramRefusesWindowsNamesThatHideAScript(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows file names")
	}
	dir := t.TempDir()
	for _, name := range []string{"speak.bat", "speak.cmd", "speak.exe", "speak.txt", "speak.exe:stream", "speak.txt:hidden.exe"} {
		// The stream needs NTFS; without it the path is refused as missing.
		_ = os.WriteFile(filepath.Join(dir, name), []byte("@echo off\r\n"), 0o644)
	}
	for _, name := range []string{"speak.bat.", "speak.cmd ", "speak.exe:stream", "speak.txt:hidden.exe"} {
		if _, err := NewVoiceProgram(filepath.Join(dir, name), nil); err == nil || !strings.Contains(err.Error(), "not a plain file name") {
			t.Errorf("%q: err = %v; want it refused as not a plain file name", name, err)
		}
	}
}

func TestAWindowsProgramWithoutExeIsRefusedSoABatchFileBesideItNeverRuns(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows looks up name.bat for name")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	speak, marker := filepath.Join(dir, "speak"), filepath.Join(dir, "marker")
	if err := os.WriteFile(speak, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "speak.bat"), []byte("@echo off\r\necho x> \""+marker+"\"\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewVoiceProgram(speak, []string{"-test.run=^TestHelperVoice$", "--", "wav", dir}); err == nil || !strings.Contains(err.Error(), "--voice") {
		t.Errorf("err = %v; want a refusal naming --voice", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the batch file ran")
	}
}

func TestAChildThatKeepsTheOutputOpenDoesNotHoldTheAnswer(t *testing.T) {
	lower(t, &speechTimeout, 2*time.Second)
	f := newVoiceFixture(t, "orphan")
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(f.dir, "stop"), nil, 0o644)
		waitUntil(t, "the grandchild to end", func() bool {
			_, err := os.Stat(filepath.Join(f.dir, "gone"))
			return err == nil
		})
		time.Sleep(200 * time.Millisecond) // it is exiting; let it let go of the folder
	})
	began := time.Now()
	if resp, body := f.say(t, "Sentence."); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("%d: %q", resp.StatusCode, body)
	}
	if d := time.Since(began); d > 10*time.Second {
		t.Errorf("took %v; the grandchild held the answer", d)
	}
}

func TestABrowserThatGoesAwayStopsTheProgramAndCachesNothing(t *testing.T) {
	sp, dir := newTestSpeaker(t, "gate")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := sp.say(ctx, "Sentence.")
		done <- err
	}()
	waitUntil(t, "the program to start", func() bool { return startsIn(dir) == 1 })
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled sentence was said")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("say did not return after the request was cancelled")
	}
	if _, ok := sp.cached("Sentence."); ok {
		t.Error("a cancelled sentence was cached")
	}
	if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := sp.say(t.Context(), "Sentence."); err != nil || string(b) != string(fakeWAV("Sentence.")) {
		t.Errorf("%v: %q", err, b)
	}
	if n := startsIn(dir); n != 2 {
		t.Errorf("%d starts; want 2", n)
	}
}

func TestARequestCancelledWhileWaitingForItsTurnStartsNothing(t *testing.T) {
	sp, dir := newTestSpeaker(t, "gate")
	first := make(chan error, 1)
	go func() {
		_, err := sp.say(t.Context(), "First.")
		first <- err
	}()
	waitUntil(t, "the program to start", func() bool { return startsIn(dir) == 1 })
	ctx, cancel := context.WithCancel(t.Context())
	second := make(chan error, 1)
	go func() {
		_, err := sp.say(ctx, "Second.")
		second <- err
	}()
	time.Sleep(200 * time.Millisecond) // it waits for the turn
	cancel()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v; want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled request kept waiting for its turn")
	}
	if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Error(err)
	}
	if n := startsIn(dir); n != 1 {
		t.Errorf("%d starts; want 1", n)
	}
}

func TestASoundLargerThanTheCacheIsServedButNotKept(t *testing.T) {
	lower(t, &speechCacheBytes, len(fakeWAV("a"))-1)
	f := newVoiceFixture(t, "wav")
	for range 2 {
		if resp, body := f.say(t, "a"); resp.StatusCode != 200 || string(body) != string(fakeWAV("a")) {
			t.Fatalf("%d: %q", resp.StatusCode, body)
		}
	}
	if n := f.starts(t); n != 2 {
		t.Errorf("%d starts; want 2", n)
	}
}

func TestOnlyTheBotPageOnThisComputerOffersTheLocalVoice(t *testing.T) {
	f := newVoiceFixture(t, "wav")
	resp, err := http.Get(f.srv.URL + "/bot")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "data-local-voice") || resp.Header.Get("Content-Security-Policy") != csp+"; media-src blob:" {
		t.Errorf("the bot page lacks the local voice: CSP %q", resp.Header.Get("Content-Security-Policy"))
	}
	resp, err = http.Get(f.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Content-Security-Policy") != csp {
		t.Errorf("the index's CSP changed: %q", resp.Header.Get("Content-Security-Policy"))
	}

	t.Setenv("HRN_HOME", t.TempDir())
	plain := httptest.NewServer(New(t.Context(), Options{}))
	defer plain.Close()
	resp, err = http.Get(plain.URL + "/bot")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "data-local-voice") || resp.Header.Get("Content-Security-Policy") != csp {
		t.Errorf("without a program the bot page offers the local voice: CSP %q", resp.Header.Get("Content-Security-Policy"))
	}

	prog, _ := NewVoiceProgram(f.exe, f.args)
	h := New(t.Context(), Options{Voice: prog, LAN: []Endpoint{{Addr: "192.168.1.20:8089"}}})
	lan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host, r.RemoteAddr = "192.168.1.20:8089", "192.168.1.50:6000"
		h.ServeHTTP(w, r)
	}))
	defer lan.Close()
	resp, err = http.Get(lan.URL + "/bot")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.Contains(string(body), "data-local-voice") || resp.Header.Get("Content-Security-Policy") != csp {
		t.Errorf("a phone on the network is offered the local voice: %d, CSP %q", resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
	}
}
