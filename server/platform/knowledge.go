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
		for j := i; j < len(spans) && j < i+8; j++ {
			end := spans[j]
			if end[1]-start[0] > KnowledgeMaxIdentifierBytes {
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
					if len(words) == 256 {
						return words
					}
				}
			}
		}
	}
	return words
}

// Plain words such as "a" or "error" are scored as text; only identifier-shaped words bypass the score floor.
func knowledgeShapedIdentifiers(words []string) []string {
	shaped := []string{}
	for _, word := range words {
		inner := len(word) > 1 && strings.IndexFunc(word[1:], func(r rune) bool { return r >= 'A' && r <= 'Z' }) >= 0
		if strings.ContainsAny(word, " _./:\\-0123456789") || inner {
			shaped = append(shaped, word)
		}
	}
	return shaped
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
func unsafeKnowledgeSource(e KnowledgeEntry) bool {
	for _, text := range append([]string{e.ID, e.Key, e.Source}, e.Identifiers...) {
		if unsafeKnowledgePattern(text, true) != "" {
			return true
		}
	}
	return unsafeKnowledgePattern(e.Content, false) != "" || unsafeKnowledgePattern(e.Evidence, false) != ""
}

// Notes may name private paths ("copy .env.example to .env"); only path fields are screened for them.
var knowledgeProseFields = map[string]bool{"content": true, "evidence": true, "review_evidence": true}

func unsafeKnowledgePattern(text string, paths bool) string {
	text = strings.ToLower(text)
	for _, marker := range []string{"<project-reference", "</project-reference>", "[reference ", "[/reference]"} {
		if strings.Contains(text, marker) {
			return marker
		}
	}
	if pattern := unsafeKnowledgePath(text); paths && pattern != "" {
		return pattern
	}
	for _, secret := range []string{"-----begin private key-----", "-----begin rsa private key-----", "-----begin openssh private key-----", "-----begin ec private key-----", "-----begin encrypted private key-----", "sk-ant-", "ghp_", "github_pat_", "authorization: bearer ", "ignore previous instructions", "disable permission checks"} {
		if strings.Contains(text, secret) {
			return secret
		}
	}
	if knowledgeToken.MatchString(text) {
		return knowledgeToken.String()
	}
	return ""
}
func unsafeKnowledgePath(text string) string {
	text = strings.ReplaceAll(text, `\`, "/")
	for _, private := range []string{".codex/auth.json", ".ssh/", ".aws/credentials", "credentials.json"} {
		if strings.Contains(text, private) {
			return private
		}
	}
	for _, segment := range strings.FieldsFunc(text, func(r rune) bool {
		return r == '/' || r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '"' || r == '\'' || r == '`' || r == '=' || r == ',' || r == ';'
	}) {
		if segment == ".env" {
			return ".env"
		}
		if strings.HasPrefix(segment, ".env.") && len(segment) > len(".env.") {
			return ".env.*"
		}
	}
	return ""
}

type knowledgeTextField struct {
	name, text string
}

