package eval

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/purpose"
	"github.com/cristiangirlea/harness/internal/report"
)

const minPassingPairs = 2

// SetupBetter and its peers label verdicts: better, worse, same or inconclusive.
const (
	SetupBetter       = "better"
	SetupWorse        = "worse"
	SetupSame         = "same"
	SetupInconclusive = "inconclusive"
)

// SetupSide aggregates one side's comparison cells.
type SetupSide struct {
	Eval      string  `json:"eval"`
	Cells     int     `json:"cells"`
	Acted     int     `json:"acted"`
	DidNotAct int     `json:"did_not_act,omitempty"`
	Passed    int     `json:"passed"`
	PassRate  float64 `json:"pass_rate"`
	// FailedTokens covers acted failures; TokensPerSuccess charges all acted tokens to passes.
	FailedTokens     int64    `json:"failed_tokens"`
	TokensPerSuccess *float64 `json:"tokens_per_success,omitempty"`
	UnknownTokens    int      `json:"cells_without_tokens,omitempty"`
}

// SetupComparison varies source/branch setup files, keeping tasks, gates and variants fixed.
type SetupComparison struct {
	Source      string   `json:"source"`       // the ref the baseline's setup files came from
	Head        string   `json:"head"`         // the branch commit the candidate ran on
	SetupCommit string   `json:"setup_commit"` // head with the source's setup files
	Paths       []string `json:"paths"`
	Changed     []string `json:"changed"`
	Tasks       []string `json:"tasks"`
	// HeldOut and Seed reproduce each purpose's task split.
	HeldOut      float64   `json:"held_out,omitempty"`
	Seed         *uint64   `json:"seed,omitempty"`
	SourceCommit string    `json:"source_commit"` // where the branch left the source, whose setup files the baseline ran with
	Unpaired     int       `json:"unpaired,omitempty"`
	Baseline     SetupSide `json:"baseline"`
	Candidate    SetupSide `json:"candidate"`
	// PassMovement and TokenMovement measure baseline repeat noise in pass rates and passing tokens.
	PassMovement           *float64 `json:"pass_rate_movement,omitempty"`
	PassingPairs           int      `json:"passing_pairs"`
	PassingBaselineMedian  *float64 `json:"passing_baseline_median_tokens,omitempty"`
	PassingCandidateMedian *float64 `json:"passing_candidate_median_tokens,omitempty"`
	PassingReduction       *float64 `json:"passing_median_token_reduction,omitempty"`
	TokenMovement          *float64 `json:"token_median_movement,omitempty"`
	SavingThreshold        *float64 `json:"saving_threshold,omitempty"`
	Verdict                string   `json:"verdict"` // better, worse, same or inconclusive
	Reason                 string   `json:"reason"`
}

type setupCell struct {
	passed, acted bool
	tokens        int64
	known         bool
	excluded      bool
}

func setupCells(spec Spec, res batch.Result) map[[2]string]setupCell {
	answerOnly := map[string]bool{}
	for _, t := range spec.Tasks {
		answerOnly[t.ID] = t.Expect != "" && len(firstOfList(t.Gate, spec.Gate)) == 0
	}
	out := map[[2]string]setupCell{}
	for _, tr := range res.Tasks {
		task, _, ok := splitID(tr.ID)
		if !ok {
			continue
		}
		repeat := tr.ID[strings.LastIndex(tr.ID, ".r")+2:]
		c := setupCell{passed: tr.Status == batch.StatusPass}
		c.acted = c.passed || answerOnly[task] || !neverActed(tr)
		if n := len(tr.Attempts); n > 0 {
			r := tr.Attempts[n-1].Result
			// Estimates cannot establish measured savings.
			if total, known, estimated := r.ModelTokens(); known && !estimated {
				c.tokens, c.known = total, true
			}
		}
		out[[2]string{task, repeat}] = c
	}
	return out
}

func sideOf(cells map[[2]string]setupCell) SetupSide {
	var s SetupSide
	var total int64
	for _, c := range cells {
		s.Cells++
		if !c.acted {
			s.DidNotAct++
		}
		if c.excluded {
			continue
		}
		s.Acted++
		if !c.known {
			s.UnknownTokens++
		}
		total += c.tokens
		if c.passed {
			s.Passed++
		} else {
			s.FailedTokens += c.tokens
		}
	}
	if s.Acted > 0 {
		s.PassRate = float64(s.Passed) / float64(s.Acted)
	}
	if s.Passed > 0 && s.UnknownTokens == 0 {
		v := float64(total) / float64(s.Passed)
		s.TokensPerSuccess = &v
	}
	return s
}

