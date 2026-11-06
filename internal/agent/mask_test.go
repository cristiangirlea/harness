package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func maskHistory(n int) []apiMessage {
	var msgs []apiMessage
	for i := range n {
		id := fmt.Sprintf("call-%d", i)
		msgs = append(msgs,
			apiMessage{Role: "assistant", Content: []block{{"type": "tool_use", "id": id, "name": "read_file", "input": map[string]any{"path": id}}}},
			apiMessage{Role: "user", Content: []block{{"type": "tool_result", "tool_use_id": id, "content": strings.Repeat("x", 300)}}})
	}
	return msgs
}

func maskDescription(name string, input json.RawMessage) string { return name + " " + string(input) }

func TestMaskingKeepsResultsWholeBelowTwiceTheWindow(t *testing.T) {
	for _, keep := range []int{0, 1, 2, 3} {
		for n := range 6 {
			if keep > 0 && n >= 2*keep {
				continue
			}
			msgs := maskHistory(n)
			if got := maskToolResults(msgs, keep, maskDescription); !reflect.DeepEqual(got, msgs) {
				t.Fatalf("keep=%d results=%d changed", keep, n)
			}
		}
	}
}

func TestMaskingMeasuresTheShortResultThresholdInBytes(t *testing.T) {
	for _, body := range []string{strings.Repeat("x", 257), strings.Repeat("\u00e9", 129)} {
		msgs := maskHistory(2)
		msgs[1].Content[0]["content"] = body
		got := maskToolResults(msgs, 1, maskDescription)[1].Content[0]["content"].(string)
		if !strings.HasPrefix(got, "[masked by hrn:") || !strings.Contains(got, fmt.Sprintf(", %d bytes;", len(body))) {
			t.Fatalf("note = %q", got)
		}
	}
}

func TestMaskingAdvancesInWindowsAndKeepsThePrefixStable(t *testing.T) {
	for _, keep := range []int{1, 2, 3} {
		var prefix []byte
		for n := 2 * keep; n < 4*keep; n++ {
			got := maskToolResults(maskHistory(n), keep, maskDescription)
			wantMasked := keep
			if n >= 3*keep {
				wantMasked = 2 * keep
			}
			for i := range n {
				masked := strings.HasPrefix(got[2*i+1].Content[0]["content"].(string), "[masked by hrn: ")
				if masked != (i < wantMasked) {
					t.Fatalf("keep=%d results=%d result=%d masked=%v", keep, n, i, masked)
				}
			}
			if n < 3*keep {
				current, _ := json.Marshal(got[:4*keep])
				if prefix != nil && !bytes.Equal(prefix, current) {
					t.Fatal("prefix changed between steps")
				}
				prefix = current
			}
		}
	}
}

func TestMaskingCopiesOnlyChangedMessagesAndPreservesTheInput(t *testing.T) {
	msgs := maskHistory(4)
	msgs[1].Content = append(msgs[1].Content, block{"type": "text", "text": "keep"})
	before, _ := json.Marshal(msgs)
	got := maskToolResults(msgs, 2, maskDescription)
	after, _ := json.Marshal(msgs)
	if !bytes.Equal(before, after) {
		t.Fatal("input changed")
	}
	for i := range msgs {
		if i == 1 || i == 3 {
			if &got[i].Content[0] == &msgs[i].Content[0] {
				t.Fatal("masked content slice was reused")
			}
		} else if &got[i].Content[0] != &msgs[i].Content[0] {
			t.Fatalf("unchanged message %d was copied", i)
		}
	}
	got[1].Content[0]["content"] = "changed again"
	if msgs[1].Content[0]["content"] != strings.Repeat("x", 300) {
		t.Fatal("masked block reused the original map")
	}
	got[1].Content[1] = block{"type": "text", "text": "changed"}
	if msgs[1].Content[1]["text"] != "keep" {
		t.Fatal("masked message reused the original slice")
	}
}

