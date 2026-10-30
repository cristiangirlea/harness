package eval

import (
	"encoding/json"
	"github.com/cristiangirlea/harness/internal/report"
	"math"
	"os"
	"testing"
)

func TestRetrievalRequiresMatchingLeanAndShaping(t *testing.T) {
	for name, edit := range map[string]func(*Variant){
		"lean true":   func(v *Variant) { v.Lean = new(bool); *v.Lean = true },
		"lean false":  func(v *Variant) { v.Lean = new(bool) },
		"shape_tools": func(v *Variant) { v.ShapeTools = true },
	} {
		t.Run(name, func(t *testing.T) {
			m, _, err := RetrievalFixture()
			if err != nil {
				t.Fatal(err)
			}
			edit(&m.Spec.Matrix[1])
			if err := m.Validate(); err == nil {
				t.Fatal("different settings accepted")
			}
		})
	}
}

func TestSyntheticComparisonCannotClaimRealModelSavings(t *testing.T) {
	var m RetrievalManifest
	var samples []RetrievalSample
	b, e := os.ReadFile("testdata/retrieval-manifest.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &m); e != nil {
		t.Fatal(e)
	}
	b, e = os.ReadFile("testdata/retrieval-synthetic-samples.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &samples); e != nil {
		t.Fatal(e)
	}
	zero := int64(0)
	cost := 0.0
	prep := report.Overhead{ModelTokens: &zero, CostUSD: &cost}
	compare := func() RetrievalComparison {
		t.Helper()
		c, e := CompareRetrieval(m, samples, prep, prep, 30)
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	c := compare()
	if c.Gate != "pass" || c.ComparablePairs != 30 || c.Baseline.KnownTokens != 30000 || c.Candidate.KnownTokens != 27000 {
		t.Fatalf("synthetic fixture %#v", c)
	}
	samples[1].Usage.Attempts[0].Usage.Status = "missing"
	c = compare()
	if c.Gate == "pass" || c.ComparablePairs != 29 {
		t.Fatal("missing accepted")
	}
	samples[1].Usage.Attempts[0].Usage.Status = "estimated"
	if compare().Gate == "pass" {
		t.Fatal("estimated accepted")
	}
	samples[1].Usage.Attempts[0].Usage.Status = "reported"
	samples[1].Usage.Passed = false
	c = compare()
	if c.Gate != "reject" {
		t.Fatal("regression accepted")
	}
	if *c.Candidate.TokensPerSuccess != 27000.0/29 {
		t.Fatal("failed usage dropped from numerator")
	}
	samples[1].Usage.Passed = true
	samples[1].IsolationFailure = true
	if compare().Gate != "reject" {
		t.Fatal("isolation accepted")
	}
	samples[1].IsolationFailure = false
	samples = samples[:len(samples)-1]
	if compare().Gate != "pending" {
		t.Fatal("incomplete comparison accepted")
	}
	for i := range samples {
		samples[i].Usage.Passed = false
	}
	c = compare()
	if c.Baseline.TokensPerSuccess != nil || c.Candidate.CostPerSuccess != nil {
		t.Fatal("zero success defined")
	}
	samples = append(samples, samples[0])
	if _, e = CompareRetrieval(m, samples, prep, prep, 1); e == nil {
		t.Fatal("duplicate sample accepted")
	}
}

func TestRetrievalUsesReductionOfMedians(t *testing.T) {
	m, s, err := RetrievalFixture()
	if err != nil {
		t.Fatal(err)
	}
	for i := range s {
		r := &s[i].Usage.Attempts[0]
		r.OutputTokens = 0
		r.CacheRead = 0
		cell := (i / 2) % 3
		if s[i].Variant == "off" {
			r.InputTokens = []int64{100, 100, 1000}[cell]
		} else {
			r.InputTokens = []int64{90, 900, 900}[cell]
		}
	}
	z := int64(0)
	c := 0.0
	p := report.Overhead{ModelTokens: &z, CostUSD: &c}
	out, err := CompareRetrieval(m, s, p, p, 1)
	if err != nil {
		t.Fatal(err)
	}
	if out.Gate == "pass" || *out.BaselineMedian != 100 || *out.CandidateMedian != 900 || *out.MedianPairedSaving < 0.099 {
		t.Fatalf("ratio median incorrectly gated: %+v", out)
	}
}
func TestRetrievalRejectsInvalidAccounting(t *testing.T) {
	for _, mutate := range []func(*RetrievalSample, *report.Overhead){
		func(s *RetrievalSample, p *report.Overhead) { s.Usage.Attempts[0].InputTokens = -1 },
		func(s *RetrievalSample, p *report.Overhead) { s.Usage.Attempts[0].CostUSD = math.NaN() },
		func(s *RetrievalSample, p *report.Overhead) { s.Usage.Attempts[0].InputTokens = math.MaxInt64 },
		func(s *RetrievalSample, p *report.Overhead) { v := math.Inf(1); p.CostUSD = &v },
		func(s *RetrievalSample, p *report.Overhead) { v := int64(-1); s.Usage.Online.ModelTokens = &v },
		func(s *RetrievalSample, p *report.Overhead) { v := int64(math.MaxInt64); p.ModelTokens = &v },
	} {
		m, s, e := RetrievalFixture()
		if e != nil {
			t.Fatal(e)
		}
		p := report.Overhead{}
		mutate(&s[0], &p)
		if _, e = CompareRetrieval(m, s, p, report.Overhead{}, 1); e == nil {
			t.Fatal("invalid accounting accepted")
		}
	}
	m, s, _ := RetrievalFixture()
	m.EvidenceKind = "measured"
	m.Spec.Matrix[1].RetrievalVersion = "v_fixture"
	out, e := CompareRetrieval(m, s, report.Overhead{}, report.Overhead{}, 1)
	if e != nil {
		t.Fatal(e)
	}
	if out.TokenSavingsSupported || out.CostSavingsSupported || out.Baseline.PreparationTokensKnown {
		t.Fatal("missing preparation accepted")
	}
}

func TestRetrievalVariantsKeepTheirModesWhenExpanded(t *testing.T) {
	m, _, e := RetrievalFixture()
	if e != nil {
		t.Fatal(e)
	}
	expanded, e := Expand(m.Spec, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	if expanded.Tasks[0].Retrieval != "off" || expanded.Tasks[3].Retrieval != "required" {
		t.Fatal("matrix lost retrieval options")
	}
}

func TestRetrievalSuppliedEvidenceCannotBecomeMeasuredSavings(t *testing.T) {
	m, s, err := RetrievalFixture()
	if err != nil {
		t.Fatal(err)
	}
	m.EvidenceKind = "measured"
	m.Spec.Matrix[1].RetrievalVersion = "v_fixture"
	z := int64(0)
	c := 0.0
	p := report.Overhead{ModelTokens: &z, CostUSD: &c}
	out, err := CompareRetrieval(m, s, p, p, 1)
	if err != nil {
		t.Fatal(err)
	}
	if out.EvidenceKind != "supplied_unverified" || out.Gate != "pending" || out.TokenSavingsSupported || out.CostSavingsSupported {
		t.Fatalf("unverified claims: %+v", out)
	}
	s[1].RunRefs = s[0].RunRefs
	if _, err = CompareRetrieval(m, s, p, p, 1); err == nil {
		t.Fatal("reused runs accepted")
	}
	m.Spec.Matrix[1].Retrieval = "off"
	if err = m.Validate(); err == nil {
		t.Fatal("off candidate accepted")
	}
}

func TestRetrievalMissingAndPartialEvidenceCannotSupportClaims(t *testing.T) {
	for _, status := range []string{"missing", "partial", ""} {
		m, s, _ := RetrievalFixture()
		m.EvidenceKind = "measured"
		m.Spec.Matrix[1].RetrievalVersion = "v_fixture"
		if status == "" {
			s[1].Usage.Attempts[0].Usage = nil
		} else {
			s[1].Usage.Attempts[0].Usage.Status = status
		}
		z := int64(0)
		c := 0.0
		p := report.Overhead{ModelTokens: &z, CostUSD: &c}
		out, e := CompareRetrieval(m, s, p, p, 1)
		if e != nil {
			t.Fatal(e)
		}
		if out.Gate == "pass" || out.CostSavingsSupported || out.TokenSavingsSupported || out.ComparablePairs != 29 {
			t.Fatal("partial usage accepted")
		}
	}
}

// shapeRetrieval gives every fixture cell the tokens and pass state cell returns for it.
func shapeRetrieval(t *testing.T, cell func(s RetrievalSample) (int64, bool)) RetrievalComparison {
	t.Helper()
	m, s, err := RetrievalFixture()
	if err != nil {
		t.Fatal(err)
	}
	for i := range s {
		r := &s[i].Usage.Attempts[0]
		r.OutputTokens, r.CacheRead = 0, 0
		r.InputTokens, s[i].Usage.Passed = cell(s[i])
	}
	z := int64(0)
	c := 0.0
	p := report.Overhead{ModelTokens: &z, CostUSD: &c}
	out, err := CompareRetrieval(m, s, p, p, 1)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRetrievalSavingsAreMeasuredOverPassingPairsOnly(t *testing.T) {
	failing := map[string]bool{}
	out := shapeRetrieval(t, func(s RetrievalSample) (int64, bool) {
		fails := len(failing) < 16 || failing[s.TaskID+string(rune('0'+s.Repeat))]
		if fails {
			failing[s.TaskID+string(rune('0'+s.Repeat))] = true
		}
		switch {
		case fails && s.Variant == "off":
			return 1000, false
		case fails:
			return 100, false // the candidate fails as often, but cheaply
		case s.Variant == "off":
			return 1000, true
		}
		return 990, true
	})
	if out.Gate == "pass" {
		t.Fatalf("cheap failures counted as savings: %+v", out)
	}
	if out.PassingPairs != 14 || out.PassingBaselineMedian == nil || *out.PassingBaselineMedian != 1000 || *out.PassingCandidateMedian != 990 {
		t.Errorf("passing pairs = %d, medians %v %v; want 14 pairs at 1000 and 990", out.PassingPairs, out.PassingBaselineMedian, out.PassingCandidateMedian)
	}
}

func TestRetrievalSavingsMustExceedTheBaselinesOwnMovementBetweenRepeats(t *testing.T) {
	baseline := []int64{1000, 1300, 1000} // the baseline's median moves 27% between repeats
	for _, c := range []struct {
		name  string
		ratio float64
		pass  bool
	}{{"below the movement", 0.8, false}, {"above the movement", 0.6, true}} {
		t.Run(c.name, func(t *testing.T) {
			out := shapeRetrieval(t, func(s RetrievalSample) (int64, bool) {
				n := baseline[s.Repeat]
				if s.Variant != "off" {
					n = int64(float64(n) * c.ratio)
				}
				return n, true
			})
			if (out.Gate == "pass") != c.pass {
				t.Errorf("gate = %s (%s), want pass %v", out.Gate, out.Reason, c.pass)
			}
			if out.BaselineMovement == nil || math.Abs(*out.BaselineMovement-300.0/1100) > 1e-9 || out.SavingThreshold == nil || *out.SavingThreshold != *out.BaselineMovement {
				t.Errorf("movement = %v, threshold = %v; want 300/1100 for both", out.BaselineMovement, out.SavingThreshold)
			}
		})
	}
}

func TestRetrievalGivesNoSavingVerdictWhenTheNoiseWasNotMeasured(t *testing.T) {
	out := shapeRetrieval(t, func(s RetrievalSample) (int64, bool) {
		if s.Repeat > 0 {
			return 1000, false // only one repeat passes, on both sides
		}
		if s.Variant == "off" {
			return 1000, true
		}
		return 100, true
	})
	if out.Gate == "pass" || out.BaselineMovement != nil {
		t.Errorf("gate = %s, movement = %v; one passing repeat measures no noise", out.Gate, out.BaselineMovement)
	}
}
