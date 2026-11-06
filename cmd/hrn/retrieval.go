package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
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
		return fmt.Errorf("usage: hrn retrieval fixture|compare [--manifest file --samples file --baseline-preparation file --candidate-preparation file --reuse N --runs --json]; hrn retrieval samples --manifest FILE --eval ID[:OFFSET] [--eval ...]; hrn retrieval corpus-digest FILE")
	}
	if args[0] == "samples" {
		return runRetrievalSamples(args[1:], out)
	}
	if args[0] == "corpus-digest" {
		return runRetrievalCorpusDigest(args[1:], out)
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
	var reportedModels []string
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
			if e := resolveRetrievalRunsWithModels(m, cells, &reportedModels); e != nil {
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
		err = writeRetrievalModels(out, m, reportedModels)
	}
	return err
}

func runRetrievalCorpusDigest(args []string, out io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: hrn retrieval corpus-digest FILE")
	}
	var corpus map[string]json.RawMessage
	if err := readRetrievalJSON(args[0], &corpus); err != nil {
		return err
	}
	var entries []retrieval.Entry
	if raw, ok := corpus["entries"]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return fmt.Errorf("corpus entries: %w", err)
		}
	}
	if len(entries) == 0 {
		return fmt.Errorf("corpus has no entries")
	}
	rows := make([][3]string, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, [3]string{entry.Key, entry.Source, retrieval.TextDigest(entry.Content)})
	}
	sort.Slice(rows, func(i, j int) bool {
		for k := range rows[i] {
			if rows[i][k] != rows[j][k] {
				return rows[i][k] < rows[j][k]
			}
		}
		return false
	})
	var encoded bytes.Buffer
	enc := json.NewEncoder(&encoded)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rows); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out, retrieval.TextDigest(strings.TrimSuffix(encoded.String(), "\n")))
	return err
}

