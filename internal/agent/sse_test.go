package agent

import (
	"runtime"
	"strings"
	"testing"
)

// sse frames stream events the way the API sends them.
func sse(events []string) string {
	var b strings.Builder
	for _, ev := range events {
		b.WriteString("data: " + ev + "\n\n")
	}
	return b.String()
}

// A parse costs about what its stream holds. The scanner's buffer was a fixed
// 1 MiB per parse, which made FuzzParseSSE allocate gigabytes a second and
// stall on a memory-limited CI runner.
func TestParsingASmallStreamAllocatesLittle(t *testing.T) {
	stream := sse(textTurn("hello"))
	parse := func() {
		if _, _, err := parseSSE(strings.NewReader(stream), func(string, string) {}, func(block) {}); err != nil {
			t.Fatal(err)
		}
	}
	parse() // warm up anything allocated once
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	const n = 50
	for range n {
		parse()
	}
	runtime.ReadMemStats(&after)
	per := (after.TotalAlloc - before.TotalAlloc) / n
	t.Logf("parsing a %d-byte stream allocates %d bytes", len(stream), per)
	if per > 128<<10 {
		t.Fatalf("parsing a %d-byte stream allocated %d bytes each time; want under 128 KiB", len(stream), per)
	}
}

// "content_block":null decodes to a nil block; a delta for it then wrote into
// a nil map and panicked the api loop mid-run.
func TestANullContentBlockIsAnErrorNotAPanic(t *testing.T) {
	for _, delta := range []string{
		`{"type":"text_delta","text":"hi"}`,
		`{"type":"thinking_delta","thinking":"hm"}`,
		`{"type":"signature_delta","signature":"s"}`,
		`{"type":"citations_delta","citation":{"type":"x"}}`,
		`{"type":"input_json_delta","partial_json":"{}"}`,
	} {
		stream := sse([]string{
			`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[]}}`,
			`{"type":"content_block_start","index":0,"content_block":null}`,
			`{"type":"content_block_delta","index":0,"delta":` + delta + `}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_stop"}`,
		})
		_, _, err := parseSSE(strings.NewReader(stream), func(string, string) {}, func(block) {})
		if err == nil || !strings.Contains(err.Error(), "content block 0") {
			t.Errorf("delta %s after a null block: err = %v; want an error naming content block 0", delta, err)
		}
	}
}

func TestALineLongerThanAMegabyteStillParses(t *testing.T) {
	long := strings.Repeat("x", 3<<20)
	resp, _, err := parseSSE(strings.NewReader(sse(textTurn(long))), func(string, string) {}, func(block) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0]["text"] != long {
		t.Fatalf("content = %d block(s); want the one 3 MiB text block whole", len(resp.Content))
	}
}
