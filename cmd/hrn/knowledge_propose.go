package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/remote"
	"github.com/cristiangirlea/harness/internal/retrieval"
)

const proposeUsage = "hrn knowledge propose -request base.json -b claude|codex [-run ID]... [-repo host/owner/name] [-purpose P] [-since 72h|2026-11-01] [-limit N] [-o out.json]"

func validateProposalBase(base retrieval.Import) error {
	for _, field := range []struct{ name, value string }{
		{"tenant_id", base.TenantID}, {"project_id", base.ProjectID}, {"backend", base.Backend}, {"model", base.Model}, {"revision", base.Revision},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("base request requires non-empty %s", field.name)
		}
	}
	for _, field := range []struct{ name, value string }{
		{"origin", base.Origin}, {"reviewed_by", base.ReviewedBy}, {"review_evidence", base.ReviewEvidence},
	} {
		if field.value != "" {
			return fmt.Errorf("base request requires empty %s", field.name)
		}
	}
	if len(base.Entries) > 0 {
		return errors.New("base request requires empty entries")
	}
	return nil
}

func activeProposalEntries(ctx context.Context, scope retrieval.Scope) ([]retrieval.Entry, error) {
	c, err := remote.NewFromEnv()
	if err != nil {
		return nil, err
	}
	request := func(path string, body any) ([]byte, error) {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return c.Request(callCtx, http.MethodPost, path, body)
	}
	data, err := request("/v1/knowledge/active", scope)
	if err != nil {
		return nil, err
	}
	var active retrieval.Active
	if err = json.Unmarshal(data, &active); err != nil || active.VersionID == "" {
		return nil, err
	}
	data, err = request("/v1/knowledge/inspect", struct {
		retrieval.Scope
		VersionID string `json:"version_id"`
	}{scope, active.VersionID})
	if err != nil {
		return nil, err
	}
	var version retrieval.Version
	if err = json.Unmarshal(data, &version); err != nil {
		return nil, err
	}
	for i := range version.Entries {
		version.Entries[i].ID = ""
		version.Entries[i].Excerpt = false
	}
	return version.Entries, nil
}

func runKnowledgePropose(args []string, out, log io.Writer) error {
	fs := flag.NewFlagSet("knowledge propose", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	request := fs.String("request", "", "base import request")
	backend := fs.String("b", "", "plan backend")
	repo := fs.String("repo", "", "exact repository")
	purpose := fs.String("purpose", "", "run purpose")
	since := fs.String("since", "", "start date or duration")
	limit := fs.Int("limit", 10, "maximum source runs")
	output := fs.String("o", "", "draft output file")
	var ids admissionRuns
	fs.Var(&ids, "run", "source run ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case *request == "" || fs.NArg() != 0:
		return errors.New("usage: " + proposeUsage)
	case *backend != "claude" && *backend != "codex":
		return errors.New("knowledge propose requires -b claude or codex")
	case *limit < 1 || *limit > 25:
		return errors.New("-limit must be from 1 to 25")
	case len(ids) == 0 && *repo == "":
		return errors.New("knowledge propose requires -run or -repo")
	}
	for _, id := range ids {
		if !admissionRunID.MatchString(id) {
			return fmt.Errorf("invalid -run ID %q", id)
		}
	}
	var draft retrieval.Import
	if err := readRetrievalJSON(*request, &draft); err != nil {
		return err
	}
	if err := validateProposalBase(draft); err != nil {
		return err
	}
	batches, unreadable, err := batch.List()
	if err != nil {
		return err
	}
	if unreadable > 0 {
		return fmt.Errorf("cannot verify gates: %d unreadable batch records", unreadable)
	}
	attempts := proposalAttempts(batches)
	runs, err := proposalRuns(ids, *repo, *purpose, *since, *limit, attempts, log)
	if err != nil {
		return err
	}
	ctx := context.Background()
	active, err := activeProposalEntries(ctx, draft.Scope)
	if err != nil {
		return err
	}
	keys := map[string]bool{}
	for _, e := range active {
		keys[e.Key] = true
	}
	var kept []retrieval.Entry
	var sources []string
	proposed := 0
	drops := map[string]int{}
	for _, r := range runs {
		evidence := proposalEvidence(r, attempts[r.ID])
		reviewID, final, err := proposeReview(ctx, *backend, proposePrompt+evidence)
		if err != nil {
			fmt.Fprintf(log, "skip run %s: review %s failed: %v\n", r.ID, reviewID, err)
			continue
		}
		entries, err := parseProposal(final)
		if err != nil {
			fmt.Fprintf(log, "skip run %s: review %s: %v\n", r.ID, reviewID, err)
			continue
		}
		proposed += len(entries)
		before := len(kept)
		grounding := proposalGrounding(r, attempts[r.ID])
		for i, p := range entries {
			if i >= 5 {
				drops["over 5 entries"] += len(entries) - i
				break
			}
			e, why := buildProposal(r.ID, grounding, p, i+1)
			switch {
			case why != "":
				fmt.Fprintf(log, "drop run %s entry %d: %s\n", r.ID, i+1, why)
			case keys[e.Key]:
				why = "key collision"
			case proposalDuplicate(e.Content, active, kept):
				why = "duplicate content"
			default:
				keys[e.Key] = true
				kept = append(kept, e)
				continue
			}
			drops[why]++
		}
		if len(kept) > before {
			sources = append(sources, r.ID)
		}
	}
	if len(active)+len(kept) > 100 {
		return errors.New("draft exceeds 100 entries")
	}
	if len(kept) > 0 {
		draft.Entries = append(active, kept...)
		if err := writeProposalDraft(draft, *output, out); err != nil {
			return err
		}
	}
	fmt.Fprintf(log, "%d proposed, %d kept, %d dropped\n", proposed, len(kept), proposed-len(kept))
	for _, why := range slices.Sorted(maps.Keys(drops)) {
		fmt.Fprintf(log, "  %s: %d\n", why, drops[why])
	}
	if len(kept) == 0 {
		fmt.Fprintln(log, "No proposal kept. No draft written.")
		return nil
	}
	fmt.Fprintf(log, "Source runs: %s\n", strings.Join(sources, ", "))
	fmt.Fprintln(log, `Read and edit the entries. Set origin to "reviewed_source" and fill in reviewed_by and review_evidence.`)
	fmt.Fprintln(log, "hrn knowledge import FILE")
	fmt.Fprintln(log, "hrn knowledge admit --version <printed id> --spec SPEC -b claude|codex")
	return nil
}

func writeProposalDraft(draft retrieval.Import, path string, out io.Writer) error {
	data, err := json.MarshalIndent(draft, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if path == "" {
		return writeTerminalSafeJSON(out, data)
	}
	// Publish only the complete draft, including when replacing an earlier one.
	f, err := os.CreateTemp(filepath.Dir(path), ".hrn-propose-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
