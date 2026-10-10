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
		t.Fatalf("parsing a %d-byte stream allocated %d bytes each time; want well under 1 MiB", len(stream), per)
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