// CompareSetups pairs task/repeat cells, using gate outcomes instead of judge scores.
func CompareSetups(spec Spec, baseline, candidate batch.Result) SetupComparison {
	b, c := setupCells(spec, baseline), setupCells(spec, candidate)
	// Exclude idle pairs on both sides so abandoning hard tasks cannot improve one pass rate.
	unpaired := 0
	for k, bc := range b {
		cc, ok := c[k]
		if !ok {
			delete(b, k)
			unpaired++
			continue
		}
		if !bc.acted || !cc.acted {
			bc.excluded, cc.excluded = true, true
			b[k], c[k] = bc, cc
		}
	}
	for k := range c {
		if _, ok := b[k]; !ok {
			delete(c, k)
			unpaired++
		}
	}
	out := SetupComparison{Baseline: sideOf(b), Candidate: sideOf(c), Unpaired: unpaired}

	perRepeat := map[string][2]int{}
	baseTokens := map[string][]float64{}
	var passingBase, passingCand []float64
	for k, bc := range b {
		if bc.excluded {
			continue
		}
		pr := perRepeat[k[1]]
		pr[1]++
		if bc.passed {
			pr[0]++
			if bc.known {
				baseTokens[k[1]] = append(baseTokens[k[1]], float64(bc.tokens))
			}
		}
		perRepeat[k[1]] = pr
		if cc := c[k]; bc.passed && cc.passed && bc.known && cc.known {
			out.PassingPairs++
			passingBase = append(passingBase, float64(bc.tokens))
			passingCand = append(passingCand, float64(cc.tokens))
		}
	}
	if len(perRepeat) >= 2 {
		lo, hi := 1.0, 0.0
		for _, pr := range perRepeat {
			rate := float64(pr[0]) / float64(pr[1])
			lo, hi = min(lo, rate), max(hi, rate)
		}
		move := hi - lo
		out.PassMovement = &move
	}
	byRepeat := map[int][]float64{}
	i := 0
	for _, v := range baseTokens {
		byRepeat[i] = v
		i++
	}
	if move, ok := movement(byRepeat); ok {
		threshold := max(0.10, move)
		out.TokenMovement, out.SavingThreshold = &move, &threshold
	}
	if len(passingBase) > 0 {
		x, y := report.Median(passingBase), report.Median(passingCand)
		out.PassingBaselineMedian, out.PassingCandidateMedian = &x, &y
		if x > 0 {
			v := 1 - y/x
			out.PassingReduction = &v
		}
	}

	diff := out.Candidate.PassRate - out.Baseline.PassRate
	switch {
	case out.Baseline.Acted == 0 || out.Candidate.Acted == 0:
		out.Verdict, out.Reason = SetupInconclusive, "a side has no cell that acted"
	case out.PassMovement == nil:
		out.Verdict, out.Reason = SetupInconclusive, "the baseline acted in fewer than two repeats, so its own movement was not measured"
	case diff > *out.PassMovement+1e-12:
		out.Verdict = SetupBetter
		out.Reason = fmt.Sprintf("a higher pass rate (%s → %s), beyond the baseline's own movement of %s between repeats",
			pct(out.Baseline.PassRate), pct(out.Candidate.PassRate), pct(*out.PassMovement))
	case diff < -*out.PassMovement-1e-12:
		out.Verdict = SetupWorse
		out.Reason = fmt.Sprintf("a lower pass rate (%s → %s), beyond the baseline's own movement of %s between repeats",
			pct(out.Baseline.PassRate), pct(out.Candidate.PassRate), pct(*out.PassMovement))
	case out.SavingThreshold == nil:
		out.Verdict, out.Reason = SetupInconclusive, "the pass rates are within the baseline's own movement, but it passed in fewer than two repeats, so the token noise was not measured"
	case out.PassingPairs < minPassingPairs:
		out.Verdict = SetupInconclusive
		out.Reason = fmt.Sprintf("the pass rates are within the baseline's own movement, but only %d pairs passed on both sides, fewer than %d to compare tokens on", out.PassingPairs, minPassingPairs)
	case out.PassingReduction == nil:
		out.Verdict, out.Reason = SetupInconclusive, "the pass rates are within the baseline's own movement, but the baseline's passing median is zero tokens, so there is no saving to measure"
	case *out.PassingReduction >= *out.SavingThreshold-1e-12:
		out.Verdict = SetupBetter
		out.Reason = fmt.Sprintf("pass rates within the baseline's own movement and %s fewer median tokens over passing pairs, above the %s threshold",
			pct(*out.PassingReduction), pct(*out.SavingThreshold))
	case *out.PassingReduction <= -*out.SavingThreshold+1e-12:
		out.Verdict = SetupWorse
		out.Reason = fmt.Sprintf("pass rates within the baseline's own movement and %s more median tokens over passing pairs, beyond the %s threshold",
			pct(-*out.PassingReduction), pct(*out.SavingThreshold))
	default:
		out.Verdict = SetupSame
		out.Reason = fmt.Sprintf("pass rates within the baseline's own movement, and the token change (%s) is inside the %s threshold: that movement, or 10%%",
			pct(*out.PassingReduction), pct(*out.SavingThreshold))
	}
	if n := out.Baseline.DidNotAct + out.Candidate.DidNotAct; n > 0 {
		out.Reason += fmt.Sprintf("; %d cells did not act, so their pairs were left out on both sides: re-run them", n)
	}
	if out.Unpaired > 0 {
		out.Reason += fmt.Sprintf("; %d cells ran on one side only and were left out", out.Unpaired)
	}
	return out
}

