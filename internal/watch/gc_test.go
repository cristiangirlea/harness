package watch

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIndexReportsCorruptGCLastOnlyToLocalViewers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	root := t.TempDir()
	b, _ := json.Marshal(map[string]any{"enabled": true, "roots": []string{root}})
	if err := os.WriteFile(filepath.Join(home, "gc.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "gc-last"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	h := New(context.Background(), Options{LAN: []Endpoint{{Addr: "192.168.1.2:8089"}}})
	local := backendRequest(h, http.MethodGet, "/", "127.0.0.1:1234", "127.0.0.1:8089", "", "", "")
	if !strings.Contains(local.Body.String(), "gc-last") {
		t.Fatal("local index omitted gc-last error")
	}
	lan := backendRequest(h, http.MethodGet, "/", "192.168.1.3:1234", "192.168.1.2:8089", "", "", "")
	if strings.Contains(lan.Body.String(), "gc-last") {
		t.Fatal("LAN index exposed gc-last error")
	}
}

func TestIndexShowsAnOverdueGCLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	root := t.TempDir()
	b, _ := json.Marshal(map[string]any{"enabled": true, "roots": []string{root}, "interval": "1h"})
	os.WriteFile(filepath.Join(home, "gc.json"), b, 0600)
	b, _ = json.Marshal(map[string]any{"pid": os.Getpid(), "time": time.Now().Add(-2 * time.Hour)})
	os.WriteFile(filepath.Join(home, "gc.lock"), b, 0600)
	w := backendRequest(New(context.Background(), Options{}), http.MethodGet, "/", "127.0.0.1:1234", "127.0.0.1:8089", "", "", "")
	if !strings.Contains(w.Body.String(), "gc.lock held since") {
		t.Fatalf("missing warning: %s", w.Body.String())
	}
}

func TestIndexShowsLastAutomaticGCPass(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "gc.log"), []byte("{\"time\":\"2026-10-28T12:00:00Z\",\"scheduled\":true,\"actions\":[{\"path\":\"private\",\"bytes\":42,\"reason\":\"idle\",\"kind\":\"remove\"}]}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	w := backendRequest(New(context.Background(), Options{}), http.MethodGet, "/", "127.0.0.1:1234", "127.0.0.1:8089", "", "", "")
	if !strings.Contains(w.Body.String(), "Last automatic GC") || !strings.Contains(w.Body.String(), "42") {
		t.Fatalf("missing pass: %s", w.Body.String())
	}
}

func TestIndexShowsPendingAndUnreadableGCLogsOnlyLocally(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	path := filepath.Join(home, "gc.log")
	for _, tc := range []struct{ body, want string }{{`{"time":"2026-10-28T12:00:00Z","started":true}` + "\n", "outcome pending"}, {"bad\n", "Log unreadable"}} {
		os.WriteFile(path, []byte(tc.body), 0600)
		opt := Options{LAN: []Endpoint{{Addr: "192.168.1.2:8089"}}}
		h := New(context.Background(), opt)
		local := backendRequest(h, http.MethodGet, "/", "127.0.0.1:1234", "127.0.0.1:8089", "", "", "")
		if !strings.Contains(local.Body.String(), tc.want) {
			t.Fatalf("local missing %s", tc.want)
		}
		lan := backendRequest(h, http.MethodGet, "/", "192.168.1.3:1234", "192.168.1.2:8089", "", "", "")
		if strings.Contains(lan.Body.String(), "Last automatic GC") {
			t.Fatal("LAN sees GC line")
		}
	}
}

func TestPublicIndexDoesNotShowGCLog(t *testing.T) {
	f := newAccessFixture(t)
	os.WriteFile(filepath.Join(os.Getenv("HRN_HOME"), "gc.log"), []byte(`{"time":"2026-10-28T12:00:00Z"}`+"\n"), 0600)
	w := f.request(New(context.Background(), f.opt), "/", "harness.example.com", "", f.validToken())
	if w.Code != 200 || strings.Contains(w.Body.String(), "Last automatic GC") {
		t.Fatalf("public index %d: %s", w.Code, w.Body)
	}
}

func TestPublicIndexDoesNotShowCorruptGCLast(t *testing.T) {
	f := newAccessFixture(t)
	b, _ := json.Marshal(map[string]any{"enabled": true, "roots": []string{t.TempDir()}})
	if err := os.WriteFile(filepath.Join(os.Getenv("HRN_HOME"), "gc.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("HRN_HOME"), "gc-last"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	w := f.request(New(context.Background(), f.opt), "/", "harness.example.com", "", f.validToken())
	if w.Code != 200 || strings.Contains(w.Body.String(), "gc-last") {
		t.Fatalf("public index %d: %s", w.Code, w.Body)
	}
}

func TestIndexReportsEnabledGCConfigurationErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	for _, config := range []string{`{"enabled":true,"roots":[]}`, `{"enabled":true,"interval":"bad"}`} {
		os.WriteFile(filepath.Join(home, "gc.json"), []byte(config), 0600)
		w := backendRequest(New(context.Background(), Options{}), http.MethodGet, "/", "127.0.0.1:1234", "127.0.0.1:8089", "", "", "")
		if !strings.Contains(w.Body.String(), "gc.json") {
			t.Fatal("missing configuration error")
		}
	}
}
