package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/retrieval"
	"github.com/jackc/pgx/v5"
)

var knowledgeID = regexp.MustCompile(`^[a-z]_[0-9a-f]{32,64}$`)
var exactWords = regexp.MustCompile(`[A-Za-z0-9_./:\\-]+`)
var knowledgeIdentifier = regexp.MustCompile(`^[A-Za-z0-9_./:\\-]+( +[A-Za-z0-9_./:\\-]+)*$`)
var knowledgeToken = regexp.MustCompile(`\bakia[a-z0-9]{16}\b|\beyj[a-z0-9_-]+\.[a-z0-9_-]+\.[a-z0-9_-]+\b`)
var ErrRetrievalBusy = errors.New("retrieval capacity unavailable")

func knowledgeQueryIdentifiers(query string) []string {
	words := []string{}
	seen := map[string]bool{}
	spans := exactWords.FindAllStringIndex(query, -1)
	for i, start := range spans {
		for j := i; j < len(spans); j++ {
			end := spans[j]
			if end[1]-start[0] > retrieval.MaxIdentifierBytes {
				break
			}
			if j > i && strings.Trim(query[spans[j-1][1]:end[0]], " ") != "" {
				break
			}
			word := query[start[0]:end[1]]
			// Preserve literal identifiers while also dropping sentence punctuation.
			for _, candidate := range []string{word, strings.TrimRight(word, ".:")} {
				if candidate != "" && !seen[candidate] {
					seen[candidate] = true
					words = append(words, candidate)
				}
			}
		}
	}
	return words
}

func digestKnowledge(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func digestKnowledgeText(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])
}
func unsafeKnowledgeFrame(v string) bool {
	v = strings.ToLower(v)
	for _, marker := range []string{"<project-reference", "</project-reference>", "[reference ", "[/reference]"} {
		if strings.Contains(v, marker) {
			return true
		}
	}
	return false
}
func unsafeKnowledgeSource(e retrieval.Entry) bool {
	for _, text := range append([]string{e.ID, e.Key, e.Content, e.Source, e.Evidence}, e.Identifiers...) {
		if unsafeKnowledgeText(text) {
			return true
		}
	}
	return false
}
func unsafeKnowledgeText(text string) bool {
	if unsafeKnowledgeFrame(text) {
		return true
	}
	text = strings.ToLower(strings.ReplaceAll(text, `\`, "/"))
	for _, private := range []string{".codex/auth.json", ".ssh/", ".aws/credentials", "credentials.json"} {
		if strings.Contains(text, private) {
			return true
		}
	}
	for _, segment := range strings.FieldsFunc(text, func(r rune) bool {
		return r == '/' || r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '"' || r == '\'' || r == '`'
	}) {
		if segment == ".env" || (strings.HasPrefix(segment, ".env.") && len(segment) > len(".env.")) {
			return true
		}
	}
	for _, secret := range []string{"-----begin private key-----", "-----begin rsa private key-----", "-----begin openssh private key-----", "-----begin ec private key-----", "-----begin encrypted private key-----", "sk-ant-", "ghp_", "github_pat_", "authorization: bearer ", "ignore previous instructions", "disable permission checks"} {
		if strings.Contains(text, secret) {
			return true
		}
	}
	return knowledgeToken.MatchString(text)
}
func validKnowledgeScope(s retrieval.Scope) bool {
	return knowledgeID.MatchString(s.TenantID) && knowledgeID.MatchString(s.ProjectID) && refPattern.MatchString(s.Revision) && len(s.Backend) > 0 && len(s.Backend) <= 64 && len(s.Model) > 0 && len(s.Model) <= 128
}