func pct(v float64) string { return strconv.FormatFloat(math.Round(v*1000)/10, 'f', -1, 64) + "%" }

// HeldOut reproducibly selects a share of each purpose's tasks using seed.
func HeldOut(tasks []batch.Task, seed uint64, fraction float64) []string {
	byPurpose := map[string][]string{}
	var purposes []string
	for _, t := range tasks {
		p := t.Purpose
		if p == "" {
			p = purpose.Classify(t.Prompt).Category
		}
		if byPurpose[p] == nil {
			purposes = append(purposes, p)
		}
		byPurpose[p] = append(byPurpose[p], t.ID)
	}
	slices.Sort(purposes)
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	var out []string
	for _, p := range purposes {
		ids := slices.Clone(byPurpose[p])
		slices.Sort(ids)
		rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
		n := int(math.Round(fraction * float64(len(ids))))
		out = append(out, ids[:min(max(n, 0), len(ids))]...)
	}
	slices.Sort(out)
	return out
}

// Comparisons run every task twice, so only plan backends may be used.
var setupBackends = []string{"claude", "codex"}

// SetupOptions selects baseline setup files and tasks.
type SetupOptions struct {
	Source  string   // a ref, such as origin/main
	Paths   []string // git pathspecs; DefaultSetupPaths when empty
	HeldOut float64  // the share of tasks to run, by purpose; all of them when 0
	Seed    uint64
}

// SetupRun retains evals, batches and worktrees for inspection or cleanup.
type SetupRun struct {
	Comparison                SetupComparison
	Baseline, Candidate       Record
	BaselineRes, CandidateRes batch.Result
}

// RunSetupComparison varies source/branch setup files over identical code.
func RunSetupComparison(ctx context.Context, spec Spec, opts SetupOptions, notify func(batch.Event)) (SetupRun, error) {
	var out SetupRun
	if len(spec.Matrix) != 1 {
		return out, fmt.Errorf("a setup comparison runs one variant, the matrix has %d: the setup is what it compares", len(spec.Matrix))
	}
	plan := strings.Join(setupBackends, " or ")
	for _, t := range spec.Tasks {
		if b := cmp.Or(spec.Matrix[0].Backend, t.Backend, spec.Backend); !slices.Contains(setupBackends, b) {
			return out, fmt.Errorf("task %s runs on backend %q: a setup comparison runs every task twice, so it takes a plan backend (%s), named in the spec", t.ID, b, plan)
		}
	}
	if j := spec.Judge; j != nil && !slices.Contains(setupBackends, j.Backend) {
		return out, fmt.Errorf("judge backend %q: name a plan backend (%s) for the judge, which would otherwise default to the configured one", j.Backend, plan)
	}
	if spec.Repeats < 2 {
		return out, fmt.Errorf("repeats %d: at least 2, or the baseline's own movement between repeats cannot be measured", spec.Repeats)
	}
	if opts.Source == "" {
		return out, errors.New("no setup source: name the ref whose setup files the baseline runs with")
	}
	if opts.HeldOut < 0 || opts.HeldOut > 1 {
		return out, fmt.Errorf("held-out share %v: between 0 and 1", opts.HeldOut)
	}
	paths := opts.Paths
	if len(paths) == 0 {
		paths = batch.DefaultSetupPaths
	}
	if opts.HeldOut > 0 {
		keep := HeldOut(spec.Tasks, opts.Seed, opts.HeldOut)
		spec.Tasks = slices.DeleteFunc(slices.Clone(spec.Tasks), func(t batch.Task) bool { return !slices.Contains(keep, t.ID) })
		if len(spec.Tasks) == 0 {
			return out, fmt.Errorf("a held-out share of %v leaves no task", opts.HeldOut)
		}
	}
	head, err := batch.Head(ctx, spec.Dir)
	if err != nil {
		return out, err
	}
	setup, err := batch.SetupCommit(ctx, spec.Dir, head, opts.Source, paths)
	if err != nil {
		return out, err
	}
	base, cand := spec, spec
	base.Base, cand.Base = setup.Commit, head
	if out.Baseline, out.BaselineRes, err = Run(ctx, base, notify); err != nil {
		return out, fmt.Errorf("baseline: %w", err)
	}
	if out.Candidate, out.CandidateRes, err = Run(ctx, cand, notify); err != nil {
		return out, fmt.Errorf("candidate: %w", err)
	}
	c := CompareSetups(spec, out.BaselineRes, out.CandidateRes)
	c.Source, c.SourceCommit, c.Head, c.SetupCommit, c.Paths, c.Changed = opts.Source, setup.Source, head, setup.Commit, paths, setup.Changed
	for _, t := range spec.Tasks {
		c.Tasks = append(c.Tasks, t.ID)
	}
	if opts.HeldOut > 0 {
		seed := opts.Seed
		c.HeldOut, c.Seed = opts.HeldOut, &seed
	}
	c.Baseline.Eval, c.Candidate.Eval = out.Baseline.ID, out.Candidate.ID
	out.Comparison = c
	out.Candidate.SetupComparison = &c
	return out, save(out.Candidate)
}
