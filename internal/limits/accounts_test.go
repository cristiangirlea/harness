package limits

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/kernel"
)

func TestAccountLimitsKeepOldBackendKey(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now().Truncate(time.Second)
	res := kernel.Result{Status: kernel.StatusLimited, LimitResets: now.Add(time.Hour)}

	old := fmt.Sprintf(`{"codex":{"until":%q,"reason":"old limit"}}`, res.LimitResets.Format(time.RFC3339))
	if err := os.WriteFile(path(), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	res.LimitResets = now.Add(2 * time.Hour)
	if err := NoteAccount("codex", "team-a", res); err != nil {
		t.Fatal(err)
	}
	if got := Until("codex", now); !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("backend limit: %v", got)
	}
	if got := UntilAccount("codex", "team-a", now); !got.Equal(res.LimitResets) {
		t.Fatalf("account limit: %v", got)
	}
	if !UntilAccount("codex", "team-b", now).IsZero() || !UntilAccount("codex", "team-a", res.LimitResets).IsZero() {
		t.Fatal("unrelated or expired limit")
	}
}
