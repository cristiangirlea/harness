package execproto

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/prefs"
)

func TestProtocolBackendPreferences(t *testing.T) {
	name := registerFake(t, nil)
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	if err := prefs.Save(home, prefs.Prefs{EnabledNames: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	h := openHarness(t, nil)
	hello := h.next(t, "hello")
	found := false
	for _, raw := range hello["backends"].([]any) {
		b := raw.(map[string]any)
		if b["name"] == name {
			found = true
			if b["available"] != false || !strings.Contains(b["detail"].(string), "disabled") {
				t.Errorf("disabled backend: %+v", b)
			}
		}
	}
	if !found {
		t.Fatal("missing disabled backend")
	}
	h.send(t, start(t.TempDir(), name))
	rejected := h.next(t, "rejected")
	e := rejected["error"].(map[string]any)
	if e["code"] != "unsupported" || !strings.Contains(e["message"].(string), "disabled") || strings.Contains(e["message"].(string), home) {
		t.Fatalf("rejection: %+v", e)
	}
	h.exit(t, 2)
}

func TestProtocolMalformedBackendPreferences(t *testing.T) {
	name := registerFake(t, nil)
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "backends.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	h := openHarness(t, nil)
	hello := h.next(t, "hello")
	for _, raw := range hello["backends"].([]any) {
		b := raw.(map[string]any)
		if b["available"] != false || strings.Contains(b["detail"].(string), home) {
			t.Errorf("backend: %+v", b)
		}
	}
	h.send(t, start(t.TempDir(), name))
	e := h.next(t, "rejected")["error"].(map[string]any)
	if e["code"] != "storage" || strings.Contains(e["message"].(string), home) || strings.Contains(e["message"].(string), "backends.json") {
		t.Fatalf("rejection: %+v", e)
	}
	h.exit(t, 2)
}

func TestProtocolPreferencesAreCheckedAtStart(t *testing.T) {
	name := registerFake(t, nil)
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	h := openHarness(t, nil)
	h.next(t, "hello")
	if err := prefs.Save(home, prefs.Prefs{EnabledNames: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	h.send(t, start(t.TempDir(), name))
	e := h.next(t, "rejected")["error"].(map[string]any)
	if e["code"] != "unsupported" {
		t.Fatalf("rejection: %+v", e)
	}
	h.exit(t, 2)
}
