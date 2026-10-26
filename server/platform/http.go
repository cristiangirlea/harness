package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type HTTPServer struct {
	Store                *Store
	OperatorToken        string
	ScalerToken          string
	MaxCostUSDPerJob     float64
	submissionAuthorized func(context.Context, string, string) (bool, error)
	workerAuthorized     func(context.Context, string, string) (bool, error)
	eventStreams         eventStreams
	eventStreamDuration  time.Duration // Zero uses the production maximum.
}

const (
	maxTenantEventStreams  = 4
	maxEventStreamDuration = 30 * time.Minute
)

type eventStreams struct {
	mu     sync.Mutex
	active map[string]int
}

func (s *eventStreams) acquire(tenant string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[tenant] >= maxTenantEventStreams {
		return false
	}
	if s.active == nil {
		s.active = make(map[string]int)
	}
	s.active[tenant]++
	return true
}

func (s *eventStreams) release(tenant string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[tenant] <= 1 {
		delete(s.active, tenant)
	} else {
		s.active[tenant]--
	}
}

func (h *HTTPServer) Handler() http.Handler {
	mux := http.NewServeMux()
	h.knowledgeRoutes(mux)
	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("POST /v1/tenants", h.tenants)
	mux.HandleFunc("POST /v1/projects", h.projects)
	mux.HandleFunc("POST /v1/projects/{project}/jobs", h.submit)
	mux.HandleFunc("GET /v1/jobs/{job}", h.show)
	mux.HandleFunc("GET /v1/jobs/{job}/events", h.events)
	mux.HandleFunc("POST /v1/jobs/{job}/cancel", h.cancel)
	mux.HandleFunc("POST /v1/jobs/{job}/resume", h.resume)
	mux.HandleFunc("POST /internal/projects/{project}/claim", h.claim)
	mux.HandleFunc("POST /internal/jobs/{job}/heartbeat", h.heartbeat)
	mux.HandleFunc("POST /internal/jobs/{job}/checkpoint", h.checkpoint)
	mux.HandleFunc("POST /internal/jobs/{job}/finish", h.finish)
	mux.HandleFunc("GET /internal/projects/{project}/jobs/{job}/checkpoint", h.latestCheckpoint)
	mux.HandleFunc("GET /internal/scaling/projects/{project}", h.scaling)
	return mux
}

func token(r *http.Request) string {
	value := r.Header.Get("Authorization")
	result, ok := strings.CutPrefix(value, "Bearer ")
	if !ok || strings.ContainsAny(result, " \t\r\n") {
		return ""
	}
	return result
}

func sameToken(a, b string) bool {
	if len(a) < 32 || len(b) < 32 {
		return false
	}
	left, right := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}

func (h *HTTPServer) operator(w http.ResponseWriter, r *http.Request) bool {
	if sameToken(token(r), h.OperatorToken) {
		return true
	}
	failure(w, http.StatusUnauthorized, "unauthorized", "operator token required")
	return false
}

func (h *HTTPServer) tenant(w http.ResponseWriter, r *http.Request, id string) bool {
	allowed, err := h.Store.TenantAuthorized(r.Context(), id, token(r))
	if err != nil {
		failure(w, 500, "storage_error", "authorization storage unavailable")
		return false
	}
	if allowed {
		return true
	}
	failure(w, 404, "not_found", "tenant or resource not found")
	return false
}

