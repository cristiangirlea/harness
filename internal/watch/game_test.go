package watch

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/game"
)

func TestGameCardVisibility(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now().UTC()
	if _, err := game.Append([]game.Event{{Key: "first", Kind: "first_use", Track: game.TrackDelivery, XP: 50, At: now}, {Key: "badge:first-merge", Track: game.TrackBadge, At: now}}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(game.Dir(), "reps")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	record := map[string]any{"date": now.Format("2006-01-02"), "daily": map[string]any{"kind": "recall", "source": "sample source", "material": "masked •••• material", "reveal": "hidden reveal", "answer": "hidden answer"}}
	data, _ := json.Marshal(record)
	if err := os.WriteFile(filepath.Join(dir, now.Format("2006-01-02")+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, remote, host, header     string
		public, card, material, weekly bool
	}{
		{"local", "127.0.0.1:1234", "127.0.0.1:8089", "", false, true, true, true},
		{"lan", "192.168.1.5:1234", "192.168.1.2:8089", "", false, true, false, false},
		{"forwarded", "127.0.0.1:1234", "127.0.0.1:8089", "Forwarded", false, true, false, false},
		{"public", "127.0.0.1:1234", "example.com", "", true, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &server{opt: Options{}}
			if tc.public {
				s.opt.Public = &PublicAccess{}
			}
			req := httptest.NewRequest("GET", "http://"+tc.host+"/", nil)
			req.RemoteAddr = tc.remote
			if tc.header != "" {
				req.Header.Set(tc.header, "for=192.168.1.5")
			}
			w := httptest.NewRecorder()
			s.index(w, req)
			body := w.Body.String()
			for text, want := range map[string]bool{"id=\"game\"": tc.card, "masked •••• material": tc.material, "sample source": tc.material, "This week": tc.weekly, "hidden reveal": false, "hidden answer": false, "First merge": tc.card, "Level 1": tc.card} {
				if strings.Contains(body, text) != want {
					t.Errorf("%q present = %v, want %v", text, !want, want)
				}
			}
			if tc.card && strings.Count(body, "<span class=\"game-day") != 35 {
				t.Errorf("expected 35 squares")
			}
		})
	}
	if err := os.WriteFile(filepath.Join(game.Dir(), "settings.json"), []byte(`{"enabled":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := &server{}
	req := httptest.NewRequest("GET", "http://127.0.0.1/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	s.index(w, req)
	if strings.Contains(w.Body.String(), `id="game"`) {
		t.Fatal("disabled card shown")
	}
}

func TestGameCardNoExerciseAndAnswered(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	s := &server{}
	req := httptest.NewRequest("GET", "http://127.0.0.1/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	s.index(w, req)
	if !strings.Contains(w.Body.String(), "hrn bot practice") {
		t.Fatal("missing practice hint")
	}
	dir := filepath.Join(game.Dir(), "reps")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	data, _ := json.Marshal(map[string]any{"date": now.Format("2006-01-02"), "daily": map[string]any{"kind": "recall", "source": "source", "material": "material", "reveal": "private reveal", "answer": "private answer", "answered_at": now}})
	if err := os.WriteFile(filepath.Join(dir, now.Format("2006-01-02")+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	exercise, err := game.NewPractice(nil, now).ExistingToday()
	if err != nil || exercise == nil || exercise.Answer != "" || exercise.Reveal != "" {
		t.Fatal("read-only exercise exposed an answer or reveal")
	}
	w = httptest.NewRecorder()
	s.index(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "Done") || strings.Contains(body, "private reveal") || strings.Contains(body, "private answer") {
		t.Fatal("answered exercise was not safely shown")
	}
}

type cardSource struct{}

func (cardSource) Candidates(kind string) ([]game.Candidate, error) {
	return []game.Candidate{{Kind: kind, Source: "fixture", Material: "password=supersecret", Reveal: "reveal secret"}}, nil
}
func (cardSource) Read(string) (game.Candidate, error)    { return game.Candidate{}, nil }
func (cardSource) Merged(string, time.Time) (bool, error) { return false, nil }

func TestGameCardUsesStoredMaskedExerciseWithoutChangingIt(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now().UTC()
	if _, err := game.NewPractice(cardSource{}, now).Today(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(game.Dir(), "reps", now.Format("2006-01-02")+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{}
	req := httptest.NewRequest("GET", "http://127.0.0.1/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	s.index(w, req)
	body := w.Body.String()
	if strings.Contains(body, "supersecret") || strings.Contains(body, "reveal secret") || !strings.Contains(body, "••••") {
		t.Fatal("material was not masked or reveal was shown")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("render changed the exercise record")
	}
}
