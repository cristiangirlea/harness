package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/remote"
	"github.com/cristiangirlea/harness/internal/retrieval"
)

type admissionRuns []string

func (r *admissionRuns) String() string     { return strings.Join(*r, ",") }
func (r *admissionRuns) Set(s string) error { *r = append(*r, s); return nil }

var admissionRunID = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

func admissionSourceRuns(entries []retrieval.Entry, flags []string) ([]string, error) {
	seen := map[string]bool{}
	for _, id := range flags {
		if !admissionRunID.MatchString(id) {
			return nil, fmt.Errorf("invalid source run %q", id)
		}
		seen[id] = true
	}
	for _, e := range entries {
		for _, id := range strings.FieldsFunc(e.Evidence, func(r rune) bool { return !(unicode.IsDigit(r) && r < 128 || r >= 'a' && r <= 'f' || r == '-') }) {
			if admissionRunID.MatchString(id) {
				seen[id] = true
			}
		}
	}
	if len(seen) == 0 {
		return nil, errors.New("the candidate cites no source runs; pass --source-run")
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	slices.Sort(out)
	return out, nil
}
func runKnowledgeAdmit(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("knowledge admit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	version := fs.String("version", "", "")
	path := fs.String("spec", "", "")
	held := fs.Float64("held-out", 0.3, "")
	seed := fs.Uint64("seed", 0, "")
	backend := fs.String("b", "", "")
	var sources admissionRuns
	fs.Var(&sources, "source-run", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *version == "" || *path == "" {
		return errors.New("knowledge admit requires --version and --spec")
	}
	if *backend != "claude" && *backend != "codex" {
		return errors.New("knowledge admit requires -b claude or codex")
	}
	spec, err := eval.LoadSpec(*path)
	if err != nil {
		return err
	}
	if len(spec.Matrix) != 1 {
		return errors.New("knowledge admit requires one matrix variant")
	}
	spec.Backend = *backend
	spec.Matrix[0].Backend = *backend
	var scope retrieval.Scope
	for i, t := range spec.Tasks {
		opts := t.Options.Over(spec.Options)
		model := spec.Matrix[0].Model
		if model == "" {
			model = opts.Model
		}
		s := retrieval.Scope{TenantID: opts.RetrievalTenant, ProjectID: opts.RetrievalProject, Revision: opts.RetrievalRevision, Backend: *backend, Model: model}
		mode := spec.Matrix[0].Retrieval
		if mode == "" {
			mode = opts.Retrieval
		}
		if mode != "required" {
			return errors.New("knowledge admit requires retrieval required for every task")
		}
		if i == 0 {
			scope = s
		} else if scope != s {
			return errors.New("knowledge admit requires one retrieval scope across all tasks")
		}
	}
	if len(spec.Tasks) == 0 || scope.TenantID == "" || scope.ProjectID == "" || scope.Revision == "" || scope.Model == "" {
		return errors.New("knowledge admit requires a complete retrieval scope in the spec")
	}
	client, err := remote.NewOperatorFromEnv()
	if err != nil {
		return err
	}
	ctx := context.Background()
	data, err := client.Request(ctx, http.MethodPost, "/v1/knowledge/active", scope)
	if err != nil {
		return err
	}
	var active retrieval.Active
	if err = json.Unmarshal(data, &active); err != nil {
		return err
	}
	if active.VersionID == "" {
		return errors.New("no active knowledge version")
	}
	if active.VersionID == *version {
		return errors.New("candidate is already active")
	}
	data, err = client.Request(ctx, http.MethodPost, "/v1/knowledge/inspect", struct {
		retrieval.Scope
		VersionID string `json:"version_id"`
	}{scope, *version})
	if err != nil {
		return err
	}
	var candidate retrieval.Version
	if err = json.Unmarshal(data, &candidate); err != nil {
		return err
	}
	runs, err := admissionSourceRuns(candidate.Entries, sources)
	if err != nil {
		return err
	}
	comparison, err := eval.RunLessonComparison(ctx, spec, active.VersionID, *version, *held, *seed, nil)
	if err != nil {
		return err
	}
	c := comparison.Comparison
	verdict := eval.AdmitLesson(c)
	a := retrieval.Admission{Scope: scope, VersionID: *version, Verdict: verdict.Verdict, BaselineVersionID: active.VersionID, EvalIDs: []string{comparison.Baseline.ID, comparison.Candidate.ID}, PassRateBaseline: c.Baseline.PassRate, PassRateCandidate: c.Candidate.PassRate, FailedAttemptTokensBaseline: float64(verdict.BaselineFailedTokens), FailedAttemptTokensCandidate: float64(verdict.CandidateFailedTokens), SourceRuns: runs}
	if c.PassingBaselineMedian != nil {
		a.MedianTokensBaseline = *c.PassingBaselineMedian
	}
	if c.PassingCandidateMedian != nil {
		a.MedianTokensCandidate = *c.PassingCandidateMedian
	}
	if err = client.RecordAdmission(ctx, a); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(a)
}
