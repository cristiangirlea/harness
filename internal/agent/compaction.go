package agent

import (
	"fmt"
	"strconv"
	"strings"
)

const compactionMaxTokens = 4096

const compactionPrompt = `Summarize this conversation so another model can continue the task. Include:
- The task and how success is checked.
- What is done, naming files and commands.
- Findings and approaches that failed.
- The next steps.
Return only the continuation summary, within 4096 tokens. Do not perform the task or call tools.`

// CheckCompactWindow prevents backend limits from being deferred until execution.
func CheckCompactWindow(backend string, window CompactWindow) error {
	if err := window.Check(); err != nil {
		return err
	}
	if window == "" || window == "auto" {
		return nil
	}
	n, _ := strconv.Atoi(string(window))
	minimum := 5 * compactionMaxTokens
	if backend == "claude" {
		minimum = 100000
	}
	if n < minimum {
		return fmt.Errorf("compact_window: the %s backend requires at least %d tokens", backend, minimum)
	}
	return nil
}

type apiCompaction struct {
	k             int
	summary       string
	failedContext int64
}

func (c apiCompaction) messages(prompt string, history []apiMessage) []apiMessage {
	if c.summary == "" {
		return history
	}
	text := prompt + "\n\n<continuation_summary>\n" + c.summary + "\n</continuation_summary>"
	return append([]apiMessage{{Role: "user", Content: []block{{"type": "text", "text": text}}}}, history[c.k:]...)
}

func compactionSummary(msg *apiResponse) (string, error) {
	if msg.StopReason != "end_turn" {
		return "", fmt.Errorf("summary stopped with %q", msg.StopReason)
	}
	var text strings.Builder
	for _, bl := range msg.Content {
		if bl["type"] == "tool_use" {
			return "", fmt.Errorf("summary returned a tool call")
		}
		if bl["type"] == "text" {
			text.WriteString(str(bl["text"]))
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", fmt.Errorf("summary returned no text")
	}
	return strings.TrimSpace(text.String()), nil
}
