package agent

import (
	"strings"
	"testing"
)

// FuzzParseSSE feeds arbitrary event streams to the parser. Anything the API
// could send, or a proxy could mangle, must come back as a value or an error,
// never as a panic in the middle of a run.
func FuzzParseSSE(f *testing.F) {
	f.Add(strings.Join(textTurn("hello"), "\n\n"))
	f.Add(strings.Join(toolTurn("toolu_1", "read_file", `{"path":"a"}`), "\n\n"))
	f.Add("event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	f.Add("data: not json\n\n")
	f.Add("")

	f.Fuzz(func(t *testing.T, stream string) {
		// The callbacks are where a malformed block would reach the renderer.
		resp, _, err := parseSSE(strings.NewReader(stream),
			func(kind, text string) {},
			func(bl block) { _ = bl["type"] })
		if err != nil {
			return
		}
		if resp == nil {
			t.Fatal("a stream that parsed must return a response")
		}
		u := resp.Usage
		if u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadInputTokens < 0 || u.CacheCreationInputTokens < 0 {
			t.Fatalf("negative usage: %+v", u)
		}
	})
}