func (h *HTTPServer) project(w http.ResponseWriter, r *http.Request, id string) bool {
	check := h.workerAuthorized
	if check == nil {
		check = h.Store.WorkerAuthorized
	}
	allowed, err := check(r.Context(), id, token(r))
	if err != nil {
		failure(w, 500, "storage_error", "authorization storage unavailable")
		return false
	}
	if allowed {
		return true
	}
	failure(w, 404, "not_found", "project or resource not found")
	return false
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func failure(w http.ResponseWriter, status int, code, message string) {
	respond(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func invalid(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		failure(w, 404, "not_found", "resource not found")
	case errors.Is(err, ErrConflict):
		failure(w, 409, "conflict", err.Error())
	case errors.Is(err, ErrNotOwner):
		failure(w, 409, "stale_owner", "execution owner is not current")
	case errors.Is(err, ErrUnsupported):
		failure(w, 422, "unsupported", err.Error())
	default:
		failure(w, 500, "storage_error", "request could not be completed")
	}
}

func createProjectErrorStatus(err error) int {
	var pgErr *pgconn.PgError
	// Only the tenant foreign key: PostgreSQL always names the constraint, and
	// an unnamed or other one must not become a 404 if the table gains a key.
	if errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == "projects_tenant_id_fkey" {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func decode(w http.ResponseWriter, r *http.Request, destination any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		failure(w, 400, "invalid_json", err.Error())
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		failure(w, 400, "invalid_json", "extra JSON content")
		return false
	}
	return true
}

func (h *HTTPServer) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.Store.db.Ping(ctx); err != nil {
		failure(w, 503, "unavailable", "database unavailable")
		return
	}
	respond(w, 200, map[string]string{"status": "ok"})
}

func (h *HTTPServer) tenants(w http.ResponseWriter, r *http.Request) {
	if !h.operator(w, r) {
		return
	}
	var input struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Name == "" || len(input.Name) > 100 || strings.TrimSpace(input.Name) != input.Name {
		failure(w, 400, "invalid_request", "invalid tenant name")
		return
	}
	id, issued, err := h.Store.CreateTenant(r.Context(), input.Name)
	if err != nil {
		invalid(w, err)
		return
	}
	respond(w, 201, map[string]string{"id": id, "tenant_token": issued})
}

func (h *HTTPServer) projects(w http.ResponseWriter, r *http.Request) {
	if !h.operator(w, r) {
		return
	}
	var project Project
	if !decode(w, r, &project) {
		return
	}
	if project.ID != "" {
		failure(w, 400, "invalid_request", "project ID is generated by the server")
		return
	}
	if err := validateProject(project); err != nil {
		failure(w, 400, "invalid_request", err.Error())
		return
	}
	created, issued, err := h.Store.CreateProject(r.Context(), project)
	if err != nil {
		if createProjectErrorStatus(err) == http.StatusNotFound {
			failure(w, http.StatusNotFound, "not_found", "tenant or resource not found")
			return
		}
		invalid(w, err)
		return
	}
	respond(w, 201, map[string]any{"project": created, "worker_token": issued})
}

func (h *HTTPServer) submit(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	check := h.submissionAuthorized
	if check == nil {
		check = func(ctx context.Context, projectID, candidate string) (bool, error) {
			tenantID, err := h.Store.ProjectTenant(ctx, projectID)
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			return h.Store.TenantAuthorized(ctx, tenantID, candidate)
		}
	}
	allowed, err := check(r.Context(), projectID, token(r))
	if err != nil {
		failure(w, 500, "storage_error", "authorization storage unavailable")
		return
	}
	if !allowed {
		failure(w, 404, "not_found", "tenant or resource not found")
		return
	}
	var input Submission
	if !decode(w, r, &input) {
		return
	}
	if input.ProjectID != projectID {
		failure(w, 400, "invalid_request", "project ID mismatch")
		return
	}
	if !h.tenant(w, r, input.TenantID) {
		return
	}
	if err := validateSubmission(input); err != nil {
		if errors.Is(err, ErrUnsupported) {
			invalid(w, err)
		} else {
			failure(w, 400, "invalid_request", err.Error())
		}
		return
	}
	if err := checkCostLimit(input, h.MaxCostUSDPerJob); err != nil {
		// A retry of a job accepted before the limit was lowered must still get that job back.
		existing := false
		if input.IdempotencyKey != "" {
			var lookupErr error
			existing, lookupErr = h.Store.IdempotentJobExists(r.Context(), input.TenantID, input.ProjectID, input.IdempotencyKey)
			if lookupErr != nil {
				failure(w, 500, "storage_error", "job storage unavailable")
				return
			}
		}
		if !existing {
			failure(w, 422, "cost_limit", err.Error())
			return
		}
	}
	job, reused, err := h.Store.Submit(r.Context(), input)
	if err != nil {
		invalid(w, err)
		return
	}
	code := 201
	if reused {
		code = 200
	}
	respond(w, code, job)
}

func scope(r *http.Request) (string, string) {
	return r.URL.Query().Get("tenant_id"), r.URL.Query().Get("project_id")
}

func (h *HTTPServer) show(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID := scope(r)
	if !h.tenant(w, r, tenantID) {
		return
	}
	job, err := h.Store.GetJob(r.Context(), tenantID, projectID, r.PathValue("job"))
	if err != nil {
		invalid(w, err)
		return
	}
	respond(w, 200, job)
}

func (h *HTTPServer) cancel(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID := scope(r)
	if !h.tenant(w, r, tenantID) {
		return
	}
	state, err := h.Store.Cancel(r.Context(), tenantID, projectID, r.PathValue("job"))
	if err != nil {
		invalid(w, err)
		return
	}
	respond(w, 200, map[string]string{"state": string(state)})
}

func (h *HTTPServer) events(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID := scope(r)
	if !h.tenant(w, r, tenantID) {
		return
	}
	queryCursor := r.URL.Query().Get("after")
	headerCursor := r.Header.Get("Last-Event-ID")
	if queryCursor != "" && headerCursor != "" && queryCursor != headerCursor {
		failure(w, 400, "cursor_mismatch", "after and Last-Event-ID differ")
		return
	}
	cursor := queryCursor
	if cursor == "" {
		cursor = headerCursor
	}
	after := int64(0)
	if cursor != "" {
		value, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || value < 0 {
			failure(w, 400, "invalid_cursor", "cursor must be a nonnegative integer")
			return
		}
		after = value
	}
	jobID := r.PathValue("job")
	if r.Header.Get("Accept") != "text/event-stream" {
		result, err := h.Store.Events(r.Context(), tenantID, projectID, jobID, after, 100)
		if err != nil {
			invalid(w, err)
			return
		}
		respond(w, 200, map[string]any{"events": result})
		return
	}
	// Check scope before committing streaming headers.
	if _, err := h.Store.GetJob(r.Context(), tenantID, projectID, jobID); err != nil {
		invalid(w, err)
		return
	}
	if !h.eventStreams.acquire(tenantID) {
		failure(w, http.StatusTooManyRequests, "stream_limit", "too many open event streams for tenant")
		return
	}
	defer h.eventStreams.release(tenantID)
	duration := h.eventStreamDuration
	if duration <= 0 {
		duration = maxEventStreamDuration
	}
	ctx, cancel := context.WithTimeout(r.Context(), duration)
	defer cancel()
	// Bound blocked writes as well as polling and database calls.
	controller := http.NewResponseController(w)
	deadline, _ := ctx.Deadline()
	_ = controller.SetWriteDeadline(deadline)
	defer controller.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		// Read state first: a terminal state and its event commit together, so
		// the following event query cannot miss the event that closes the job.
		job, err := h.Store.GetJob(ctx, tenantID, projectID, jobID)
		if err != nil {
			return
		}
		entries, err := h.Store.Events(ctx, tenantID, projectID, jobID, after, 100)
		if err != nil {
			return
		}
		terminal := job.State.terminal()
		for _, entry := range entries {
			encoded, err := json.Marshal(entry)
			if err != nil {
				return
			}
			if _, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", entry.ID, entry.Kind, encoded); err != nil {
				return
			}
			after = entry.ID
			terminal = terminal || State(entry.Kind).terminal()
		}
		if err := controller.Flush(); err != nil {
			return
		}
		if len(entries) == 100 {
			continue // Drain every backlog page before closing or waiting.
		}
		if terminal {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type workerRequest struct {
	ProjectID  string `json:"project_id"`
	WorkerID   string `json:"worker_id"`
	Generation int64  `json:"generation"`
}
type checkpointRequest struct {
	workerRequest
	Checkpoint agent.APICheckpoint `json:"checkpoint"`
}
type finishRequest struct {
	workerRequest
	Result agent.Result `json:"result"`
}

func workerProjectID(body []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(body))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return ""
	}
	var projectID string
	for decoder.More() {
		field, err := decoder.Token()
		if err != nil {
			break
		}
		if name, ok := field.(string); ok && strings.EqualFold(name, "project_id") {
			if err := decoder.Decode(&projectID); err != nil {
				break
			}
			continue
		}
		var ignored json.RawMessage
		if err := decoder.Decode(&ignored); err != nil {
			break
		}
	}
	return projectID
}

func (h *HTTPServer) decodeWorkerRequest(w http.ResponseWriter, r *http.Request, destination any) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, readErr := io.ReadAll(r.Body)
	projectID := workerProjectID(body)
	if !h.project(w, r, projectID) {
		return "", false
	}
	if readErr != nil {
		failure(w, 400, "invalid_json", readErr.Error())
		return "", false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return projectID, decode(w, r, destination)
}

func (h *HTTPServer) claim(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	if !h.project(w, r, projectID) {
		return
	}
	var request struct {
		WorkerID string `json:"worker_id"`
	}
	if !decode(w, r, &request) {
		return
	}
	if request.WorkerID == "" || len(request.WorkerID) > 128 {
		failure(w, 400, "invalid_request", "invalid worker ID")
		return
	}
	job, err := h.Store.Claim(r.Context(), projectID, request.WorkerID)
	if errors.Is(err, ErrNotFound) {
		w.WriteHeader(204)
		return
	}
	if err != nil {
		invalid(w, err)
		return
	}
	respond(w, 200, job)
}

func (h *HTTPServer) heartbeat(w http.ResponseWriter, r *http.Request) {
	var request workerRequest
	projectID, ok := h.decodeWorkerRequest(w, r, &request)
	if !ok {
		return
	}
	if request.ProjectID != projectID {
		failure(w, 404, "not_found", "project or resource not found")
		return
	}
	ok, cancel, err := h.Store.Heartbeat(r.Context(), request.ProjectID, r.PathValue("job"), request.WorkerID, request.Generation)
	if err != nil {
		failure(w, 500, "storage_error", "heartbeat failed")
		return
	}
	if !ok {
		invalid(w, ErrNotOwner)
		return
	}
	respond(w, 200, map[string]bool{"cancel_requested": cancel})
}

func (h *HTTPServer) checkpoint(w http.ResponseWriter, r *http.Request) {
	var request checkpointRequest
	projectID, ok := h.decodeWorkerRequest(w, r, &request)
	if !ok {
		return
	}
	if request.ProjectID != projectID {
		failure(w, 404, "not_found", "project or resource not found")
		return
	}
	if err := h.Store.SaveCheckpoint(r.Context(), request.ProjectID, r.PathValue("job"), request.WorkerID, request.Generation, request.Checkpoint); err != nil {
		invalid(w, err)
		return
	}
	respond(w, 200, map[string]string{"status": "saved"})
}

func (h *HTTPServer) finish(w http.ResponseWriter, r *http.Request) {
	var request finishRequest
	projectID, ok := h.decodeWorkerRequest(w, r, &request)
	if !ok {
		return
	}
	if request.ProjectID != projectID {
		failure(w, 404, "not_found", "project or resource not found")
		return
	}
	if err := h.Store.Finish(r.Context(), request.ProjectID, r.PathValue("job"), request.WorkerID, request.Generation, request.Result); err != nil {
		invalid(w, err)
		return
	}
	respond(w, 200, map[string]string{"status": "saved"})
}

func (s *Store) ProjectTenant(ctx context.Context, projectID string) (string, error) {
	var tenantID string
	err := s.db.QueryRow(ctx, "SELECT tenant_id FROM projects WHERE id=$1", projectID).Scan(&tenantID)
	return tenantID, err
}

func (h *HTTPServer) latestCheckpoint(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	if !h.project(w, r, projectID) {
		return
	}
	tenantID, err := h.Store.ProjectTenant(r.Context(), projectID)
	if err != nil {
		invalid(w, ErrNotFound)
		return
	}
	cp, err := h.Store.LatestCheckpoint(r.Context(), tenantID, projectID, r.PathValue("job"))
	if err != nil {
		invalid(w, err)
		return
	}
	respond(w, 200, cp)
}

func (h *HTTPServer) scaling(w http.ResponseWriter, r *http.Request) {
	if !sameToken(token(r), h.ScalerToken) {
		failure(w, 401, "unauthorized", "scaler token required")
		return
	}
	demand, err := h.Store.ScalingDemand(r.Context(), r.PathValue("project"))
	if err != nil {
		if scalingDemandErrorStatus(err) == http.StatusNotFound {
			failure(w, http.StatusNotFound, "not_found", "project or resource not found")
			return
		}
		failure(w, 503, "unavailable", "scaling metric unavailable")
		return
	}
	respond(w, 200, map[string]int{"value": demand})
}

func scalingDemandErrorStatus(err error) int {
	if errors.Is(err, pgx.ErrNoRows) {
		return http.StatusNotFound
	}
	return http.StatusServiceUnavailable
}

type recoveryRequest struct {
	TenantID            string `json:"tenant_id"`
	ProjectID           string `json:"project_id"`
	Action              string `json:"action"`
	TerminationEvidence string `json:"termination_evidence"`
}

func (h *HTTPServer) resume(w http.ResponseWriter, r *http.Request) {
	if !h.operator(w, r) {
		return
	}
	var input recoveryRequest
	if !decode(w, r, &input) {
		return
	}
	if input.TerminationEvidence == "" || len(input.TerminationEvidence) > 1024 || (input.Action != "resume" && input.Action != "fail" && input.Action != "complete") {
		failure(w, 400, "invalid_request", "invalid recovery action or termination evidence")
		return
	}
	state, err := h.Store.ResolveRecovery(r.Context(), input.TenantID, input.ProjectID, r.PathValue("job"),
		input.Action, input.TerminationEvidence)
	if err != nil {
		invalid(w, err)
		return
	}
	respond(w, 200, map[string]string{"state": string(state)})
}
