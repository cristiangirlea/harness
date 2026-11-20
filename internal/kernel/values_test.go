package kernel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDurationJSON(t *testing.T) {
	type doc struct {
		Timeout Duration `json:"timeout,omitempty"`
	}
	for _, tc := range []struct {
		in   string
		want Duration
	}{
		{`{"timeout":"20m"}`, Duration(20 * time.Minute)},
		{`{"timeout":"1h30m"}`, Duration(90 * time.Minute)},
		{`{"timeout":null}`, 0}, // absent, as a generated document spells it
		{`{}`, 0},
	} {
		var d doc
		if err := json.Unmarshal([]byte(tc.in), &d); err != nil || d.Timeout != tc.want {
			t.Errorf("%s: got %s err=%v, want %s", tc.in, d.Timeout, err, tc.want)
		}
	}
	var d doc
	if err := json.Unmarshal([]byte(`{"timeout":"soon"}`), &d); err == nil || !strings.Contains(err.Error(), "20m") {
		t.Errorf("a bad duration should say what it wants: %v", err)
	}
	// A bare number is refused: 300 meaning seconds would have run as 300ns.
	if err := json.Unmarshal([]byte(`{"timeout":300}`), &d); err == nil || !strings.Contains(err.Error(), "20m") {
		t.Errorf("a number must be refused, not read as nanoseconds: %v", err)
	}
	out, _ := json.Marshal(doc{Timeout: Duration(20 * time.Minute)})
	if string(out) != `{"timeout":"20m0s"}` {
		t.Errorf("marshal = %s", out)
	}
	if out, _ := json.Marshal(doc{}); string(out) != `{}` {
		t.Errorf("zero must be omitted: %s", out)
	}
}

func TestCompactWindowAcceptsTheSharedRangeAndAuto(t *testing.T) {
	for _, v := range []string{"", "auto", "20480", "99999", "100000", "1000000"} {
		if err := CompactWindow(v).Check(); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	for _, v := range []string{"16000", "20479", "1000001", "0200000", "+200000"} {
		if err := CompactWindow(v).Check(); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

func TestTheEgressFlagTakesACommaListAndRepeats(t *testing.T) {
	var h HostList
	for _, v := range []string{"api.example.com, auth.example.com", "chat.example.com"} {
		if err := h.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if want := "api.example.com,auth.example.com,chat.example.com"; h.String() != want {
		t.Fatalf("hosts %q, want %q", h.String(), want)
	}
	data, err := json.Marshal(Options{Egress: h})
	if err != nil || string(data) != `{"egress":["api.example.com","auth.example.com","chat.example.com"]}` {
		t.Fatalf("json %s, %v", data, err)
	}
	if s := (Options{}).Schema()["egress"].(map[string]any); s["type"] != "array" {
		t.Errorf("schema %v, want an array", s)
	}
}

func TestEgressHostsAreCheckedBeforeARun(t *testing.T) {
	if err := (Options{Egress: HostList{"api.example.com"}}).ValidateShape(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []HostList{{"*.example.com"}, {"10.0.0.1"}, {"example.com:443"}, {"Example.com"}, {"example.com", "example.com"}, {""}, {}} {
		if err := (Options{Egress: bad}).ValidateShape(); err == nil || !strings.Contains(err.Error(), "egress") {
			t.Errorf("egress %q: %v, want an egress error", bad, err)
		}
	}
}
