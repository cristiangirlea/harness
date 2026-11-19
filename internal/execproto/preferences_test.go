package execproto

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/prefs/prefstest"
	"github.com/cristiangirlea/harness/internal/runlog"
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

func TestProtocolAccountLimitAndRemovedPinRejectCodes(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	orig, _ := agent.Get("claude")
	agent.Register(agent.NewClaudeBackend())
	t.Cleanup(func() { agent.Register(orig) })
	p := prefs.Prefs{Accounts: map[string][]prefs.Account{"claude": {{Name: "team-a", Home: t.TempDir()}}}}
	if err := prefs.Save(runlog.Home(), p); err != nil {
		t.Fatal(err)
	}
	if err := limits.NoteAccount("claude", "team-a", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rec, err := runlog.Create("claude", agent.Task{Dir: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(agent.Result{Backend: "claude", Account: "removed", Session: "saved", Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, code, message string }{
		{"", "unavailable", "accounts are all at their usage limit"},
		{rec.ID(), "bad_request", "no longer declared"},
	} {
		h := openHarness(t, nil)
		h.next(t, "hello")
		req := start(dir, "claude")
		if tc.id != "" {
			req["task"].(map[string]any)["continue_run_id"] = tc.id
		}
		h.send(t, req)
		e := h.next(t, "rejected")["error"].(map[string]any)
		if e["code"] != tc.code || !strings.Contains(e["message"].(string), tc.message) {
			t.Errorf("rejection: %v", e)
		}
		h.exit(t, 2)
	}
}

func TestProtocolMalformedOrganisationPreferences(t *testing.T) {
	name := registerFake(t, nil)
	home := prefstest.TempDir(t)
	t.Setenv("HRN_HOME", home)
	policy := prefstest.OrgPath(t)
	prefstest.Write(t, policy, "{")
	h := openHarness(t, nil)
	hello := h.next(t, "hello")
	for _, raw := range hello["backends"].([]any) {
		if b := raw.(map[string]any); b["available"] != false {
			t.Errorf("backend available: %+v", b)
		}
	}
	h.send(t, start(prefstest.TempDir(t), name))
	rejected := h.next(t, "rejected")
	if e := rejected["error"].(map[string]any); e["code"] != "storage" {
		t.Fatalf("rejection: %+v", e)
	}
	for _, frame := range []map[string]any{hello, rejected} {
		raw, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "backends.json") || strings.Contains(string(raw), strings.ReplaceAll(home, `\`, `\`)) || strings.Contains(string(raw), strings.ReplaceAll(filepath.Dir(policy), `\`, `\`)) {
			t.Fatalf("frame exposes preference location: %s", raw)
		}
	}
	h.exit(t, 2)
}
