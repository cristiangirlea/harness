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
		// A request rate, not a plan: it passes in a minute and switching
		// backends for it would be noise.
		"429: You've hit your rate limit. Try again in 1 minute.",
		"You've reached your per-minute request limit.",
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

func TestAClockTimeJustPassedIsNotTomorrow(t *testing.T) {
	now := time.Date(2026, 10, 11, 22, 15, 0, 0, time.Local)
	limited, resets := usageLimit("You've hit your session limit · resets 10:10pm", now)
	if !limited || !resets.IsZero() {
		t.Errorf("five minutes after the reset: limited %v, resets %v; want limited with no time to remember", limited, resets)
	}
	_, resets = usageLimit("You've hit your session limit · resets 10:00pm", now)
	if want := time.Date(2026, 10, 12, 22, 0, 0, 0, time.Local); !resets.Equal(want) {
		t.Errorf("a quarter of an hour after: resets %v, want tomorrow %v", resets, want)
	}
}

// agyQuotaMessage is the captured agy message, error_id faked.
const agyQuotaMessage = "error: Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 167h4m51s.\n" +
	`AGY_ERROR: {"short_error":"RESOURCE_EXHAUSTED (code 429): Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 167h4m51s.","status":"RESOURCE_EXHAUSTED","error_code":429,"code_kind":"http","retryable":true,"error_id":"00000000-fake"}`

func TestUsageLimitReadsAgysErrorObject(t *testing.T) {
	now := time.Date(2026, 10, 16, 10, 57, 45, 0, time.UTC)
	limited, resets := usageLimit(agyQuotaMessage, now)
	if !limited {
		t.Fatal("agy's RESOURCE_EXHAUSTED 429 should read as a usage limit")
	}
	if want := now.Add(167*time.Hour + 4*time.Minute + 51*time.Second); !resets.Equal(want) {
		t.Errorf("resets = %v, want %v", resets, want)
	}
	// The object decides, not its words: other statuses are errors.
	for _, msg := range []string{
		`AGY_ERROR: {"short_error":"PERMISSION_DENIED (code 403): Resets in 1h.","status":"PERMISSION_DENIED","error_code":403}`,
		`AGY_ERROR: {"short_error":"RESOURCE_EXHAUSTED (code 500): Resets in 1h.","status":"RESOURCE_EXHAUSTED","error_code":500}`,
		`AGY_ERROR: not json`,
	} {
		if limited, _ := usageLimit(msg, now); limited {
			t.Errorf("%q is not agy's usage limit", msg)
		}
	}
	if limited, resets := usageLimit(`AGY_ERROR: {"short_error":"RESOURCE_EXHAUSTED (code 429): quota reached.","status":"RESOURCE_EXHAUSTED","error_code":429}`, now); !limited || !resets.IsZero() {
		t.Errorf("without a reset: limited %v, resets %v; want limited with no time", limited, resets)
	}
	// agy retries, so another error may come before the quota one.
	retried := `AGY_ERROR: {"short_error":"UNAVAILABLE (code 503): try again.","status":"UNAVAILABLE","error_code":503,"retryable":true}` + "\n" + agyQuotaMessage
	if limited, resets := usageLimit(retried, now); !limited || resets.IsZero() {
		t.Errorf("a 503 before the quota error: limited %v, resets %v; want limited with a time", limited, resets)
	}
}
