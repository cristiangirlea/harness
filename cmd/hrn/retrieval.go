package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/report"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func runRetrieval(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: hrn retrieval fixture|compare [--manifest file --samples file --baseline-preparation file --candidate-preparation file --reuse N --runs --json]")
	}
	fs := flag.NewFlagSet("retrieval", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "held-out manifest")
	samples := fs.String("samples", "", "sample cells with results or run references")
	basePrep := fs.String("baseline-preparation", "", "baseline preparation accounting JSON")
	candidatePrep := fs.String("candidate-preparation", "", "candidate preparation accounting JSON")
	reuse := fs.Int("reuse", 1, "explicit preparation reuse count")
	asJSON := fs.Bool("json", false, "JSON comparison evidence")
	runs := fs.Bool("runs", false, "load attempts from run_refs under HRN_HOME (automatic for measured inputs)")
	if e := fs.Parse(args[1:]); e != nil {
		return e
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	var m eval.RetrievalManifest
	var cells []eval.RetrievalSample
	var bp, cp report.Overhead
	var resolvedRuns bool
	switch args[0] {
	case "fixture":
		var err error
		m, cells, err = eval.RetrievalFixture()
		if err != nil {
			return err
		}
		zero := int64(0)
		cost := 0.0
		bp = report.Overhead{ModelTokens: &zero, CostUSD: &cost}
		cp = bp
	case "compare":
		if e := readRetrievalJSON(*manifest, &m); e != nil {
			return e
		}
		if e := readRetrievalJSON(*samples, &cells); e != nil {
			return e
		}
		if *basePrep != "" {
			if e := readRetrievalJSON(*basePrep, &bp); e != nil {
				return e
			}
		}
		if *candidatePrep != "" {
			if e := readRetrievalJSON(*candidatePrep, &cp); e != nil {
				return e
			}
		}
		if *runs || m.EvidenceKind == "measured" {
			if e := resolveRetrievalRuns(m, cells); e != nil {
				return e
			}
			resolvedRuns = true
		}
	default:
		return fmt.Errorf("unknown retrieval command")
	}
	result, err := eval.CompareRetrieval(m, cells, bp, cp, *reuse)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	_, err = fmt.Fprintf(out, "Evidence: %s\nGate: %s — %s\nCells: %d/%d; comparable usage: %d/%d\nSuccess: baseline %d/%d, candidate %d/%d\nMedian tokens: baseline %s, candidate %s; reduction %s\nPreparation coverage (tokens/cost): baseline %t/%t, candidate %t/%t\nSupported full-workflow token/cost savings: %t/%t\n", result.EvidenceKind, result.Gate, result.Reason, result.ObservedPairs, result.ExpectedPairs, result.ComparablePairs, result.ExpectedPairs, result.Baseline.Successes, result.Baseline.Tasks, result.Candidate.Successes, result.Candidate.Tasks, numberPtr(result.BaselineMedian), numberPtr(result.CandidateMedian), numberPtr(result.MedianReduction), result.Baseline.PreparationTokensKnown, result.Baseline.PreparationCostKnown, result.Candidate.PreparationTokensKnown, result.Candidate.PreparationCostKnown, result.TokenSavingsSupported, result.CostSavingsSupported)
	if err == nil && resolvedRuns {
		err = writeRetrievalModels(out, m, cells)
	}
	return err
}

func writeRetrievalModels(out io.Writer, m eval.RetrievalManifest, cells []eval.RetrievalSample) error {
	if labels := retrievalModelLabels(m, cells); len(labels) != 0 {
		_, err := fmt.Fprintf(out, "Models: %s\n", strings.Join(labels, ", "))
		return err
	}
	return nil
}

func retrievalModelLabels(m eval.RetrievalManifest, cells []eval.RetrievalSample) []string {
	if len(m.Spec.Matrix) == 0 {
		return nil
	}
	requested := m.Spec.Matrix[0].Model // Manifest validation requires the same model in every variant.
	seen := map[string]bool{}
	var labels []string
	for _, cell := range cells {
		for _, attempt := range cell.Usage.Attempts {
			label := requested
			if attempt.Model != "" && attempt.Model != requested {
				label += " (reported " + attempt.Model + ")"
			}
			if !seen[label] {
				seen[label] = true
				labels = append(labels, label)
			}
		}
	}
	return labels
}

