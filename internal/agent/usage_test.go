package agent

import (
	"encoding/json"
	"testing"
)

func TestUsageMetadataCountsCacheTokensOnceAndKeepsOldRecordsReadable(t *testing.T) {
	for _, tc := range []struct {
		backend string
		want    int64
		known   bool
	}{{"codex", 110, true}, {"api", 190, true}, {"claude", 190, true}, {"gemini", 0, false}} {
		r := Result{Backend: tc.backend, InputTokens: 100, OutputTokens: 10, CacheRead: 80}
		n, k, _ := r.ModelTokens()
		if n != tc.want || k != tc.known {
			t.Fatalf("%s %d %v", tc.backend, n, k)
		}
	}
	var old Result
	if err := json.Unmarshal([]byte(`{"backend":"codex","input_tokens":100,"output_tokens":10,"cache_read_tokens":80}`), &old); err != nil {
		t.Fatal(err)
	}
	if n, k, _ := old.ModelTokens(); !k || n != 110 {
		t.Fatal("legacy usage")
	}
	old.Usage = &UsageMetadata{Status: "missing", InputSemantics: "inclusive_cache"}
	if _, k, _ := old.ModelTokens(); k {
		t.Fatal("explicit missing ignored")
	}
}
