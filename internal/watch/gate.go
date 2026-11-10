package watch

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/mask"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
)

// gateCommandTools are the command tools whose results hrn writes on a routed run; claude's hook
// answers are read to stop a run but not recorded, so its results carry none of the gate's record.
var gateCommandTools = map[string]string{"api": "run_command", "codex": "shell"}

// gateFact says whether a run's commands went through the gate, and the key its audit lines carry.
func gateFact(rec runlog.Record) fact {
	if !rec.Task.Gate {
		return fact{Label: "Commands", Value: "ungated"}
	}
	value := "through " + tools.GateName
	switch {
	case rec.Backend != "claude":
		value += " · audit run_id " + rec.ID
	case rec.Result.Session != "":
		value += " · audit session " + rec.Result.Session
	}
	if strings.HasPrefix(rec.Result.Error, agent.GateStopped) {
		value += " · stopped: a command may have run ungated"
	}
	return fact{Label: "Commands", Value: value}
}

// gateNote is the gate's record of one command result on a routed run, read before masking so
// the lines hrn appends are whole.
func gateNote(rec runlog.Record, e agent.Event) string {
	if !rec.Task.Gate || e.Kind != agent.EventToolResult || gateCommandTools[rec.Backend] == "" || e.Tool != gateCommandTools[rec.Backend] {
		return ""
	}
	r, ok := tools.ReadGateResult(e.Text)
	if !ok {
		return ""
	}
	parts := []string{r.By + ": " + r.Decision}
	for _, part := range []string{r.Rule, r.Exit} {
		if part != "" {
			parts = append(parts, mask.Text(part))
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
