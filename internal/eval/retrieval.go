package eval

import (
	"fmt"
	"github.com/cristiangirlea/harness/internal/report"
)

// RetrievalManifest extends the existing eval matrix. It contains no answers or
// corpus entries. A pilot must pin the model/settings and record cache conditions.
type RetrievalManifest struct {
	ID              string `json:"id"`
	EvidenceKind    string `json:"evidence_kind"` // synthetic_fixture or measured
	Revision        string `json:"revision"`
	CorpusDigest    string `json:"corpus_digest"`
	HeldOut         bool   `json:"held_out"`
	CacheConditions string `json:"cache_conditions"`
	Spec            Spec   `json:"spec"`
}
type RetrievalSample struct {
	TaskID           string           `json:"task_id"`
	Variant          string           `json:"variant"`
	Repeat           int              `json:"repeat"`
	RunRefs          []string         `json:"run_refs"`
	Usage            report.TaskUsage `json:"usage"`
	IsolationFailure bool             `json:"isolation_failure"`
}
type RetrievalComparison struct {
	TokenSavingsSupported bool                 `json:"token_savings_supported"`
	BaselineMedian        *float64             `json:"baseline_median_tokens"`
	CandidateMedian       *float64             `json:"candidate_median_tokens"`
	MedianReduction       *float64             `json:"median_token_reduction"`
	CostSavingsSupported  bool                 `json:"cost_savings_supported"`
	EvidenceKind          string               `json:"evidence_kind"`
	ExpectedPairs         int                  `json:"expected_pairs"`
	ObservedPairs         int                  `json:"observed_pairs"`
	ComparablePairs       int                  `json:"comparable_pairs"`
	Baseline              report.WorkflowUsage `json:"baseline"`
	Candidate             report.WorkflowUsage `json:"candidate"`
	MedianPairedSaving    *float64             `json:"median_paired_saving"`
	Gate                  string               `json:"gate"`
	Reason                string               `json:"reason"`
}

func (m RetrievalManifest) Validate() error {
	if m.ID == "" || m.Revision == "" || m.CorpusDigest == "" || !m.HeldOut || m.CacheConditions == "" || (m.EvidenceKind != "synthetic_fixture" && m.EvidenceKind != "measured") || len(m.Spec.Tasks) < 10 || m.Spec.Repeats < 3 || m.Spec.Repeats > 100 || len(m.Spec.Tasks) > 10000 || len(m.Spec.Matrix) != 2 {
		return fmt.Errorf("retrieval comparison requires fixed identity, held-out tasks, two variants, >=10 tasks and >=3 repeats")
	}
	a, b := m.Spec.Matrix[0], m.Spec.Matrix[1]
	if a.Key() == b.Key() || a.Backend == "" || a.Backend != b.Backend || a.Model == "" || a.Model != b.Model || a.Effort != b.Effort {
		return fmt.Errorf("variants must use identical explicit backend/model/effort")
	}
	if a.ChangesHarness() || b.ChangesHarness() {
		return fmt.Errorf("variants must not change the harness: prompt_prefix, append_system_prompt or no_skills would be counted as retrieval")
	}
	if a.Retrieval != "off" || (b.Retrieval != "optional" && b.Retrieval != "required") {
		return fmt.Errorf("comparison requires explicit baseline off and candidate enabled")
	}
	if m.EvidenceKind == "measured" && b.RetrievalVersion == "" {
		return fmt.Errorf("measured candidate requires an explicit retrieval version pin")
	}
	seen := map[string]bool{}
	for _, t := range m.Spec.Tasks {
		if t.ID == "" || seen[t.ID] || (len(t.Gate) == 0 && t.Expect == "") {
			return fmt.Errorf("unique tasks and deterministic checks required")
		}
		seen[t.ID] = true
	}
	_, err := Expand(m.Spec, "retrieval-validation")
	return err
}