// Every knowledge operation is admitted separately from the queue pool. No
// knowledge transaction locks project/job rows used by claims or heartbeats.
func (s *Store) knowledgeContext(ctx context.Context) (context.Context, func(), error) {
	select {
	case s.retrievalSlots <- struct{}{}:
	default:
		return nil, nil, ErrRetrievalBusy
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	return ctx, func() { cancel(); <-s.retrievalSlots }, nil
}
func knowledgeProject(ctx context.Context, tx pgx.Tx, sc retrieval.Scope) error {
	if !validKnowledgeScope(sc) {
		return ErrNotFound
	}
	var found bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE tenant_id=$1 AND id=$2)`, sc.TenantID, sc.ProjectID).Scan(&found); err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ImportKnowledge(ctx context.Context, in retrieval.Import) (string, error) {
	for _, text := range []string{in.TenantID, in.ProjectID, in.Backend, in.Model, in.Revision, in.Origin, in.ReviewedBy, in.ReviewEvidence} {
		if unsafeKnowledgeText(text) {
			return "", ErrUnsupported
		}
	}
	if !validKnowledgeScope(in.Scope) || in.Origin != "reviewed_source" || strings.TrimSpace(in.ReviewedBy) == "" || strings.TrimSpace(in.ReviewEvidence) == "" || len(in.ReviewedBy) > 256 || len(in.ReviewEvidence) > 2048 || unsafeKnowledgeFrame(in.ReviewEvidence) || len(in.Entries) > 100 || len(in.Entries) == 0 {
		return "", ErrUnsupported
	}
	// Canonical IDs and order make retries (including concurrent imports) idempotent.
	entries := append([]retrieval.Entry(nil), in.Entries...)
	seen := map[string]bool{}
	for i := range entries {
		e := &entries[i]
		if unsafeKnowledgeSource(*e) || e.Key == "" || len(e.Key) > retrieval.MaxEntryKeyBytes || seen[e.Key] || len(e.Content) == 0 || len(e.Content) > 16384 || len(e.Source) == 0 || len(e.Source) > 512 || len(e.Evidence) == 0 || len(e.Evidence) > 1024 || len(e.Identifiers) > retrieval.MaxEntryIdentifiers || !utf8.ValidString(e.Content) {
			return "", ErrUnsupported
		}
		for _, id := range e.Identifiers {
			if len(id) == 0 || len(id) > retrieval.MaxIdentifierBytes || !knowledgeIdentifier.MatchString(id) {
				return "", ErrUnsupported
			}
		}
		seen[e.Key] = true
		e.Identifiers = append([]string{}, e.Identifiers...)
		sort.Strings(e.Identifiers)
		e.ID = ""
		e.ID = "e_" + digestKnowledge(*e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	in.Entries = entries
	id := "v_" + digestKnowledge(in)
	ctx, done, err := s.knowledgeContext(ctx)
	if err != nil {
		return "", err
	}
	defer done()
	tx, err := s.knowledge.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if err = knowledgeProject(ctx, tx, in.Scope); err != nil {
		return "", err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO knowledge_versions(tenant_id,project_id,id,backend,model,revision,reviewed_by,review_evidence) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, in.TenantID, in.ProjectID, id, in.Backend, in.Model, in.Revision, in.ReviewedBy, in.ReviewEvidence)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() > 0 {
		for _, e := range entries {
			_, err = tx.Exec(ctx, `INSERT INTO knowledge_entries(tenant_id,project_id,id,entry_key,content,source,evidence,identifiers) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, in.TenantID, in.ProjectID, e.ID, e.Key, e.Content, e.Source, e.Evidence, e.Identifiers)
			if err != nil {
				return "", err
			}
			_, err = tx.Exec(ctx, `INSERT INTO knowledge_members(tenant_id,project_id,version_id,entry_id,entry_key) VALUES($1,$2,$3,$4,$5)`, in.TenantID, in.ProjectID, id, e.ID, e.Key)
			if err != nil {
				return "", err
			}
		}
	}
	return id, tx.Commit(ctx)
}