// Resolve local records even without --runs for measured inputs. This verifies
// execution identity, not corpus provenance or independently attested checks.
func resolveRetrievalRuns(m eval.RetrievalManifest, cells []eval.RetrievalSample) error {
	if err := m.Validate(); err != nil {
		return err
	}
	batches, unreadable, err := batch.List()
	if err != nil || unreadable != 0 {
		return fmt.Errorf("cannot verify recorded batch attempts: unreadable=%d: %v", unreadable, err)
	}
	type membership struct {
		base string
		task batch.TaskResult
	}
	members := map[string]membership{}
	for _, b := range batches {
		for _, task := range b.Tasks {
			for _, a := range task.Attempts {
				if a.RunID == "" {
					continue
				}
				if _, exists := members[a.RunID]; exists {
					return fmt.Errorf("ambiguous batch run membership")
				}
				members[a.RunID] = membership{b.Base, task}
			}
		}
	}
	used := map[string]bool{}
	for i := range cells {
		s := &cells[i]
		var variant *eval.Variant
		for j := range m.Spec.Matrix {
			v := &m.Spec.Matrix[j]
			name := v.Name
			if name == "" {
				parts := []string{v.Backend}
				if v.Model != "" {
					parts = append(parts, v.Model)
				}
				if v.Effort != "" {
					parts = append(parts, v.Effort)
				}
				name = strings.Join(parts, "-")
			}
			if s.Variant == name {
				variant = v
			}
		}
		if variant == nil || len(s.RunRefs) == 0 {
			return fmt.Errorf("unknown variant or missing run references")
		}
		var original string
		for _, task := range m.Spec.Tasks {
			if task.ID == s.TaskID {
				original = task.Prompt
			}
		}
		if original == "" {
			return fmt.Errorf("unknown task")
		}
		s.Usage.Attempts = nil
		for _, id := range s.RunRefs {
			if used[id] {
				return fmt.Errorf("run %s reused across comparison attempts or cells", id)
			}
			used[id] = true
			r, e := runlog.Load(id)
			if e != nil {
				return fmt.Errorf("measured run %s: %w", id, e)
			}
			if !r.Done || r.ID != id {
				return fmt.Errorf("unfinished or mismatched run %s", id)
			}
			switch r.Result.Status {
			case agent.StatusOK, agent.StatusError, agent.StatusRefused, agent.StatusMaxTurns, agent.StatusTimeout, agent.StatusBudget, agent.StatusCanceled:
			default:
				return fmt.Errorf("run lacks a terminal result status")
			}
			model := r.Task.Model
			if model == "" {
				// Old records have no requested model; keep matching their reported result model.
				model = r.Result.Model
			}
			if r.Backend != variant.Backend || r.Result.Backend != variant.Backend || model != variant.Model || r.Task.Effort != variant.Effort {
				return fmt.Errorf("run backend/model/effort differs from variant")
			}
			prompt := r.Task.OriginalPrompt
			if prompt == "" {
				prompt = r.Task.Prompt
			}
			if prompt != original {
				return fmt.Errorf("run original task differs from manifest")
			}
			revision := r.Task.RetrievalRevision
			if revision != "" && revision != m.Revision {
				return fmt.Errorf("run revision differs from manifest")
			}
			if member, ok := members[id]; ok {
				if member.base != m.Revision {
					return fmt.Errorf("batch revision differs from manifest")
				}
				revision = member.base
				var ids []string
				for _, a := range member.task.Attempts {
					ids = append(ids, a.RunID)
				}
				if !reflect.DeepEqual(ids, s.RunRefs) {
					return fmt.Errorf("comparison omits or reorders recorded batch attempts")
				}
				if member.task.Status == batch.StatusRunning || member.task.Status == "" {
					return fmt.Errorf("unfinished batch task")
				}
				if s.Usage.Passed != (member.task.Status == batch.StatusPass) {
					return fmt.Errorf("sample outcome differs from recorded batch checks")
				}
				for _, a := range member.task.Attempts {
					if a.RunID == id && !reflect.DeepEqual(a.Result, r.Result) {
						return fmt.Errorf("batch and run result differ")
					}
				}
			}
			if revision != m.Revision {
				return fmt.Errorf("run revision differs from manifest")
			}
			pin := r.Result.Retrieval
			if variant.Retrieval == "off" {
				if pin != nil || (r.Task.Retrieval != "" && r.Task.Retrieval != "off") {
					return fmt.Errorf("baseline run has retrieval enabled")
				}
				if _, e := os.Stat(filepath.Join(runlog.Root(), id, "retrieval.json")); !os.IsNotExist(e) {
					return fmt.Errorf("baseline has retrieval metadata or cannot be verified")
				}
			} else {
				if r.Task.Retrieval != variant.Retrieval || pin == nil || pin.Mode != variant.Retrieval || pin.Packet == nil || pin.Failure != "" || variant.RetrievalVersion == "" || pin.Packet.VersionID != variant.RetrievalVersion {
					return fmt.Errorf("candidate run lacks enabled retrieval with manifest version pin")
				}
				want := retrieval.Scope{TenantID: r.Task.RetrievalTenant, ProjectID: r.Task.RetrievalProject, Backend: variant.Backend, Model: variant.Model, Revision: m.Revision}
				if want.TenantID == "" || want.ProjectID == "" || r.Task.RetrievalRevision != m.Revision || pin.Scope != want || pin.OriginalPrompt != original || (r.Task.RetrievalVersion != "" && r.Task.RetrievalVersion != variant.RetrievalVersion) {
					return fmt.Errorf("candidate run retrieval scope mismatch")
				}
				if e := retrieval.ValidatePacket(*pin.Packet, variant.RetrievalVersion, ""); e != nil {
					return e
				}
				var saved retrieval.Record
				if e := readRetrievalJSON(filepath.Join(runlog.Root(), id, "retrieval.json"), &saved); e != nil {
					return e
				}
				if !reflect.DeepEqual(&saved, pin) {
					return fmt.Errorf("candidate persisted retrieval differs from result")
				}
			}
			s.Usage.Attempts = append(s.Usage.Attempts, r.Result)
		}
	}
	return nil
}
func numberPtr(v *float64) string {
	if v == nil {
		return "undefined"
	}
	return fmt.Sprintf("%.4g", *v)
}