func runRetrievalSamples(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("retrieval samples", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "held-out manifest")
	var refs []string
	fs.Func("eval", "saved eval ID[:OFFSET] (repeatable)", func(ref string) error {
		refs = append(refs, ref)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || len(refs) == 0 {
		return fmt.Errorf("usage: hrn retrieval samples --manifest FILE --eval ID[:OFFSET] [--eval ...]")
	}
	var m eval.RetrievalManifest
	if err := readRetrievalJSON(*manifest, &m); err != nil {
		return err
	}
	if err := m.Validate(); err != nil {
		return err
	}
	tasks := map[string]bool{}
	for _, task := range m.Spec.Tasks {
		tasks[task.ID] = true
	}
	variants := map[string]eval.Variant{}
	for _, v := range m.Spec.Matrix {
		variants[v.Key()] = v
	}
	type cellKey struct {
		task, variant string
		repeat        int
	}
	seen := map[cellKey]bool{}
	var samples []eval.RetrievalSample
	for _, ref := range refs {
		id, offsetText, hasOffset := strings.Cut(ref, ":")
		offset := 0
		if hasOffset {
			var err error
			offset, err = strconv.Atoi(offsetText)
			if err != nil {
				return fmt.Errorf("eval %s: invalid offset %q", id, offsetText)
			}
		}
		rec, err := eval.LoadRecord(id)
		if err != nil {
			return err
		}
		// Results already contains the ordered rerun batches saved by eval.Run.
		ids := map[string]bool{}
		for _, tr := range rec.Results {
			if ids[tr.ID] {
				return fmt.Errorf("cell %s: duplicate result", tr.ID)
			}
			ids[tr.ID] = true
			if _, _, _, err := retrievalCellParts(rec.ID, tr.ID); err != nil {
				return err
			}
		}
		for _, tr := range rec.ScoredResults() {
			task, variant, n, err := retrievalCellParts(rec.ID, tr.ID)
			if err != nil {
				return err
			}
			v, ok := variants[variant]
			if !tasks[task] || !ok {
				return fmt.Errorf("cell %s: unknown manifest task or variant", tr.ID)
			}
			if n < 1 || n > m.Spec.Repeats || offset < 0 || offset > m.Spec.Repeats-n {
				return fmt.Errorf("cell %s: repeat outside manifest range", tr.ID)
			}
			repeat := offset + n - 1
			key := cellKey{task, variant, repeat}
			if seen[key] {
				return fmt.Errorf("cell %s: duplicate comparison cell", tr.ID)
			}
			seen[key] = true
			switch tr.Status {
			case batch.StatusPass, batch.StatusFail, batch.StatusError, batch.StatusCanceled:
			default:
				return fmt.Errorf("cell %s: unfinished task status %q", tr.ID, tr.Status)
			}
			zero := int64(0)
			cost := 0.0
			sample := eval.RetrievalSample{TaskID: task, Variant: variant, Repeat: repeat,
				RunRefs: []string{}, IsolationFailure: tr.Commit != "",
				Usage: report.TaskUsage{Passed: tr.Status == batch.StatusPass, Attempts: []agent.Result{},
					Online: report.Overhead{ModelTokens: &zero, CostUSD: &cost}}}
			for _, attempt := range tr.Attempts {
				if attempt.RunID == "" {
					return fmt.Errorf("cell %s: empty attempt run ID", tr.ID)
				}
				sample.RunRefs = append(sample.RunRefs, attempt.RunID)
				if v.Retrieval != "off" {
					var saved retrieval.Record
					if err := readRetrievalJSON(filepath.Join(runlog.Root(), attempt.RunID, "retrieval.json"), &saved); err != nil {
						return fmt.Errorf("cell %s: %w", tr.ID, err)
					}
					sample.Usage.Online.DurationNS += saved.DurationNS
				}
			}
			samples = append(samples, sample)
		}
	}
	for _, task := range m.Spec.Tasks {
		for _, variant := range m.Spec.Matrix {
			for r := 0; r < m.Spec.Repeats; r++ {
				if !seen[cellKey{task.ID, variant.Key(), r}] {
					return fmt.Errorf("cell %s.%s.r%d: missing comparison cell (repeat %d)", task.ID, variant.Key(), r+1, r)
				}
			}
		}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(samples)
}

func retrievalCellParts(evalID, cell string) (string, string, int, error) {
	parts := strings.Split(cell, ".")
	if len(parts) == 5 && strings.HasPrefix(parts[4], "u") {
		round, err := strconv.Atoi(parts[4][1:])
		if err == nil && round > 0 {
			parts = parts[:4]
		}
	}
	if len(parts) != 4 || parts[0] != evalID || !strings.HasPrefix(parts[3], "r") {
		return "", "", 0, fmt.Errorf("cell %s: invalid cell ID or eval prefix", cell)
	}
	n, err := strconv.Atoi(parts[3][1:])
	if err != nil {
		return "", "", 0, fmt.Errorf("cell %s: invalid repeat", cell)
	}
	return parts[1], parts[2], n, nil
}

func writeRetrievalModels(out io.Writer, m eval.RetrievalManifest, reportedModels []string) error {
	if labels := retrievalModelLabels(m, reportedModels); len(labels) != 0 {
		_, err := fmt.Fprintf(out, "Models: %s\n", strings.Join(labels, ", "))
		return err
	}
	return nil
}

func retrievalModelLabels(m eval.RetrievalManifest, reportedModels []string) []string {
	if len(m.Spec.Matrix) == 0 {
		return nil
	}
	requested := m.Spec.Matrix[0].Model // Manifest validation requires the same model in every variant.
	seen := map[string]bool{}
	var labels []string
	for _, reported := range reportedModels {
		label := requested
		if reported != "" && reported != requested {
			label += " (reported " + reported + ")"
		}
		if !seen[label] {
			seen[label] = true
			labels = append(labels, label)
		}
	}
	return labels
}

// Resolve local records even without --runs for measured inputs. This verifies
// execution identity, not corpus provenance or independently attested checks.
func resolveRetrievalRuns(m eval.RetrievalManifest, cells []eval.RetrievalSample) error {
	return resolveRetrievalRunsWithModels(m, cells, nil)
}

func resolveRetrievalRunsWithModels(m eval.RetrievalManifest, cells []eval.RetrievalSample, reportedModels *[]string) error {
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
				// A run without a requested model matches on its reported result model.
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
			attempt := r.Result
			attempt.Model = variant.Model
			s.Usage.Attempts = append(s.Usage.Attempts, attempt)
			if reportedModels != nil {
				*reportedModels = append(*reportedModels, r.Result.Model)
			}
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
