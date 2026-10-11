package agent

import (
	"maps"
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

// manyDeltas is a thinking block and a text block, each streamed as n small
// deltas, as a long response arrives.
func manyDeltas(n int) string {
	events := []string{
		messageStart("msg_long"),
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
	}
	for range n {
		events = append(events, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm "}}`)
	}
	events = append(events,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
	for range n {
		events = append(events, `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"word "}}`)
	}
	events = append(events, `{"type":"content_block_stop","index":1}`)
	return sse(append(events, messageEnd("end_turn")...))
}

// A block's deltas are gathered, not added to its string one by one: that
// copied the whole text on every delta, so a long response took time and
// memory quadratic in its length (64,000 deltas: 2.75 s).
func TestAStreamOfManyDeltasAllocatesInProportionToItsLength(t *testing.T) {
	stream := manyDeltas(16000)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	resp, _, err := parseSSE(strings.NewReader(stream), func(string, string) {}, func(block) {})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Content[1]["text"]; got != strings.Repeat("word ", 16000) {
		t.Fatalf("text block holds %d bytes; want the 16000 deltas joined", len(str(got)))
	}
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("parsing a %d-byte stream of 32000 deltas allocates %d bytes", len(stream), alloc)
	if limit := 32 * uint64(len(stream)); alloc > limit {
		t.Fatalf("parsing a %d-byte stream allocated %d bytes; want under %d (32 times the stream; it was 410)", len(stream), alloc, limit)
	}
}

func TestDeltasBuildTheSameBlocksAndCallbacks(t *testing.T) {
	stream := sse(append([]string{
		messageStart("msg_1"),
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"ab"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"cd"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":"Hi "}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"the"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"re"}}`,
		`{"type":"content_block_stop","index":1}`,
	}, messageEnd("end_turn")...))
	var deltas []string
	var stopped []block
	resp, _, err := parseSSE(strings.NewReader(stream),
		func(kind, text string) { deltas = append(deltas, kind+":"+text) },
		// A copy: the block itself is filled in again when the parse ends, which
		// would hide a block that reached onBlock without its text.
		func(b block) { stopped = append(stopped, maps.Clone(b)) })
	if err != nil {
		t.Fatal(err)
	}
	want := []block{
		{"type": "thinking", "thinking": "let me think", "signature": "abcd"},
		{"type": "text", "text": "Hi there"},
	}
	for i, w := range want {
		for k, v := range w {
			if resp.Content[i][k] != v {
				t.Errorf("block %d %s = %q; want %q", i, k, resp.Content[i][k], v)
			}
			if stopped[i][k] != v {
				t.Errorf("block %d at its stop: %s = %q; want %q", i, k, stopped[i][k], v)
			}
		}
	}
	if got := strings.Join(deltas, "|"); got != "thinking:let me |thinking:think|text:the|text:|text:re" {
		t.Errorf("onDelta saw %q", got)
	}
}

func TestABlockCutOffBeforeItsStopKeepsWhatArrived(t *testing.T) {
	stream := sse([]string{
		messageStart("msg_cut"),
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a reply"}}`,
	})
	resp, started, err := parseSSE(strings.NewReader(stream), func(string, string) {}, func(block) {})
	if err == nil || !started {
		t.Fatalf("err = %v, started = %v; want the stream reported cut off after it started", err, started)
	}
	if got := resp.Content[0]["text"]; got != "half a reply" {
		t.Fatalf("text = %q; want what arrived before the cut", got)
	}
}

// A message_start can fail after it has already decoded new content into the
// response. What the earlier blocks gathered must land in those blocks, not in
// whatever now sits at their index, and never past the end of the content.
func TestAMessageStartThatFailsMidStreamIsAnErrorAndLeavesTheNewContentAlone(t *testing.T) {
	for _, next := range []string{
		`{"content":[],"model":1}`,
		`{"content":null,"model":1}`,
		`{"content":[{"text":"Q"}],"model":1}`,
	} {
		stream := sse([]string{
			messageStart("msg_1"),
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`,
			`{"type":"message_start","message":` + next + `}`,
		})
		var resp *apiResponse
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("message_start %s: panic: %v", next, p)
				}
			}()
			resp, _, err = parseSSE(strings.NewReader(stream), func(string, string) {}, func(block) {})
		}()
		if err == nil {
			t.Errorf("message_start %s: no error", next)
		}
		if len(resp.Content) == 1 && resp.Content[0]["text"] != "Q" {
			t.Errorf("message_start %s: text = %q; want the decoded block's own \"Q\"", next, resp.Content[0]["text"])
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
