package kernel

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResultsWithoutRefusalFlagsStillRoundTrip(t *testing.T) {
	var r Result
	if err := json.Unmarshal([]byte(`{"backend":"fake","status":"ok","turns":1,"duration_ns":0}`), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.RefusalFlags) != 0 {
		t.Fatal(r.RefusalFlags)
	}
	b, err := json.Marshal(r)
	if err != nil || strings.Contains(string(b), "refusal_flags") {
		t.Fatalf("JSON = %s, error %v", b, err)
	}
	r.RefusalFlags = []RefusalFlag{{Kind: "written", Rule: "permission", Refused: "git push", Later: "write_file", RefusedAt: 0, LaterAt: 2}}
	b, err = json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"refused_call":0`) || !strings.Contains(string(b), `"later_call":2`) || strings.Contains(string(b), `"refused_at"`) || strings.Contains(string(b), `"later_at"`) {
		t.Fatalf("JSON = %s", b)
	}
	var loaded Result
	if err := json.Unmarshal(b, &loaded); err != nil {
		t.Fatal(err)
	}
	if len(loaded.RefusalFlags) != 1 || loaded.RefusalFlags[0] != r.RefusalFlags[0] {
		t.Fatalf("loaded = %+v", loaded)
	}
}
