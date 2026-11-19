package watch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/nowindow"
)

// VoiceProgram is a speech program on this computer: one sentence on standard input, a WAV file on standard output.
type VoiceProgram struct {
	Path string
	Args []string
}

// NewVoiceProgram checks the program before hrn watch listens, so a bad --voice stops it at once.
func NewVoiceProgram(path string, args []string) (*VoiceProgram, error) {
	if path == "" {
		return nil, errors.New("--voice needs the full path of a speech program")
	}
	if strings.ContainsRune(path, 0) {
		return nil, errors.New("--voice holds a NUL byte")
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("--voice needs the full path of a speech program, not %q", path)
	}
	if base := filepath.Base(path); runtime.GOOS == "windows" && (strings.HasSuffix(base, ".") || strings.HasSuffix(base, " ") || strings.Contains(path[len(filepath.VolumeName(path)):], ":")) {
		return nil, fmt.Errorf("--voice %s is not a plain file name", path)
	}
	// Go's exec on Windows runs name.bat for a name without an extension, and .bat or .cmd through the command interpreter.
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(path), ".exe") {
		return nil, fmt.Errorf("--voice %s is not an .exe file; name a program, or a script's interpreter with the script as --voice-arg", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("--voice %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("--voice %s is not a program file", path)
	}
	for _, a := range args {
		if strings.ContainsRune(a, 0) {
			return nil, errors.New("--voice-arg holds a NUL byte")
		}
	}
	return &VoiceProgram{Path: path, Args: append([]string(nil), args...)}, nil
}

// Package variables so tests can lower them; there are no flags for them.
var (
	speechTimeout            = 30 * time.Second
	speechMaxOutput    int64 = 16 << 20
	speechCacheEntries       = 64
	speechCacheBytes         = 32 << 20
)

type speaker struct {
	prog    VoiceProgram
	turn    chan struct{} // one synthesis at a time
	cacheMu sync.Mutex
	cache   map[string][]byte
	order   []string // oldest first
	size    int
}

func newSpeaker(prog *VoiceProgram) *speaker {
	if prog == nil {
		return nil
	}
	return &speaker{prog: *prog, turn: make(chan struct{}, 1), cache: map[string][]byte{}}
}

// The program and its arguments are fixed for the process, so the sentence alone is the key.
func (sp *speaker) say(ctx context.Context, text string) ([]byte, error) {
	if b, ok := sp.cached(text); ok {
		return b, nil
	}
	select {
	case sp.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-sp.turn }()
	// A request that waited for the turn finds what the one before it made.
	if b, ok := sp.cached(text); ok {
		return b, nil
	}
	b, err := sp.run(ctx, text)
	if err != nil {
		return nil, err
	}
	sp.keep(text, b)
	return b, nil
}

func (sp *speaker) run(ctx context.Context, text string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, speechTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, sp.prog.Path, sp.prog.Args...)
	nowindow.Set(cmd)
	cmd.Env = speechEnv()
	cmd.Dir = filepath.Dir(sp.prog.Path)
	cmd.Stdin = strings.NewReader(text + "\n")
	out := &cappedBuffer{max: speechMaxOutput, stop: cancel}
	cmd.Stdout = out // stderr stays unread: it may echo the sentence
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	b := out.buf.Bytes()
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, errors.New("the speech program did not write a WAV file")
	}
	return b, nil
}

// No API key or token reaches the program: only these variables, when set.
func speechEnv() []string {
	names := []string{"PATH", "HOME", "USERPROFILE", "SystemRoot", "SYSTEMROOT", "TEMP", "TMP", "TMPDIR", "LANG", "LC_ALL"}
	if runtime.GOOS == "windows" {
		names = append(names, "APPDATA", "LOCALAPPDATA", "PATHEXT")
	}
	env := []string{} // never nil: a nil Env passes hrn's whole environment
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok {
			env = append(env, n+"="+v)
		}
	}
	return env
}

type cappedBuffer struct {
	buf  bytes.Buffer
	max  int64
	stop func()
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if int64(c.buf.Len())+int64(len(p)) > c.max {
		c.stop()
		return 0, errors.New("the speech program wrote too much")
	}
	return c.buf.Write(p)
}

func (sp *speaker) cached(text string) ([]byte, bool) {
	sp.cacheMu.Lock()
	defer sp.cacheMu.Unlock()
	b, ok := sp.cache[text]
	return b, ok
}

func (sp *speaker) keep(text string, b []byte) {
	sp.cacheMu.Lock()
	defer sp.cacheMu.Unlock()
	if len(b) > speechCacheBytes {
		return
	}
	sp.cache[text] = b
	sp.order = append(sp.order, text)
	sp.size += len(b)
	for len(sp.order) > speechCacheEntries || sp.size > speechCacheBytes {
		sp.size -= len(sp.cache[sp.order[0]])
		delete(sp.cache, sp.order[0])
		sp.order = sp.order[1:]
	}
}

func speakable(q string) bool {
	if q == "" || len(q) > 600 || !utf8.ValidString(q) {
		return false
	}
	return !strings.ContainsFunc(q, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// The program runs on this computer for this computer's browser only; its sound never crosses the network.
func (s *server) speakBot(w http.ResponseWriter, r *http.Request) {
	if !s.fromThisComputer(r) {
		http.Error(w, "The local voice works only on this computer", http.StatusForbidden)
		return
	}
	if s.speech == nil {
		http.NotFound(w, r)
		return
	}
	// A media element on another local page sends neither header; fetch with it would need a preflight hrn never grants.
	if site := r.Header.Get("Sec-Fetch-Site"); r.Header.Get("X-Hrn-Voice") != "1" || site != "" && !strings.EqualFold(site, "same-origin") {
		http.Error(w, "Only Ada's page can ask for the local voice", http.StatusForbidden)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if !speakable(q) {
		http.Error(w, "Not a sentence the local voice can say", http.StatusBadRequest)
		return
	}
	b, err := s.speech.say(r.Context(), q)
	// Waiting for the turn and the program can outlast the guard's write deadline.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err != nil {
		http.Error(w, "The local voice could not say that", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	_, _ = w.Write(b)
}