func TestMaskingKeepsResultIDsAndExplicitErrorFlags(t *testing.T) {
	for _, isError := range []any{nil, false, true} {
		msgs := maskHistory(2)
		if isError != nil {
			msgs[1].Content[0]["is_error"] = isError
		}
		got := maskToolResults(msgs, 1, maskDescription)[1].Content[0]
		want := block{"type": "tool_result", "tool_use_id": "call-0", "content": `[masked by hrn: read_file {"path":"call-0"}, 300 bytes; run the tool again to see it]`}
		if isError != nil {
			want["is_error"] = isError
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestMaskingCountsShortAndNonStringResultsButLeavesThemWhole(t *testing.T) {
	for _, content := range []any{"", strings.Repeat("x", 256), []any{block{"type": "text", "text": "unexpected"}}, nil} {
		msgs := maskHistory(6)
		msgs[1].Content[0]["content"] = content
		got := maskToolResults(msgs, 2, maskDescription)
		if !reflect.DeepEqual(got[1], msgs[1]) {
			t.Fatal("short or non-string result changed")
		}
		if got[7].Content[0]["content"] == msgs[7].Content[0]["content"] {
			t.Fatal("unchanged result did not count toward the window")
		}
		if !reflect.DeepEqual(got[9:], msgs[9:]) {
			t.Fatal("newest window changed")
		}
	}
}

func TestMaskingCountsEveryResultBlockInAUserMessage(t *testing.T) {
	msgs := maskHistory(4)
	msgs = []apiMessage{{Role: "assistant", Content: append(msgs[0].Content, msgs[2].Content...)},
		{Role: "user", Content: append(msgs[1].Content, msgs[3].Content...)}, msgs[4], msgs[5], msgs[6], msgs[7]}
	got := maskToolResults(msgs, 2, maskDescription)
	for _, b := range got[1].Content {
		if !strings.HasPrefix(b["content"].(string), "[masked by hrn:") {
			t.Fatal("old result was not masked")
		}
	}
	if !reflect.DeepEqual(got[2:], msgs[2:]) {
		t.Fatal("newest results changed")
	}
}

func TestMaskingCarriesOnlyAnExactOriginalHandleLine(t *testing.T) {
	for _, line := range []string{"original: 0123456789abcdef (9000 bytes); read_original reads it", "prefix original: 0123456789abcdef (9000 bytes); read_original reads it", "original: 0123456789abcdeg (9000 bytes); read_original reads it", "original: 0123456789abcdef (9000 bytes); read_original reads it suffix"} {
		msgs := maskHistory(2)
		body := strings.Repeat("x", 300) + "\n" + line + "\ntail"
		msgs[1].Content[0]["content"] = body
		got := maskToolResults(msgs, 1, maskDescription)[1].Content[0]["content"].(string)
		ending := "; run the tool again to see it]"
		if strings.HasPrefix(line, "original: 0123456789abcdef") && !strings.HasSuffix(line, "suffix") {
			ending = "; original 0123456789abcdef: read_original reads it]"
		}
		if !strings.HasSuffix(got, ending) || !strings.Contains(got, fmt.Sprintf(", %d bytes", len(body))) {
			t.Fatalf("note = %q", got)
		}
	}
}

func TestMaskingUsesTheMatchingEarlierToolUseAndMakesOneLine(t *testing.T) {
	msgs := maskHistory(2)
	msgs[0].Content = append([]block{{"type": "tool_use", "id": "other", "name": "wrong", "input": map[string]any{}}}, msgs[0].Content...)
	got := maskToolResults(msgs, 1, func(name string, input json.RawMessage) string {
		if name != "read_file" || string(input) != `{"path":"call-0"}` {
			t.Fatalf("description input: %s %s", name, input)
		}
		return "read\nfile\rpath"
	})
	if got[1].Content[0]["content"] != "[masked by hrn: read file path, 300 bytes; run the tool again to see it]" {
		t.Fatalf("note = %q", got[1].Content[0]["content"])
	}
}
