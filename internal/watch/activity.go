package watch

import (
	"os"
	"path/filepath"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// An unreadable timestamp is not evidence that a writer stopped.
func quietSince(last time.Time) time.Time {
	if runlog.Active(last) {
		return time.Time{}
	}
	return last
}

func runStopped(rec runlog.Record) time.Time {
	if rec.Done {
		return time.Time{}
	}
	return quietSince(runlog.LastWrite(rec.ID))
}

// batchUnfinished describes the saved state, independently of its liveness.
func batchUnfinished(res batch.Result) bool {
	for _, t := range res.Tasks {
		if t.Status == batch.StatusRunning {
			return true
		}
	}
	return res.Duration == 0
}

func batchStopped(res batch.Result) time.Time {
	if !batchUnfinished(res) || !idRE.MatchString(res.ID) {
		return time.Time{}
	}
	info, err := os.Stat(filepath.Join(runlog.Home(), "batches", res.ID+".json"))
	if err != nil {
		return time.Time{}
	}
	last := info.ModTime()
	for _, t := range res.Tasks {
		for _, a := range t.Attempts {
			if at := runlog.LastWrite(a.RunID); at.After(last) {
				last = at
			}
		}
	}
	return quietSince(last)
}

func batchGoing(res batch.Result) bool {
	return batchUnfinished(res) && batchStopped(res).IsZero()
}

func noWord(last time.Time) string {
	return "no word since " + last.Local().Format("15:04")
}
