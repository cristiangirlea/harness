package quota

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/kernel"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestQuotaUsesTheAccountHomeOrRecord(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now()
	t.Setenv("CODEX_HOME", t.TempDir())
	for i, account := range []string{"team-a", "team-b"} {
		home := t.TempDir()
		dir := filepath.Join(home, "sessions", "2026", "01", "01")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		used := 20 + 40*i
		reset := now.Add(time.Hour).Unix()
		line := fmt.Sprintf(`{"timestamp":%q,"payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":%d,"window_minutes":300,"resets_at":%d}}}}`, now.Format(time.RFC3339Nano), used, reset)
		if err := os.WriteFile(filepath.Join(dir, "test.jsonl"), []byte(line), 0600); err != nil {
			t.Fatal(err)
		}
		rec, err := runlog.Create("claude", kernel.Task{Dir: t.TempDir()}, nil)
		if err != nil {
			t.Fatal(err)
		}
		rec.Event(kernel.Event{Time: now, Kind: kernel.EventQuota, Quota: &kernel.Quota{FiveHour: &kernel.QuotaWindow{UsedPercent: float64(used), ResetsAt: reset}}})
		if err := rec.Close(kernel.Result{Backend: "claude", Account: account, Status: kernel.StatusOK}); err != nil {
			t.Fatal(err)
		}
		q := ForAccount("codex", account, home, now)
		if q.FiveHour == nil || q.FiveHour.Left != float64(100-used) {
			t.Fatalf("codex %s: %+v", account, q.FiveHour)
		}
	}
	for account, want := range map[string]float64{"team-a": 80, "team-b": 40} {
		q := ForAccount("claude", account, "", now)
		if q.FiveHour == nil || q.FiveHour.Left != want {
			t.Fatalf("claude %s: %+v", account, q.FiveHour)
		}
	}
	if ForAccount("claude", "unknown", "", now).FiveHour != nil {
		t.Fatal("borrowed another account's quota")
	}
}
