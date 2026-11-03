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
	for _, name := range []string{"result.json", "result_evidence.json", "result_zero.json"} {
		raw, err := os.ReadFile("../../internal/agent/testdata/golden/" + name)
		if err != nil {
			t.Fatal(err)
		}
		result, err := decodeResult(raw, name != "result.json")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		encoded, err := json.Marshal(result)
		if err != nil || !bytes.Equal(encoded, bytes.TrimSpace(raw)) {
			t.Fatalf("%s changed: %s, %v", name, encoded, err)
		}
	}
	for _, name := range []string{"checkpoint.json", "checkpoint_zero.json"} {
		raw, err := os.ReadFile("../../internal/agent/testdata/golden/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var cp Checkpoint
		if err := cp.decode(raw, name == "checkpoint_zero.json"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(cp.Bytes(), raw) {
			t.Fatalf("%s changed", name)
		}
	}
	for _, name := range []string{"legacy_timeout.json", "record.json", "record_zero.json", "without_retrieval.json", "with_retrieval.json"} {
		raw, err := os.ReadFile("../../internal/runlog/testdata/golden/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			Task   json.RawMessage `json:"task"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		var task map[string]json.RawMessage
		if err := json.Unmarshal(record.Task, &task); err != nil {
			t.Fatal(err)
		}
		delete(task, "prompt")
		delete(task, "dir")
		delete(task, "timeout_ns")
		options, err := json.Marshal(task)
		if err != nil {
			t.Fatal(err)
		}
		var o JobOptions
		if err := json.Unmarshal(options, &o); err != nil {
			t.Fatalf("%s options: %v", name, err)
		}
		if len(record.Result) != 0 {
			if _, err := decodeResult(record.Result, name == "record_zero.json"); err != nil {
				t.Fatalf("%s result: %v", name, err)
			}
		}
	}
}

func testCheckpoint(t *testing.T, raw string) Checkpoint {
	t.Helper()
	var cp Checkpoint
	if err := json.Unmarshal([]byte(raw), &cp); err != nil {
		t.Fatal(err)
	}
	return cp
}

func TestCheckpointStartFramePreservesBytes(t *testing.T) {
	raw := `{ "version": 1, "sequence": 1, "boundary": "model_intent", "task_digest": "d", "request": { "html": "<x>" } }`
	cp := testCheckpoint(t, raw)
	frame, err := workerStart(Job{}, "a", "dir", &cp)
	if err != nil || !bytes.Contains(frame, []byte(raw)) {
		t.Fatalf("frame %s: %v", frame, err)
	}
	var output bytes.Buffer
	client := workerClient{input: strings.NewReader(`{"v":1,"seq":1,"type":"hello"}` + "\n"), output: &output, start: frame}
	_, _ = client.run()
	if !bytes.Contains(output.Bytes(), []byte(raw)) {
		t.Fatalf("start changed checkpoint: %s", output.Bytes())
	}
}