// CompareRetrieval operates over existing agent results, never starts a model.
func CompareRetrieval(m RetrievalManifest, samples []RetrievalSample, basePrep, candidatePrep report.Overhead, reuse int) (RetrievalComparison, error) {
	out := RetrievalComparison{EvidenceKind: m.EvidenceKind, ExpectedPairs: len(m.Spec.Tasks) * m.Spec.Repeats, Gate: "pending"}
	if err := m.Validate(); err != nil {
		return out, err
	}
	variants := []string{m.Spec.Matrix[0].Key(), m.Spec.Matrix[1].Key()}
	tasks := map[string]bool{}
	for _, t := range m.Spec.Tasks {
		tasks[t.ID] = true
	}
	type key struct {
		task, variant string
		repeat        int
	}
	indexed := map[key]RetrievalSample{}
	usedRuns := map[string]bool{}
	for _, s := range samples {
		if s.Usage.Passed && (len(s.Usage.Attempts) == 0 || s.Usage.Attempts[len(s.Usage.Attempts)-1].Status != "ok") {
			return out, fmt.Errorf("passing sample lacks a successful final attempt")
		}
		if err := report.ValidateWorkflow([]report.TaskUsage{s.Usage}, report.Overhead{}, reuse); err != nil {
			return out, err
		}
		for _, a := range s.Usage.Attempts {
			if a.Backend != m.Spec.Matrix[0].Backend || a.Model != m.Spec.Matrix[0].Model {
				return out, fmt.Errorf("sample backend/model differs from manifest")
			}
		}
		if m.EvidenceKind == "measured" && len(s.RunRefs) == 0 {
			return out, fmt.Errorf("measured sample needs run references")
		}
		if m.EvidenceKind == "measured" {
			for _, id := range s.RunRefs {
				if id == "" || usedRuns[id] {
					return out, fmt.Errorf("empty or reused measured run reference")
				}
				usedRuns[id] = true
			}
		}
		k := key{s.TaskID, s.Variant, s.Repeat}
		if !tasks[s.TaskID] || (s.Variant != variants[0] && s.Variant != variants[1]) || s.Repeat < 0 || s.Repeat >= m.Spec.Repeats {
			return out, fmt.Errorf("unknown comparison cell")
		}
		if _, ok := indexed[k]; ok {
			return out, fmt.Errorf("duplicate comparison cell")
		}
		indexed[k] = s
	}
	var baselines, candidates []report.TaskUsage
	var savings, baselineTokens, candidateTokens []float64
	regression := false
	for _, t := range m.Spec.Tasks {
		for r := 0; r < m.Spec.Repeats; r++ {
			a, ak := indexed[key{t.ID, variants[0], r}]
			b, bk := indexed[key{t.ID, variants[1], r}]
			if ak {
				baselines = append(baselines, a.Usage)
			}
			if bk {
				candidates = append(candidates, b.Usage)
			}
			if !ak || !bk {
				continue
			}
			out.ObservedPairs++
			regression = regression || b.IsolationFailure || (a.Usage.Passed && !b.Usage.Passed)
			an, ac := taskTokens(a.Usage)
			bn, bc := taskTokens(b.Usage)
			if ac && bc {
				out.ComparablePairs++
				baselineTokens = append(baselineTokens, float64(an))
				candidateTokens = append(candidateTokens, float64(bn))
				if an > 0 {
					savings = append(savings, 1-float64(bn)/float64(an))
				}
			}
		}
	}
	if err := report.ValidateWorkflow(baselines, basePrep, reuse); err != nil {
		return out, err
	}
	if err := report.ValidateWorkflow(candidates, candidatePrep, reuse); err != nil {
		return out, err
	}
	if len(baselineTokens) > 0 {
		a, b := report.Median(baselineTokens), report.Median(candidateTokens)
		out.BaselineMedian = &a
		out.CandidateMedian = &b
		if a > 0 {
			v := 1 - b/a
			out.MedianReduction = &v
		}
	}
	out.Baseline = report.SummarizeWorkflow(baselines, basePrep, reuse)
	out.Candidate = report.SummarizeWorkflow(candidates, candidatePrep, reuse)
	if len(savings) > 0 {
		v := report.Median(savings)
		out.MedianPairedSaving = &v
	}
	switch {
	case regression:
		out.Gate = "reject"
		out.Reason = "deterministic correctness or isolation regression"
	case out.ObservedPairs != out.ExpectedPairs:
		out.Reason = "incomplete comparison coverage"
	case out.Candidate.Successes > out.Baseline.Successes:
		out.Gate = "pass"
		out.Reason = "higher success"
	case out.Candidate.Successes == out.Baseline.Successes && out.Candidate.Successes > 0 && out.ComparablePairs == out.ExpectedPairs && out.MedianReduction != nil && *out.MedianReduction >= 0.10-1e-12:
		out.Gate = "pass"
		out.Reason = "equal success and at least 10 percent reduction between comparable model-token medians"
	default:
		out.Reason = "insufficient comparable usage or improvement"
	}
	// Run identity can be checked by the CLI, but these inputs cannot prove the
	// reviewed corpus, held-out separation, checks or isolation outcomes. Numeric
	// improvement is diagnostic, never supported measured promotion evidence.
	if m.EvidenceKind == "measured" {
		out.EvidenceKind = "supplied_unverified"
		if out.Gate == "pass" {
			out.Gate = "pending"
		}
		out.Reason += "; corpus and check binding unverified; measured savings unsupported"
	}
	if m.EvidenceKind == "synthetic_fixture" {
		out.Reason += "; synthetic fixture, no measured LLM savings"
	}
	return out, nil
}
func taskTokens(t report.TaskUsage) (int64, bool) {
	var n int64
	known := len(t.Attempts) > 0 && t.Online.ModelTokens != nil && !t.Online.Estimated
	if t.Online.ModelTokens != nil {
		n += *t.Online.ModelTokens
	}
	for _, a := range t.Attempts {
		v, k, e := a.ModelTokens()
		n += v
		known = known && k && !e && a.Usage != nil && a.Usage.Status == "reported"
	}
	return n, known
}
