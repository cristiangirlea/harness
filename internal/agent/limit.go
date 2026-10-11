package agent

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A plan's usage limit is the claude or codex CLI saying that the subscription
// it bills to has run out for now. Neither says so in a field of its own, only
// in words, so the words are matched here: one pattern per wording seen in a
// real run, with the time the limit lifts read back where the message gives one.

var (
	limitWords = regexp.MustCompile(`(?i)hit your (?:[\w-]+ )?limit|reached your (?:[\w.-]+ ){1,3}limit|usage limit reached|requires usage credits|limit reached\W+resets`)
	// "reached your ... limit" is also how a context window could be worded.
	notPlanWords = regexp.MustCompile(`(?i)context|token|request size`)

	// claude before its current wording: "Claude AI usage limit reached|1760184000".
	resetUnix = regexp.MustCompile(`(?i)usage limit reached\|(\d{9,11})`)
	// codex: "try again in 2 days 3 hours 4 minutes".
	resetIn   = regexp.MustCompile(`(?i)try again in ((?:\d+\s*(?:days?|hours?|minutes?|mins?|seconds?|secs?|[dhms])\b[\s,]*(?:and\s+)?)+)`)
	durPart   = regexp.MustCompile(`(?i)(\d+)\s*(day|hour|min|sec|d|h|m|s)`)
	monthName = `([A-Za-z]{3,9})\.? (\d{1,2})(?:st|nd|rd|th)?,?`
	// codex: "try again at Oct 16th, 2026 3:29 PM" or "try again at 3:29 PM".
	resetAt = regexp.MustCompile(`(?i)try again at (?:` + monthName + ` (\d{4}),? )?(\d{1,2}):(\d{2})\s*([ap])\.?m`)
	// claude: "resets 10:10pm (Europe/Bucharest)", "resets 1pm", "resets Oct 14, 9am (...)".
	resetClock = regexp.MustCompile(`(?i)resets (?:on )?(?:` + monthName + ` (?:at )?)?(\d{1,2})(?::(\d{2}))?\s*([ap])\.?m(?:\s*\(([^)]+)\))?`)
)

// maxLimitWindow is the furthest ahead a reset time is believed. The longest
// window either plan has is a week; a time beyond that is a misread.
const maxLimitWindow = 8 * 24 * time.Hour

// usageLimit reports whether msg is a CLI saying its plan's usage limit was
// reached, and when the limit lifts. resets is zero when the message gives no
// time, or one that cannot be right: long past, or further off than any plan's
// window. A clock time without a date is the next one after now.
func usageLimit(msg string, now time.Time) (limited bool, resets time.Time) {
	if !limitWords.MatchString(msg) || notPlanWords.MatchString(limitWords.FindString(msg)) {
		return false, time.Time{}
	}
	t := resetTime(msg, now)
	// A minute's grace: the CLI works the time out a moment before hrn reads it.
	if t.Before(now.Add(-time.Minute)) || t.After(now.Add(maxLimitWindow)) {
		return true, time.Time{}
	}
	return true, t
}

func resetTime(msg string, now time.Time) time.Time {
	if m := resetUnix.FindStringSubmatch(msg); m != nil {
		sec, _ := strconv.ParseInt(m[1], 10, 64)
		return time.Unix(sec, 0)
	}
	if m := resetIn.FindStringSubmatch(msg); m != nil {
		var d time.Duration
		for _, p := range durPart.FindAllStringSubmatch(m[1], -1) {
			n, _ := strconv.Atoi(p[1])
			unit := map[string]time.Duration{"day": 24 * time.Hour, "d": 24 * time.Hour, "hour": time.Hour, "h": time.Hour,
				"min": time.Minute, "m": time.Minute, "sec": time.Second, "s": time.Second}[strings.ToLower(p[2])]
			d += time.Duration(n) * unit
		}
		return now.Add(d)
	}
	if m := resetAt.FindStringSubmatch(msg); m != nil {
		year, _ := strconv.Atoi(m[3])
		return clockTime(now, time.Local, m[1], m[2], year, m[4], m[5], m[6])
	}
	if m := resetClock.FindStringSubmatch(msg); m != nil {
		loc := time.Local
		if m[6] != "" {
			// The CLI names the zone; without the zone database (Windows ships
			// none to Go), the computer's own zone is the CLI's own anyway.
			if l, err := time.LoadLocation(m[6]); err == nil {
				loc = l
			}
		}
		return clockTime(now, loc, m[1], m[2], 0, m[3], m[4], m[5])
	}
	return time.Time{}
}

// clockTime builds the time a message names: a clock time on the day it gives,
// or, without one, the next such time after now. A date without a year is the
// next such date.
func clockTime(now time.Time, loc *time.Location, monthText, dayText string, year int, hourText, minText, ampm string) time.Time {
	hour, _ := strconv.Atoi(hourText)
	minute, _ := strconv.Atoi(minText)
	if hour < 1 || hour > 12 || minute > 59 {
		return time.Time{}
	}
	hour %= 12
	if strings.EqualFold(ampm, "p") {
		hour += 12
	}
	local := now.In(loc)
	if monthText == "" {
		t := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
		// Within the grace of now is now: the limit has lifted, not moved a day.
		if t.Before(now.Add(-10 * time.Minute)) {
			t = t.AddDate(0, 0, 1)
		}
		return t
	}
	month := monthOf(monthText)
	day, _ := strconv.Atoi(dayText)
	if month == 0 || day < 1 || day > 31 {
		return time.Time{}
	}
	if year != 0 {
		return time.Date(year, month, day, hour, minute, 0, 0, loc)
	}
	t := time.Date(local.Year(), month, day, hour, minute, 0, 0, loc)
	if t.Before(now.Add(-24 * time.Hour)) {
		t = t.AddDate(1, 0, 0)
	}
	return t
}

func monthOf(s string) time.Month {
	if len(s) < 3 {
		return 0
	}
	for m := time.January; m <= time.December; m++ {
		if strings.EqualFold(s[:3], m.String()[:3]) {
			return m
		}
	}
	return 0
}
