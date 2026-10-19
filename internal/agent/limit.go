package agent

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	limitWords = regexp.MustCompile(`(?i)hit your (?:[\w-]+ )?limit|reached your (?:[\w.-]+ ){1,3}limit|usage limit reached|requires usage credits|limit reached\W+resets`)
	// Context and per-minute limits do not mean a subscription has run out.
	notPlanWords = regexp.MustCompile(`(?i)context|token|request|\brate\b|per-minute`)

	resetUnix = regexp.MustCompile(`(?i)usage limit reached\|(\d{9,11})`)
	// Codex reports relative resets in human duration units.
	resetIn   = regexp.MustCompile(`(?i)try again in ((?:\d+\s*(?:days?|hours?|minutes?|mins?|seconds?|secs?|[dhms])\b[\s,]*(?:and\s+)?)+)`)
	durPart   = regexp.MustCompile(`(?i)(\d+)\s*(day|hour|min|sec|d|h|m|s)`)
	monthName = `([A-Za-z]{3,9})\.? (\d{1,2})(?:st|nd|rd|th)?,?`
	// Codex clock resets omit a zone and may omit a date.
	resetAt = regexp.MustCompile(`(?i)try again at (?:` + monthName + ` (\d{4}),? )?(\d{1,2}):(\d{2})\s*([ap])\.?m`)
	// Claude clocks may name a zone absent from host tzdata.
	resetClock = regexp.MustCompile(`(?i)resets (?:on )?(?:` + monthName + ` (?:at )?)?(\d{1,2})(?::(\d{2}))?\s*([ap])\.?m(?:\s*\(([^)]+)\))?`)

	agyError  = regexp.MustCompile(`AGY_ERROR: (\{.*\})`)
	agyResets = regexp.MustCompile(`Resets in ((?:\d+(?:\.\d+)?(?:h|ms|m|s))+)`)
)

// MaxLimitWindow bounds plausible subscription reset times.
const MaxLimitWindow = 8 * 24 * time.Hour

func usageLimit(msg string, now time.Time) (limited bool, resets time.Time) {
	t, agy := agyLimit(msg, now)
	if !agy && (!limitWords.MatchString(msg) || notPlanWords.MatchString(limitWords.FindString(msg))) {
		return false, time.Time{}
	}
	if !agy {
		t = resetTime(msg, now)
	}
	// A minute's grace: the CLI works the time out a moment before hrn reads it.
	if t.Before(now.Add(-time.Minute)) || t.After(now.Add(MaxLimitWindow)) {
		return true, time.Time{}
	}
	return true, t
}

func agyLimit(msg string, now time.Time) (time.Time, bool) {
	// agy retries, so the quota error may follow others.
	for _, m := range agyError.FindAllStringSubmatch(msg, -1) {
		var e struct {
			ShortError string `json:"short_error"`
			Status     string `json:"status"`
			ErrorCode  int    `json:"error_code"`
		}
		if json.Unmarshal([]byte(m[1]), &e) != nil || e.Status != "RESOURCE_EXHAUSTED" || e.ErrorCode != 429 {
			continue
		}
		if r := agyResets.FindStringSubmatch(e.ShortError); r != nil {
			if d, err := time.ParseDuration(r[1]); err == nil {
				return now.Add(d), true
			}
		}
		return time.Time{}, true
	}
	return time.Time{}, false
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
			// Windows may lack zone data; fall back to the CLI host's zone.
			if l, err := time.LoadLocation(m[6]); err == nil {
				loc = l
			}
		}
		return clockTime(now, loc, m[1], m[2], 0, m[3], m[4], m[5])
	}
	return time.Time{}
}

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
		// A reset within the last ten minutes just expired; do not move it to tomorrow.
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