func knowledgeTextRefusal(fields ...knowledgeTextField) error {
	for _, field := range fields {
		if pattern := unsafeKnowledgePattern(field.text, !knowledgeProseFields[field.name]); pattern != "" {
			return fmt.Errorf("%w: field %s matched pattern %q", ErrUnsupported, field.name, pattern)
		}
	}
	return nil
}
func validKnowledgeScope(s KnowledgeScope) bool {
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
func knowledgeProject(ctx context.Context, tx pgx.Tx, sc KnowledgeScope) error {
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

func (s *Store) ImportKnowledge(ctx context.Context, in KnowledgeImport) (string, error) {
	if err := knowledgeTextRefusal(
		knowledgeTextField{"tenant_id", in.TenantID}, knowledgeTextField{"project_id", in.ProjectID},
		knowledgeTextField{"backend", in.Backend}, knowledgeTextField{"model", in.Model},
		knowledgeTextField{"revision", in.Revision}, knowledgeTextField{"origin", in.Origin},
		knowledgeTextField{"reviewed_by", in.ReviewedBy}, knowledgeTextField{"review_evidence", in.ReviewEvidence},
	); err != nil {
		return "", err
	}
	if !validKnowledgeScope(in.KnowledgeScope) || in.Origin != "reviewed_source" || strings.TrimSpace(in.ReviewedBy) == "" || strings.TrimSpace(in.ReviewEvidence) == "" || len(in.ReviewedBy) > 256 || len(in.ReviewEvidence) > 2048 || unsafeKnowledgeFrame(in.ReviewEvidence) || len(in.Entries) > 100 || len(in.Entries) == 0 {
		return "", ErrUnsupported
	}
	// Canonical IDs and order make retries (including concurrent imports) idempotent.
	entries := append([]KnowledgeEntry(nil), in.Entries...)
	seen := map[string]bool{}
	for i := range entries {
		e := &entries[i]
		if err := knowledgeTextRefusal(
			knowledgeTextField{"id", e.ID}, knowledgeTextField{"key", e.Key},
			knowledgeTextField{"content", e.Content}, knowledgeTextField{"source", e.Source},
			knowledgeTextField{"evidence", e.Evidence},
		); err != nil {
			return "", fmt.Errorf("entries[%d]: %w", i, err)
		}
		for j, id := range e.Identifiers {
			if err := knowledgeTextRefusal(knowledgeTextField{"identifiers", id}); err != nil {
				return "", fmt.Errorf("entries[%d].identifiers[%d]: %w", i, j, err)
			}
		}
		if e.Excerpt || e.Key == "" || len(e.Key) > KnowledgeMaxEntryKeyBytes || seen[e.Key] || len(e.Content) == 0 || len(e.Content) > 16384 || len(e.Source) == 0 || len(e.Source) > 512 || len(e.Evidence) == 0 || len(e.Evidence) > 1024 || len(e.Identifiers) > KnowledgeMaxEntryIdentifiers || !utf8.ValidString(e.Content) {
			return "", ErrUnsupported
		}
		for _, id := range e.Identifiers {
			if len(id) == 0 || len(id) > KnowledgeMaxIdentifierBytes || !knowledgeIdentifier.MatchString(id) {
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
	if err = knowledgeProject(ctx, tx, in.KnowledgeScope); err != nil {
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

func checkKnowledgeVersion(ctx context.Context, tx pgx.Tx, sc KnowledgeScope, id string) error {
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

func (s *Store) InspectKnowledge(ctx context.Context, sc KnowledgeScope, id string) (KnowledgeVersion, error) {
	out := KnowledgeVersion{KnowledgeScope: sc, ID: id, Entries: []KnowledgeEntry{}}
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
	var admission KnowledgeAdmission
	admission.KnowledgeScope, admission.VersionID = sc, id
	err = tx.QueryRow(ctx, `SELECT verdict,baseline_version_id,eval_ids,pass_rate_baseline,pass_rate_candidate,median_tokens_baseline,median_tokens_candidate,failed_attempt_tokens_baseline,failed_attempt_tokens_candidate,source_runs,created_at FROM knowledge_admissions WHERE tenant_id=$1 AND project_id=$2 AND version_id=$3`, sc.TenantID, sc.ProjectID, id).Scan(&admission.Verdict, &admission.BaselineVersionID, &admission.EvalIDs, &admission.PassRateBaseline, &admission.PassRateCandidate, &admission.MedianTokensBaseline, &admission.MedianTokensCandidate, &admission.FailedAttemptTokensBaseline, &admission.FailedAttemptTokensCandidate, &admission.SourceRuns, &admission.CreatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if err == nil {
		out.Admission = &admission
	}
	rows, err := tx.Query(ctx, `SELECT e.id,e.entry_key,e.content,e.source,e.evidence,e.identifiers FROM knowledge_entries e JOIN knowledge_members m ON (e.tenant_id,e.project_id,e.id)=(m.tenant_id,m.project_id,m.entry_id) WHERE m.tenant_id=$1 AND m.project_id=$2 AND m.version_id=$3 ORDER BY e.entry_key COLLATE "C", e.id`, sc.TenantID, sc.ProjectID, id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var e KnowledgeEntry
		if err = rows.Scan(&e.ID, &e.Key, &e.Content, &e.Source, &e.Evidence, &e.Identifiers); err != nil {
			return out, err
		}
		out.Entries = append(out.Entries, e)
	}
	return out, rows.Err()
}

func (s *Store) ChangeKnowledge(ctx context.Context, in KnowledgeChange) (KnowledgeActive, error) {
	var out KnowledgeActive
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
	if err = knowledgeProject(ctx, tx, in.KnowledgeScope); err != nil {
		return out, err
	}
	out, err = changeKnowledgeTx(ctx, tx, in)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func changeKnowledgeTx(ctx context.Context, tx pgx.Tx, in KnowledgeChange) (KnowledgeActive, error) {
	var out KnowledgeActive
	var err error
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
		if err = checkKnowledgeVersion(ctx, tx, in.KnowledgeScope, target); err != nil {
			return out, err
		}
	} else if in.Operation == "activate" {
		return out, ErrNotFound
	}
	if target == out.VersionID {
		return out, nil
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
	return out, nil
}

const knowledgeRankedMinEntries = 20
const knowledgeCommonTermShare = 0.25

// Normalising by ln(n) keeps the relevance floor stable across version sizes.
const knowledgeMinScore = 1.31

const knowledgeSearchSQL = `WITH members AS MATERIALIZED (
 SELECT e.id,e.entry_key,e.content,e.source,e.evidence,e.identifiers,e.search_vector
 FROM knowledge_members m JOIN knowledge_entries e ON (e.tenant_id,e.project_id,e.id)=(m.tenant_id,m.project_id,m.entry_id)
 WHERE m.tenant_id=$1 AND m.project_id=$2 AND m.version_id=$3
)
 SELECT e.id,e.entry_key,e.content,e.source,e.evidence,e.identifiers
 FROM members e
 WHERE (e.identifiers && $5::text[] OR e.search_vector @@ to_tsquery('simple',COALESCE((SELECT string_agg(quote_literal(term), ' | ' ORDER BY term) FROM unnest(tsvector_to_array(to_tsvector('simple',$4))) term),'')))
 ORDER BY (e.identifiers && $5::text[]) DESC, ts_rank_cd(e.search_vector,to_tsquery('simple',COALESCE((SELECT string_agg(quote_literal(term), ' | ' ORDER BY term) FROM unnest(tsvector_to_array(to_tsvector('simple',$4))) term),''))) DESC,e.entry_key COLLATE "C",e.id LIMIT 100`

const knowledgeRankedSearchSQL = `WITH members AS MATERIALIZED (
 SELECT e.id,e.entry_key,e.content,e.source,e.evidence,e.identifiers,e.search_vector
 FROM knowledge_members m JOIN knowledge_entries e ON (e.tenant_id,e.project_id,e.id)=(m.tenant_id,m.project_id,m.entry_id)
 WHERE m.tenant_id=$1 AND m.project_id=$2 AND m.version_id=$3
), n AS (
 SELECT count(*)::double precision AS total FROM members
), terms AS MATERIALIZED (
 SELECT term,to_tsquery('simple',quote_literal(term)) AS query,to_tsquery('simple',quote_literal(term) || ':A') AS key_query
 FROM (SELECT DISTINCT term FROM unnest(tsvector_to_array(to_tsvector('simple',$4))) term) lexemes
), df AS (
 SELECT t.term,t.query,t.key_query,count(*)::double precision AS frequency
 FROM terms t JOIN members e ON e.search_vector @@ t.query
 GROUP BY t.term,t.query,t.key_query
), kept AS (
 SELECT df.query,df.key_query,ln(n.total/df.frequency)/ln(n.total) AS idf
 FROM df CROSS JOIN n WHERE df.frequency <= $6::double precision*n.total
), scored AS (
 SELECT e.*,COALESCE((
  SELECT sum(k.idf*CASE WHEN e.search_vector @@ k.key_query THEN 2 ELSE 1 END)
  FROM kept k WHERE e.search_vector @@ k.query
 ),0) AS score FROM members e
)
 SELECT e.id,e.entry_key,e.content,e.source,e.evidence,e.identifiers
 FROM scored e WHERE e.identifiers && $5::text[] OR e.score >= $7::double precision
 ORDER BY (e.identifiers && $5::text[]) DESC,e.score DESC,e.entry_key COLLATE "C",e.id LIMIT 100`

const knowledgeHeadlineSQL = `SELECT ts_headline('simple', content, to_tsquery('simple',COALESCE((SELECT string_agg(quote_literal(term), ' | ' ORDER BY term) FROM unnest(tsvector_to_array(to_tsvector('simple',$4))) term),'')), 'StartSel="",StopSel="",MaxWords=60,MinWords=20,MaxFragments=3,FragmentDelimiter=" ... "') FROM knowledge_entries WHERE tenant_id=$1 AND project_id=$2 AND id=$3`

func (s *Store) SearchKnowledge(ctx context.Context, in KnowledgeSearch) (KnowledgePacket, error) {
	started := time.Now()
	out := KnowledgePacket{Entries: []KnowledgeEntry{}, Ranking: KnowledgeRankingVersion, QueryDigest: KnowledgeTextDigest(in.Query)}
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
	if err = knowledgeProject(ctx, tx, in.KnowledgeScope); err != nil {
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
		if err = checkKnowledgeVersion(ctx, tx, in.KnowledgeScope, out.VersionID); err != nil {
			return out, err
		}
		words := knowledgeQueryIdentifiers(in.Query)
		var members int64
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM knowledge_members WHERE tenant_id=$1 AND project_id=$2 AND version_id=$3`, in.TenantID, in.ProjectID, out.VersionID).Scan(&members); err != nil {
			return out, err
		}
		ranked := members >= knowledgeRankedMinEntries
		searchSQL := knowledgeSearchSQL
		args := []any{in.TenantID, in.ProjectID, out.VersionID, in.Query, words}
		if ranked {
			searchSQL = knowledgeRankedSearchSQL
			args = []any{in.TenantID, in.ProjectID, out.VersionID, in.Query, knowledgeShapedIdentifiers(words), knowledgeCommonTermShare, knowledgeMinScore}
		}
		rows, err := tx.Query(ctx, searchSQL, args...)
		if err != nil {
			return out, err
		}
		var candidates []KnowledgeEntry
		for rows.Next() {
			var e KnowledgeEntry
			if err = rows.Scan(&e.ID, &e.Key, &e.Content, &e.Source, &e.Evidence, &e.Identifiers); err != nil {
				rows.Close()
				return out, err
			}
			candidates = append(candidates, e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
		seen := map[string]bool{}
		for _, e := range candidates {
			hash := digestKnowledge(e.Content)
			if seen[hash] {
				continue
			}
			seen[hash] = true
			label := fmt.Sprintf("[Reference %s; source=%q; revision=%s; evidence=%q]\n%s\n[/Reference]\n", e.ID, e.Source, in.Revision, e.Evidence, e.Content)
			if len(KnowledgeReferenceBlock(out.Text+label)) > KnowledgeMaxPacketBytes {
				var headline string
				if err = tx.QueryRow(ctx, knowledgeHeadlineSQL, in.TenantID, in.ProjectID, e.ID, in.Query).Scan(&headline); err != nil {
					return out, err
				}
				if headline == "" || !utf8.ValidString(headline) || hasFrameMarker(headline) || headline == e.Content {
					continue
				}
				label = fmt.Sprintf("[Reference %s; source=%q; revision=%s; evidence=%q; excerpt=true]\n%s\n[/Reference]\n", e.ID, e.Source, in.Revision, e.Evidence, headline)
				if len(KnowledgeReferenceBlock(out.Text+label)) > KnowledgeMaxPacketBytes {
					continue
				}
				e.Content, e.Excerpt = headline, true
			}
			out.Text += label
			out.Entries = append(out.Entries, e)
			if len(out.Entries) == 5 {
				break
			}
		}
		if len(out.Entries) == 0 {
			out.EmptyReason = "no_match_or_byte_cap"
			if ranked && len(candidates) == 0 {
				out.EmptyReason = "below_threshold"
			}
		}
	}
	out.Bytes = len(out.Text)
	out.Digest = KnowledgeTextDigest(out.Text)
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

func (s *Store) ActiveKnowledge(ctx context.Context, sc KnowledgeScope) (KnowledgeActive, error) {
	var out KnowledgeActive
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

func (s *Store) RecordAdmission(ctx context.Context, in KnowledgeAdmission) error {
	if in.Verdict != "admitted" && in.Verdict != "rejected" && in.Verdict != "pending" {
		return ErrUnsupported
	}
	for _, n := range []float64{in.PassRateBaseline, in.PassRateCandidate, in.MedianTokensBaseline, in.MedianTokensCandidate, in.FailedAttemptTokensBaseline, in.FailedAttemptTokensCandidate} {
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return ErrUnsupported
		}
	}
	if in.PassRateBaseline > 1 || in.PassRateCandidate > 1 {
		return ErrUnsupported
	}
	for _, v := range append(append([]string{}, in.EvalIDs...), in.SourceRuns...) {
		if len(v) == 0 || len(v) > 256 || unsafeKnowledgePattern(v, true) != "" {
			return ErrUnsupported
		}
	}
	ctx, done, err := s.knowledgeContext(ctx)
	if err != nil {
		return err
	}
	defer done()
	tx, err := s.knowledge.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = knowledgeProject(ctx, tx, in.KnowledgeScope); err != nil {
		return err
	}
	if err = checkKnowledgeVersion(ctx, tx, in.KnowledgeScope, in.VersionID); err != nil {
		return err
	}
	if err = checkKnowledgeVersion(ctx, tx, in.KnowledgeScope, in.BaselineVersionID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO knowledge_active_versions(tenant_id,project_id,backend,model,revision) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, in.TenantID, in.ProjectID, in.Backend, in.Model, in.Revision)
	if err != nil {
		return err
	}
	var active string
	var generation int64
	err = tx.QueryRow(ctx, `SELECT COALESCE(version_id,''),generation FROM knowledge_active_versions WHERE tenant_id=$1 AND project_id=$2 AND backend=$3 AND model=$4 AND revision=$5 FOR UPDATE`, in.TenantID, in.ProjectID, in.Backend, in.Model, in.Revision).Scan(&active, &generation)
	if err != nil {
		return err
	}
	if active == in.VersionID || active != in.BaselineVersionID {
		return ErrConflict
	}
	if in.EvalIDs == nil {
		in.EvalIDs = []string{}
	}
	if in.SourceRuns == nil {
		in.SourceRuns = []string{}
	}
	_, err = tx.Exec(ctx, `INSERT INTO knowledge_admissions(tenant_id,project_id,version_id,verdict,baseline_version_id,eval_ids,pass_rate_baseline,pass_rate_candidate,median_tokens_baseline,median_tokens_candidate,failed_attempt_tokens_baseline,failed_attempt_tokens_candidate,source_runs) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT (tenant_id,project_id,version_id) DO UPDATE SET verdict=EXCLUDED.verdict,baseline_version_id=EXCLUDED.baseline_version_id,eval_ids=EXCLUDED.eval_ids,pass_rate_baseline=EXCLUDED.pass_rate_baseline,pass_rate_candidate=EXCLUDED.pass_rate_candidate,median_tokens_baseline=EXCLUDED.median_tokens_baseline,median_tokens_candidate=EXCLUDED.median_tokens_candidate,failed_attempt_tokens_baseline=EXCLUDED.failed_attempt_tokens_baseline,failed_attempt_tokens_candidate=EXCLUDED.failed_attempt_tokens_candidate,source_runs=EXCLUDED.source_runs,created_at=now()`, in.TenantID, in.ProjectID, in.VersionID, in.Verdict, in.BaselineVersionID, in.EvalIDs, in.PassRateBaseline, in.PassRateCandidate, in.MedianTokensBaseline, in.MedianTokensCandidate, in.FailedAttemptTokensBaseline, in.FailedAttemptTokensCandidate, in.SourceRuns)
	if err != nil {
		return err
	}
	if in.Verdict == "admitted" {
		_, err = changeKnowledgeTx(ctx, tx, KnowledgeChange{KnowledgeScope: in.KnowledgeScope, Operation: "activate", VersionID: in.VersionID, ExpectedGeneration: generation})
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
