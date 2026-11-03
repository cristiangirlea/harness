package platform

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestJobResultDecode(t *testing.T) {
	for _, status := range []string{"ok", "error", "refused", "max_turns", "timeout", "budget", "canceled", "limited"} {
		var result JobResult
		if err := json.Unmarshal([]byte(`{"status":"`+status+`"}`), &result); err != nil {
			t.Errorf("%s: %v", status, err)
		}
	}
	for _, input := range []string{`{"status":"future"}`, `{"status":""}`, `{"status":"ok","extra":1}`} {
		var result JobResult
		if err := json.Unmarshal([]byte(input), &result); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
}

func TestCheckpointDecode(t *testing.T) {
	raw := []byte("{ \"version\":1, \"sequence\":2, \"boundary\":\"finished\", \"task_digest\":\"abc\", \"pending\":null, \"finished\":true, \"result\":{\"status\":\"ok\"}, \"request\": { \"x\": 1 } }")
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cp.Bytes(), raw) || cp.Sequence != 2 || !cp.Finished || cp.Result.Status != StatusOK {
		t.Fatalf("checkpoint did not round trip: %+v", cp)
	}
	encoded, err := cp.MarshalJSON()
	if err != nil || !bytes.Equal(encoded, raw) {
		t.Fatalf("encoded checkpoint changed: %s, %v", encoded, err)
	}
	for _, input := range []string{
		`{"version":2,"sequence":1,"boundary":"x","task_digest":"x"}`,
		`{"version":1,"sequence":0,"boundary":"x","task_digest":"x"}`,
		`{"version":1,"sequence":1,"task_digest":"x"}`,
		`{"version":1,"sequence":1,"boundary":"x"}`,
		`{"version":1,"sequence":1,"boundary":"x","task_digest":"x","extra":1}`,
		`{"version":1,"sequence":1,"boundary":"x","task_digest":"x","pending":{"extra":1}}`,
		`{"version":1,"sequence":1,"boundary":"x","task_digest":"x","finished":true,"result":{"status":"unknown"}}`,
	} {
		var cp Checkpoint
		if err := json.Unmarshal([]byte(input), &cp); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
	tooLarge := []byte(`{"version":1,"sequence":1,"boundary":"x","task_digest":"x","request":"` + strings.Repeat("a", 16*1024*1024) + `"}`)
	if err := json.Unmarshal(tooLarge, &cp); err == nil {
		t.Fatal("accepted oversized checkpoint")
	}
	prefix, suffix := `{"version":1,"sequence":1,"boundary":"x","task_digest":"x","request":"`, `"}`
	atLimit := []byte(prefix + strings.Repeat("a", 16*1024*1024-len(prefix)-len(suffix)) + suffix)
	if err := json.Unmarshal(atLimit, &cp); err != nil {
		t.Fatalf("checkpoint at size limit: %v", err)
	}
}

func TestExecutionWireGolden(t *testing.T) {
	for _, name := range []string{"result.json", "result_evidence.json"} {
		raw, err := os.ReadFile("../../internal/agent/testdata/golden/" + name)
		if err != nil {
			t.Fatal(err)
		}
		result, err := decodeResult(raw, name == "result_evidence.json")
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(result)
		if err != nil || !bytes.Equal(encoded, bytes.TrimSpace(raw)) {
			t.Fatalf("%s changed: %s, %v", name, encoded, err)
		}
	}
	raw, err := os.ReadFile("../../internal/agent/testdata/golden/checkpoint.json")
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.TrimSpace(raw)
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cp.Bytes(), raw) {
		t.Fatal("checkpoint changed")
	}
}
