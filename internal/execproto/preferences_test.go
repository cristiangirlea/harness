package execproto

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/prefs"
)

func TestProtocolAppliesMeteredAPICeiling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"claude-opus-4-6\",\"usage\":{\"input_tokens\":1000}}}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"pause_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(server.Close)
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	if err := prefs.Save(home, prefs.Prefs{EnabledNames: []string{"api"}, Billing: "metered", BillingMaxCostUSD: 0.000001, LocalAPI: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	original, err := agent.Get("api")
	if err != nil {
		t.Fatal(err)
	}
	api := agent.NewAPIBackend()
	api.BaseURL, api.APIKey = server.URL, "fixture"
	agent.Register(api)
	t.Cleanup(func() { agent.Register(original) })

	h := openHarness(t, nil)
	h.next(t, "hello")
	req := start(t.TempDir(), "api")
	task := req["task"].(map[string]any)
	task["model"], task["max_turns"] = "claude-opus-4-6", 2
	h.send(t, req)
	h.next(t, "accepted")
	for {
		frame := h.nextAny(t)
		if frame["type"] == "result" {
			result := frame["result"].(map[string]any)
			if result["status"] != string(agent.StatusBudget) || result["turns"] != float64(1) {
				t.Errorf("result: %+v", result)
			}
			break
		}
	}
	h.exit(t, 0)
	if api.CostCeilingUSD != 0 {
		t.Errorf("registered backend was modified: %v", api.CostCeilingUSD)
	}
}

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
	for i, backend := range []string{name, "api"} {
		if i != 0 {
			h = openHarness(t, nil)
			h.next(t, "hello")
		}
		h.send(t, start(t.TempDir(), backend))
		e := h.next(t, "rejected")["error"].(map[string]any)
		if e["code"] != "storage" || strings.Contains(e["message"].(string), home) || strings.Contains(e["message"].(string), "backends.json") {
			t.Fatalf("%s rejection: %+v", backend, e)
		}
		h.exit(t, 2)
	}
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
