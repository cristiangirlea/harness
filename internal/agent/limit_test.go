package agent

import (
	"testing"
	"time"
)

func TestUsageLimitReadsTheWordsTheCLIsUse(t *testing.T) {
	// Local is pinned so that a reset time without a zone is read the same way
	// on every machine.
	loc := time.FixedZone("EEST", 3*3600)
	saved := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = saved })
	now := time.Date(2026, 10, 11, 1, 26, 58, 0, loc)

	cases := []struct {
		name, msg string
		resets    time.Time // zero: limited, but no time given
	}{
		{"codex at a date",
			"You’ve hit your usage limit. Visit https://chatgpt.com/settings/usage to purchase more credits or try again at Oct 16th, 2026 3:29 PM.",
			time.Date(2026, 10, 16, 15, 29, 0, 0, loc)},
		{"codex at a time later today",
			"You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/settings/usage to purchase more credits or try again at 3:29 AM.",
			time.Date(2026, 10, 11, 3, 29, 0, 0, loc)},
		{"codex at a time that has passed today is tomorrow",
			"You've hit your usage limit. Try again at 1:05 AM.",
			time.Date(2026, 10, 12, 1, 5, 0, 0, loc)},
		{"codex in a duration",
			"You've hit your usage limit. Upgrade to Plus to continue using Codex, or try again in 2 days 3 hours 4 minutes.",
			now.Add(51*time.Hour + 4*time.Minute)},
		{"claude session limit",
			"You've hit your session limit · resets 10:10pm (Europe/Bucharest)",
			time.Date(2026, 10, 11, 22, 10, 0, 0, loc)},
		{"claude weekly limit on the hour",
			"You've hit your weekly limit · resets 1pm (Europe/Bucharest)",
			time.Date(2026, 10, 11, 13, 0, 0, 0, loc)},
		{"claude limit on a named day",
			"You've hit your weekly limit · resets Oct 14, 9am (Europe/Bucharest)",
			time.Date(2026, 10, 14, 9, 0, 0, 0, loc)},
		{"claude at midnight",
			"You've hit your session limit · resets 12am (Europe/Bucharest)",
			time.Date(2026, 10, 12, 0, 0, 0, 0, loc)},
		{"claude's older wording, with the time as a Unix second",
			"Claude AI usage limit reached|1791687600",
			time.Unix(1791687600, 0)},
		{"a model's own limit gives no time",
			"You've reached your Fable limit. Switch to another model, or manage usage credits at claude.ai/settings/usage?from=cc_cli_limit_message, to continue.",
			time.Time{}},
		{"a model that needs credits",
			"Fable 5 requires usage credits. Switch to another model, or manage usage credits at claude.ai/settings/usage?from=cc_cli_limit_message, to continue.",
			time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			limited, resets := usageLimit(c.msg, now)
			if !limited {
				t.Fatalf("%q should read as a usage limit", c.msg)
			}
			if !resets.Equal(c.resets) {
				t.Errorf("resets = %v, want %v", resets, c.resets)
			}
		})
	}
}

func TestUsageLimitLeavesOtherErrorsAlone(t *testing.T) {
	now := time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)
	for _, msg := range []string{
		"",
		"exit status 1",
		"Not logged in · Please run /login",
		"stream error: exceeded retry limit, last status: 429 Too Many Requests",
		"api 429 rate_limit_error: Number of request tokens has exceeded your per-minute rate limit",
		"Prompt is too long",
		"You've reached your maximum context limit for this conversation",
		"error_max_turns",
	} {
		if limited, _ := usageLimit(msg, now); limited {
			t.Errorf("%q is not a plan's usage limit", msg)
		}
	}
}

func TestUsageLimitDropsAResetTimeItCannotTrust(t *testing.T) {
	now := time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)
	for _, msg := range []string{
		// Long past: a misread, not a limit that lifts.
		"Claude AI usage limit reached|1000000000",
		// Further off than any plan's window.
		"You've hit your usage limit. Try again in 40 days.",
	} {
		limited, resets := usageLimit(msg, now)
		if !limited {
			t.Fatalf("%q should still read as a limit", msg)
		}
		if !resets.IsZero() {
			t.Errorf("%q: resets = %v, want none", msg, resets)
		}
	}
}
