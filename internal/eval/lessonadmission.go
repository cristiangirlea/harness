package eval

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"github.com/cristiangirlea/harness/internal/batch"
	"math"
	"slices"
	"strings"
)

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

// RunLessonComparison evaluates identical tasks with each knowledge version pinned.
func RunLessonComparison(ctx context.Context, spec Spec, active, candidate string, heldOut float64, seed uint64, notify func(batch.Event)) (SetupRun, error) {
	var out SetupRun
	if len(spec.Matrix) != 1 {
		return out, fmt.Errorf("a lesson comparison runs one variant, got %d", len(spec.Matrix))
	}
	plan := strings.Join(setupBackends, " or ")
	for _, t := range spec.Tasks {
		if b := cmp.Or(spec.Matrix[0].Backend, t.Backend, spec.Backend); !slices.Contains(setupBackends, b) {
			return out, fmt.Errorf("task %s backend %q: name a plan backend (%s)", t.ID, b, plan)
		}
	}
	if j := spec.Judge; j != nil && !slices.Contains(setupBackends, j.Backend) {
		return out, fmt.Errorf("judge backend %q: name a plan backend (%s)", j.Backend, plan)
	}
	if spec.Repeats < 2 {
		return out, fmt.Errorf("repeats %d: at least 2 required", spec.Repeats)
	}
	if heldOut < 0 || heldOut > 1 || math.IsNaN(heldOut) {
		return out, fmt.Errorf("held-out share %v: between 0 and 1", heldOut)
	}
	if active == "" || candidate == "" || active == candidate {
		return out, errors.New("two distinct knowledge versions are required")
	}
	if heldOut > 0 {
		keep := HeldOut(spec.Tasks, seed, heldOut)
		spec.Tasks = slices.DeleteFunc(slices.Clone(spec.Tasks), func(t batch.Task) bool { return !slices.Contains(keep, t.ID) })
		if len(spec.Tasks) == 0 {
			return out, fmt.Errorf("a held-out share of %v leaves no task", heldOut)
		}
	}
	// Clear more-specific pins so neither variant nor task can override the comparison.
	spec.Matrix = slices.Clone(spec.Matrix)
	spec.Matrix[0].RetrievalVersion = ""
	spec.Tasks = slices.Clone(spec.Tasks)
	for i := range spec.Tasks {
		spec.Tasks[i].RetrievalVersion = ""
	}
	base, cand := spec, spec
	base.Options.RetrievalVersion = active
	cand.Options.RetrievalVersion = candidate
	var err error
	if out.Baseline, out.BaselineRes, err = Run(ctx, base, notify); err != nil {
		return out, fmt.Errorf("baseline: %w", err)
	}
	if out.Candidate, out.CandidateRes, err = Run(ctx, cand, notify); err != nil {
		return out, fmt.Errorf("candidate: %w", err)
	}
	c := CompareSetups(spec, out.BaselineRes, out.CandidateRes)
	for _, t := range spec.Tasks {
		c.Tasks = append(c.Tasks, t.ID)
	}
	if heldOut > 0 {
		c.HeldOut = heldOut
		c.Seed = &seed
	}
	c.Baseline.Eval, c.Candidate.Eval = out.Baseline.ID, out.Candidate.ID
	out.Comparison = c
	out.Candidate.SetupComparison = &c
	return out, save(out.Candidate)
}
