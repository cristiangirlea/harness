package platform

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

type State string

const (
	Queued           State = "queued"
	Admitted         State = "admitted"
	Running          State = "running"
	WaitingApproval  State = "waiting_approval"
	RecoveryRequired State = "recovery_required"
	Succeeded        State = "succeeded"
	Failed           State = "failed"
	Canceled         State = "canceled"
)

const (
	GlobalSlots       = 2
	TenantSlots       = 2
	ProjectSlots      = 2
	AdmissionTimeout  = 5 * time.Minute
	LeaseDuration     = time.Minute
	HeartbeatInterval = 10 * time.Second
)

var (
	ErrNotFound    = errors.New("not found")
	ErrConflict    = errors.New("conflicting idempotency key")
	ErrNotOwner    = errors.New("stale or expired execution owner")
	ErrUnsupported = errors.New("unsupported remote execution mode")
	refPattern     = regexp.MustCompile("^[0-9a-f]{40}([0-9a-f]{24})?$")
)

type Project struct {
	ID            string `json:"id"`
	TenantID      string `json:"tenant_id"`
	Name          string `json:"name"`
	RepositoryURL string `json:"repository_url"`
	BaseRef       string `json:"base_ref"`
	Backend       string `json:"backend"`
}

type Job struct {
	ID                 string        `json:"id"`
	TenantID           string        `json:"tenant_id"`
	ProjectID          string        `json:"project_id"`
	Prompt             string        `json:"prompt"`
	Options            agent.Options `json:"options"`
	Backend            string        `json:"backend"`
	BaseRef            string        `json:"base_ref"`
	RepositoryURL      string        `json:"repository_url"`
	State              State         `json:"state"`
	Generation         int64         `json:"generation"`
	OwnerID            string        `json:"owner_id,omitempty"`
	LeaseUntil         *time.Time    `json:"lease_until,omitempty"`
	CheckpointSequence int64         `json:"checkpoint_sequence"`
	Result             *agent.Result `json:"result,omitempty"`
}

type Submission struct {
	TenantID       string        `json:"tenant_id"`
	ProjectID      string        `json:"project_id"`
	Prompt         string        `json:"prompt"`
	Options        agent.Options `json:"options"`
	Backend        string        `json:"backend"`
	BaseRef        string        `json:"base_ref"`
	IdempotencyKey string        `json:"idempotency_key,omitempty"`
}

type Event struct {
	ID      int64           `json:"id"`
	JobID   string          `json:"job_id"`
	Kind    string          `json:"kind"`
	Details json.RawMessage `json:"details"`
	Created time.Time       `json:"created_at"`
}

func newID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

func newToken() (string, []byte, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	token := hex.EncodeToString(b[:])
	digest := sha256.Sum256([]byte(token))
	return token, digest[:], nil
}

func validateProject(p Project) error {
	if len(p.Name) == 0 || len(p.Name) > 100 || strings.TrimSpace(p.Name) != p.Name {
		return errors.New("project name must be 1..100 trimmed characters")
	}
	parsed, err := url.Parse(p.RepositoryURL)
	if err != nil || parsed == nil || (parsed.Scheme != "https" && parsed.Scheme != "ssh") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("repository URL must be an HTTPS or SSH URL without embedded credentials")
	}
	if !refPattern.MatchString(p.BaseRef) {
		return errors.New("base ref must be a full Git commit hash")
	}
	switch p.Backend {
	case "api", "claude", "codex", "gemini", "opencode":
	default:
		return fmt.Errorf("unknown backend %q", p.Backend)
	}
	return nil
}

func validateSubmission(s Submission) error {
	if s.Options.Retrieval != "" && s.Options.Retrieval != "off" {
		return ErrUnsupported
	}
	if len(s.Prompt) == 0 || len(s.Prompt) > 100_000 {
		return errors.New("prompt must be 1..100000 bytes")
	}
	if len(s.IdempotencyKey) > 128 {
		return errors.New("idempotency key exceeds 128 bytes")
	}
	if err := s.Options.Validate(); err != nil {
		return err
	}
	if s.Backend != "api" {
		return fmt.Errorf("%w: only the API backend is supported by this initial worker", ErrUnsupported)
	}
	if s.Options.Mode != agent.ModeReadOnly {
		return fmt.Errorf("%w: remote approval and isolated tools are not available yet; use readonly", ErrUnsupported)
	}
	if s.Options.MaxCostUSD <= 0 {
		return errors.New("API jobs require a positive spending ceiling")
	}
	if !refPattern.MatchString(s.BaseRef) {
		return errors.New("base ref must be a full Git commit hash")
	}
	return nil
}

func requestHash(s Submission) ([]byte, error) {
	payload, err := json.Marshal(struct {
		Prompt  string
		Options agent.Options
		Backend string
		BaseRef string
	}{s.Prompt, s.Options, s.Backend, s.BaseRef})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	return digest[:], nil
}
