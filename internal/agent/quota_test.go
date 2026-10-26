package agent

import (
	"reflect"
	"testing"
)

func quotaEvents(t *testing.T, line string) []Event {
	t.Helper()
	var got []Event
	var res Result
	if !parseClaudeStream([]byte(line), func(e Event) { got = append(got, e) }, &res) {
		t.Fatalf("line not read: %s", line)
	}
	return got
}

func TestClaudeRateLimitEventIsRecordedAsTheQuotaNumbersOnly(t *testing.T) {
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1792600000,"rateLimitType":"five_hour","utilization":0.38,` +
		`"unifiedWindows":{"five_hour":{"utilization":0.38,"resetsAt":1792600000},"seven_day":{"utilization":0.2,"resetsAt":1793000000}}},` +
		`"uuid":"u-1","session_id":"s-1"}`
	got := quotaEvents(t, line)
	if len(got) != 1 || got[0].Kind != EventQuota || got[0].Text != "" || got[0].Input != nil {
		t.Fatalf("events = %+v, want one quota event with nothing but the numbers", got)
	}
	want := &Quota{FiveHour: &QuotaWindow{UsedPercent: 38, ResetsAt: 1792600000}, Week: &QuotaWindow{UsedPercent: 20, ResetsAt: 1793000000}}
	if !reflect.DeepEqual(got[0].Quota, want) {
		t.Fatalf("quota = %+v / %+v, want %+v / %+v", got[0].Quota.FiveHour, got[0].Quota.Week, want.FiveHour, want.Week)
	}
}

func TestClaudeRateLimitEventWithoutWindowsFallsBackToItsOwnType(t *testing.T) {
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1793000000,"rateLimitType":"seven_day","utilization":0.91}}`
	got := quotaEvents(t, line)
	if len(got) != 1 || got[0].Quota == nil || got[0].Quota.FiveHour != nil ||
		!reflect.DeepEqual(got[0].Quota.Week, &QuotaWindow{UsedPercent: 91, ResetsAt: 1793000000}) {
		t.Fatalf("events = %+v, want the weekly window from the event's own type", got)
	}
}

func TestClaudeRateLimitEventWithNoUsableWindowIsDropped(t *testing.T) {
	for _, line := range []string{
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"overage","utilization":0.5,"resetsAt":1793000000}}`,
		`{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{"five_hour":{"utilization":-1,"resetsAt":1792600000},"seven_day":{"utilization":0.2,"resetsAt":0}}}}`,
		`{"type":"rate_limit_event","rate_limit_info":"not an object"}`,
	} {
		if got := quotaEvents(t, line); len(got) != 0 {
			t.Errorf("%s: events = %+v, want none", line, got)
		}
	}
}
