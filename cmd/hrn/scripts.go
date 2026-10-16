package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/scripthint"
)

const scriptsUsage = `usage: hrn scripts hint    (a Claude Code PreToolUse hook: reads the payload on stdin)`

// maxHintPayload caps stdin; a larger payload gets no hint.
const maxHintPayload = 1 << 20

func cmdScripts(args []string) (int, error) {
	if len(args) != 1 || args[0] != "hint" {
		// Not 2: Claude Code reads exit 2 from a PreToolUse hook as a block.
		return 1, fmt.Errorf("%s", scriptsUsage)
	}
	return scriptsHint(os.Stdin, os.Stdout, time.Now())
}

// scriptsHint prints the hook's added context when a kept script covers the
// command's inline script, and otherwise nothing; it always exits 0.
func scriptsHint(in io.Reader, out io.Writer, now time.Time) (int, error) {
	data, err := io.ReadAll(io.LimitReader(in, maxHintPayload+1))
	if err != nil || len(data) > maxHintPayload {
		return 0, nil
	}
	h, ok := scripthint.Find(data)
	if !ok {
		return 0, nil
	}
	out.Write(scripthint.Context(h))
	scripthint.Record(filepath.Join(runlog.Home(), "script-hints.jsonl"), h, now)
	return 0, nil
}
