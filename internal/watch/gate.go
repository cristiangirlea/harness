package watch

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
)

// gateCommandTools are the command tools whose results hrn writes on a routed run; claude's hook
// answers are read to stop a run but not recorded, so its results carry none of the gate's record.
var gateCommandTools = map[string]string{"api": "run_command", "codex": "shell"}

// gateStopped says hrn stopped a gated run because a command may have run outside the gate.
func gateStopped(rec runlog.Record) bool {
	return strings.HasPrefix(rec.Result.Error, agent.GateStopped)
}

// gateFact says whether a run's commands went through the gate, and the key its audit lines carry.
// Every other error that starts "gate: " stopped the run before its first command.
func gateFact(rec runlog.Record) fact {
	routed := rec.Backend == "api" || rec.Backend == "codex" || rec.Backend == "claude"
	switch {
	case !rec.Task.Gate:
		return fact{Label: "Commands", Value: "ungated"}
	case !routed || strings.HasPrefix(rec.Result.Error, "gate: ") && !gateStopped(rec):
		return fact{Label: "Commands", Value: "ungated · " + tools.GateName + " was asked for, and the run ended before any command reached it"}
	}
	value := "through " + tools.GateName
	switch {
	case rec.Backend != "claude":
		value += " · audit run_id " + rec.ID
	case rec.Result.Session != "":
		value += " · audit session " + rec.Result.Session
	}
	if gateStopped(rec) {
		value += " · stopped: a command may have run ungated"
	}
	return fact{Label: "Commands", Value: value}
}

// gateNote is the gate's record of one command result on a routed run, read before masking so
// the lines hrn appends are whole. codex names its own shell's results like the gate tool's, so a
// codex run hrn stopped for using that shell shows none.
func gateNote(rec runlog.Record, e agent.Event) string {
	tool := gateCommandTools[rec.Backend]
	if !rec.Task.Gate || e.Kind != agent.EventToolResult || tool == "" || e.Tool != tool || rec.Backend == "codex" && gateStopped(rec) {
		return ""
	}
	r, ok := tools.ReadGateResult(e.Text)
	if !ok {
		return ""
	}
	parts := []string{r.By + ": " + r.Decision}
	for _, part := range []string{r.Rule, r.Exit} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if r.Original != "" && rec.Task.ShapeTools && heldOriginal(rec.ID, r.Original, r.OriginalBytes) {
		parts = append(parts, "shaped from "+size(r.OriginalBytes))
	}
	return strings.Join(parts, " · ")
}

// heldOriginal checks the run's originals store, since a command's own output could print an original's line.
func heldOriginal(id, handle string, n int) bool {
	info, err := os.Stat(filepath.Join(runlog.Root(), id, "originals", handle+".txt"))
	return err == nil && info.Mode().IsRegular() && info.Size() == int64(n)
}