func checkKnowledgeVersion(ctx context.Context, tx pgx.Tx, sc retrieval.Scope, id string) error {
	if !knowledgeID.MatchString(id) {
		return ErrNotFound
	}
	var found bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_versions WHERE tenant_id=$1 AND project_id=$2 AND backend=$3 AND model=$4 AND revision=$5 AND id=$6)`, sc.TenantID, sc.ProjectID, sc.Backend, sc.Model, sc.Revision, id).Scan(&found)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

func (s *Store) InspectKnowledge(ctx context.Context, sc retrieval.Scope, id string) (retrieval.Version, error) {
	out := retrieval.Version{Scope: sc, ID: id, Entries: []retrieval.Entry{}}
	ctx, done, err := s.knowledgeContext(ctx)
	if err != nil {
		return out, err
	}
	defer done()
	tx, err := s.knowledge.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if err = knowledgeProject(ctx, tx, sc); err != nil {
		return out, err
	}
	if err = checkKnowledgeVersion(ctx, tx, sc, id); err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `SELECT reviewed_by,review_evidence FROM knowledge_versions WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, sc.TenantID, sc.ProjectID, id).Scan(&out.ReviewedBy, &out.ReviewEvidence)
	if err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT e.id,e.entry_key,e.content,e.source,e.evidence,e.identifiers FROM knowledge_entries e JOIN knowledge_members m ON (e.tenant_id,e.project_id,e.id)=(m.tenant_id,m.project_id,m.entry_id) WHERE m.tenant_id=$1 AND m.project_id=$2 AND m.version_id=$3 ORDER BY e.entry_key COLLATE "C", e.id`, sc.TenantID, sc.ProjectID, id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var e retrieval.Entry
		if err = rows.Scan(&e.ID, &e.Key, &e.Content, &e.Source, &e.Evidence, &e.Identifiers); err != nil {
			return out, err
		}
		out.Entries = append(out.Entries, e)
	}
	return out, rows.Err()
}

func (s *Store) ChangeKnowledge(ctx context.Context, in retrieval.Change) (retrieval.Active, error) {
	var out retrieval.Active
	if in.Operation != "activate" && in.Operation != "disable" && in.Operation != "rollback" {
		return out, ErrUnsupported
	}
	ctx, done, err := s.knowledgeContext(ctx)
	if err != nil {
		return out, err
	}
	defer done()
	tx, err := s.knowledge.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if err = knowledgeProject(ctx, tx, in.Scope); err != nil {
		return out, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO knowledge_active_versions(tenant_id,project_id,backend,model,revision) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, in.TenantID, in.ProjectID, in.Backend, in.Model, in.Revision)
	if err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `SELECT COALESCE(version_id,''),COALESCE(previous_id,''),generation FROM knowledge_active_versions WHERE tenant_id=$1 AND project_id=$2 AND backend=$3 AND model=$4 AND revision=$5 FOR UPDATE`, in.TenantID, in.ProjectID, in.Backend, in.Model, in.Revision).Scan(&out.VersionID, &out.PreviousID, &out.Generation)
	if err != nil {
		return out, err
	}
	if out.Generation != in.ExpectedGeneration {
		return out, ErrConflict
	}
	target := in.VersionID
	if in.Operation == "disable" {
		target = ""
	}
	if in.Operation == "rollback" {
		target = out.PreviousID
		if target == "" {
			return out, ErrNotFound
		}
	}
	if target != "" {
		if err = checkKnowledgeVersion(ctx, tx, in.Scope, target); err != nil {
			return out, err
		}
	} else if in.Operation == "activate" {
		return out, ErrNotFound
	}
	if target == out.VersionID {
		return out, tx.Commit(ctx)
	}
	out.PreviousID, out.VersionID, out.Generation = out.VersionID, target, out.Generation+1
	_, err = tx.Exec(ctx, `UPDATE knowledge_active_versions SET version_id=NULLIF($6,''),previous_id=NULLIF($7,''),generation=$8 WHERE tenant_id=$1 AND project_id=$2 AND backend=$3 AND model=$4 AND revision=$5`, in.TenantID, in.ProjectID, in.Backend, in.Model, in.Revision, out.VersionID, out.PreviousID, out.Generation)
	if err != nil {
		return out, err
	}
	a, _ := json.Marshal(in)
	b, _ := json.Marshal(out)
	_, err = tx.Exec(ctx, `INSERT INTO knowledge_changes(tenant_id,project_id,change,active) VALUES($1,$2,$3,$4)`, in.TenantID, in.ProjectID, a, b)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func (s *Store) SearchKnowledge(ctx context.Context, in retrieval.Search) (retrieval.Packet, error) {
	started := time.Now()
	out := retrieval.Packet{Entries: []retrieval.Entry{}, Ranking: retrieval.RankingVersion, QueryDigest: retrieval.TextDigest(in.Query)}
	if len(in.Query) > 2048 || len(in.RunRef) > 256 || in.Attempt < 0 || in.Attempt > math.MaxInt32 || !utf8.ValidString(in.Query) {
		return out, ErrUnsupported
	}
	ctx, done, err := s.knowledgeContext(ctx)
	if err != nil {
		return out, err
	}
	defer done()
	tx, err := s.knowledge.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if err = knowledgeProject(ctx, tx, in.Scope); err != nil {
		return out, err
	}
	out.VersionID = in.VersionID
	if out.VersionID == "" {
		err = tx.QueryRow(ctx, `SELECT COALESCE(version_id,'') FROM knowledge_active_versions WHERE tenant_id=$1 AND project_id=$2 AND backend=$3 AND model=$4 AND revision=$5`, in.TenantID, in.ProjectID, in.Backend, in.Model, in.Revision).Scan(&out.VersionID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return out, err
		}
	}
	if out.VersionID == "" {
		out.EmptyReason = "disabled_or_unconfigured"
	} else {
		if err = checkKnowledgeVersion(ctx, tx, in.Scope, out.VersionID); err != nil {
			return out, err
		}
		words := knowledgeQueryIdentifiers(in.Query)
		rows, err := tx.Query(ctx, `SELECT e.id,e.entry_key,e.content,e.source,e.evidence,e.identifiers
 FROM knowledge_members m JOIN knowledge_entries e ON (e.tenant_id,e.project_id,e.id)=(m.tenant_id,m.project_id,m.entry_id)
 WHERE m.tenant_id=$1 AND m.project_id=$2 AND m.version_id=$3
 AND (e.identifiers && $5::text[] OR e.search_vector @@ to_tsquery('simple',COALESCE((SELECT string_agg(quote_literal(term), ' | ' ORDER BY term) FROM unnest(tsvector_to_array(to_tsvector('simple',$4))) term),'')))
 ORDER BY (e.identifiers && $5::text[]) DESC, ts_rank_cd(e.search_vector,to_tsquery('simple',COALESCE((SELECT string_agg(quote_literal(term), ' | ' ORDER BY term) FROM unnest(tsvector_to_array(to_tsvector('simple',$4))) term),''))) DESC,e.entry_key COLLATE "C",e.id LIMIT 100`, in.TenantID, in.ProjectID, out.VersionID, in.Query, words)
		if err != nil {
			return out, err
		}
		seen := map[string]bool{}
		for rows.Next() {
			var e retrieval.Entry
			if err = rows.Scan(&e.ID, &e.Key, &e.Content, &e.Source, &e.Evidence, &e.Identifiers); err != nil {
				rows.Close()
				return out, err
			}
			hash := digestKnowledge(e.Content)
			if seen[hash] {
				continue
			}
			seen[hash] = true
			label := fmt.Sprintf("[Reference %s; source=%q; revision=%s; evidence=%q]\n%s\n[/Reference]\n", e.ID, e.Source, in.Revision, e.Evidence, e.Content)
			if len(retrieval.ReferenceBlock(out.Text+label)) > retrieval.MaxPacketBytes {
				continue
			}
			out.Text += label
			out.Entries = append(out.Entries, e)
			if len(out.Entries) == 5 {
				break
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
		if len(out.Entries) == 0 {
			out.EmptyReason = "no_match_or_byte_cap"
		}
	}
	out.Bytes = len(out.Text)
	out.Digest = retrieval.TextDigest(out.Text)
	out.DurationNS = time.Since(started).Nanoseconds()
	out.AuditID, err = newID("r_")
	if err != nil {
		return out, err
	}
	payload, _ := json.Marshal(out)
	_, err = tx.Exec(ctx, `INSERT INTO retrieval_records(id,tenant_id,project_id,version_id,run_ref,attempt,packet) VALUES($1,$2,$3,NULLIF($4,''),$5,$6,$7)`, out.AuditID, in.TenantID, in.ProjectID, out.VersionID, in.RunRef, in.Attempt, payload)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func (s *Store) ActiveKnowledge(ctx context.Context, sc retrieval.Scope) (retrieval.Active, error) {
	var out retrieval.Active
	ctx, done, err := s.knowledgeContext(ctx)
	if err != nil {
		return out, err
	}
	defer done()
	tx, err := s.knowledge.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if err = knowledgeProject(ctx, tx, sc); err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `SELECT COALESCE(version_id,''),COALESCE(previous_id,''),generation FROM knowledge_active_versions WHERE tenant_id=$1 AND project_id=$2 AND backend=$3 AND model=$4 AND revision=$5`, sc.TenantID, sc.ProjectID, sc.Backend, sc.Model, sc.Revision).Scan(&out.VersionID, &out.PreviousID, &out.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return out, err
}
