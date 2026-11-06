package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var maskedOriginal = regexp.MustCompile(`(?m)^original: ([0-9a-f]{16}) \(\d+ bytes\); read_original reads it$`)

func maskToolResults(msgs []apiMessage, keep int, describe func(name string, input json.RawMessage) string) []apiMessage {
	if keep <= 0 {
		return msgs
	}
	total := 0
	for _, msg := range msgs {
		if msg.Role == "user" {
			for _, bl := range msg.Content {
				if bl["type"] == "tool_result" {
					total++
				}
			}
		}
	}
	// Advancing by a whole window keeps cached prefixes stable between steps.
	if total/keep < 2 {
		return msgs
	}
	masked := ((total - keep) / keep) * keep
	uses := map[string]block{}
	out := msgs
	seen := 0
	for i, msg := range msgs {
		if msg.Role == "assistant" {
			for _, bl := range msg.Content {
				if bl["type"] == "tool_use" {
					uses[str(bl["id"])] = bl
				}
			}
			continue
		}
		if msg.Role != "user" {
			continue
		}
		copied := false
		for j, bl := range msg.Content {
			if bl["type"] != "tool_result" {
				continue
			}
			seen++
			content, ok := bl["content"].(string)
			if seen > masked || !ok || len(content) <= 256 {
				continue
			}
			use := uses[str(bl["tool_use_id"])]
			input, _ := json.Marshal(use["input"])
			summary := strings.Join(strings.Fields(describe(str(use["name"]), input)), " ")
			ending := "; run the tool again to see it]"
			if match := maskedOriginal.FindStringSubmatch(content); match != nil {
				ending = "; original " + match[1] + ": read_original reads it]"
			}
			replacement := block{"type": bl["type"], "tool_use_id": bl["tool_use_id"],
				"content": fmt.Sprintf("[masked by hrn: %s, %d bytes%s", summary, len(content), ending)}
			if isError, set := bl["is_error"]; set {
				replacement["is_error"] = isError
			}
			if !copied {
				if &out[0] == &msgs[0] {
					out = slices.Clone(msgs)
				}
				out[i].Content = slices.Clone(msg.Content)
				copied = true
			}
			out[i].Content[j] = replacement
		}
	}
	return out
}
