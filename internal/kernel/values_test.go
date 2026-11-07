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
