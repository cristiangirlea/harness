package eval

import "testing"

func TestAdmitLesson(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	base := SetupComparison{
		Baseline:     SetupSide{Acted: 10, Passed: 8, PassRate: .8, FailedTokens: 500},
		Candidate:    SetupSide{Acted: 10, Passed: 8, PassRate: .8, FailedTokens: 900},
		PassMovement: f(0), TokenMovement: f(0), PassingPairs: 8,
		PassingBaselineMedian: f(100), PassingCandidateMedian: f(90), PassingReduction: f(.1),
	}
	tests := []struct {
		name   string
		change func(*SetupComparison)
		want   string
	}{
		{"higher pass rate", func(c *SetupComparison) { c.Candidate.PassRate = .9 }, LessonAdmitted},
		{"lower pass rate despite savings", func(c *SetupComparison) { c.Candidate.PassRate = .7 }, LessonRejected},
		{"equal and ten percent cheaper", func(c *SetupComparison) {}, LessonAdmitted},
		{"equal and five percent cheaper", func(c *SetupComparison) { c.PassingReduction = f(.05) }, LessonRejected},
		{"overlapping pass variance", func(c *SetupComparison) { c.Candidate.PassRate = .7; c.PassMovement = f(.1) }, LessonPending},
		{"overlapping token variance", func(c *SetupComparison) { c.TokenMovement = f(.08); c.PassingReduction = f(.05) }, LessonPending},
		{"no successes", func(c *SetupComparison) { c.Baseline.Passed = 0; c.PassingPairs = 0 }, LessonPending},
		{"zero repeats", func(c *SetupComparison) { c.PassMovement = nil }, LessonPending},
		{"missing token variance", func(c *SetupComparison) { c.TokenMovement = nil }, LessonPending},
		{"too few paired successes", func(c *SetupComparison) { c.PassingPairs = 1 }, LessonPending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			tt.change(&c)
			got := AdmitLesson(c)
			if got.Verdict != tt.want || got.BaselineFailedTokens != 500 || got.CandidateFailedTokens != 900 {
				t.Fatalf("got %+v, want %s with failed token costs 500 and 900", got, tt.want)
			}
		})
	}
}
