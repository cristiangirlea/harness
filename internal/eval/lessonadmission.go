package eval

// Lesson admission verdicts keep inconclusive candidates inactive.
const (
	LessonAdmitted = "admitted"
	LessonRejected = "rejected"
	LessonPending  = "pending"
)

// LessonVerdict includes failure costs separately from the admission decision.
type LessonVerdict struct {
	Verdict               string
	BaselineFailedTokens  int64
	CandidateFailedTokens int64
}

// AdmitLesson decides from paired setup measurements without using the setup verdict.
func AdmitLesson(c SetupComparison) LessonVerdict {
	out := LessonVerdict{Verdict: LessonPending, BaselineFailedTokens: c.Baseline.FailedTokens, CandidateFailedTokens: c.Candidate.FailedTokens}
	if c.Baseline.Acted == 0 || c.Candidate.Acted == 0 || c.PassMovement == nil {
		return out
	}
	diff := c.Candidate.PassRate - c.Baseline.PassRate
	const epsilon = 1e-12
	if diff > *c.PassMovement+epsilon {
		out.Verdict = LessonAdmitted
		return out
	}
	if diff < -*c.PassMovement-epsilon {
		out.Verdict = LessonRejected
		return out
	}
	if diff < -epsilon || diff > epsilon || c.TokenMovement == nil || c.PassingPairs < minPassingPairs || c.PassingReduction == nil {
		return out
	}
	saving := *c.PassingReduction
	if saving >= max(.10, *c.TokenMovement)-epsilon {
		out.Verdict = LessonAdmitted
	} else if saving < .10-epsilon && saving+*c.TokenMovement < .10-epsilon {
		out.Verdict = LessonRejected
	}
	return out
}
