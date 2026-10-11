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
	f.Add("data: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":null}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n")
	f.Add("")
	// A second message_start that fails after shrinking the content, while a
	// block's deltas are still gathered (#71's review): it must stay an error.
	f.Add("data: {\"type\":\"message_start\",\"message\":{}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\"}}\n\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"content\":[],\"model\":1}}\n\n")

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
