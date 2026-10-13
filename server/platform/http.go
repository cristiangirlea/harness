package platform

import (
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
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

type HTTPServer struct {
	Store         *Store
	OperatorToken string
	ScalerToken   string
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
	allowed, err := h.Store.WorkerAuthorized(r.Context(), id, token(r))
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
		invalid(w, err)
		return
	}
	respond(w, 201, map[string]any{"project": created, "worker_token": issued})
}

func (h *HTTPServer) submit(w http.ResponseWriter, r *http.Request) {
	var input Submission
	if !decode(w, r, &input) {
		return
	}
	if input.ProjectID != r.PathValue("project") {
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
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		entries, err := h.Store.Events(r.Context(), tenantID, projectID, jobID, after, 100)
		if err != nil {
			return
		}
		for _, entry := range entries {
			encoded, err := json.Marshal(entry)
			if err != nil {
				return
			}
			if _, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", entry.ID, entry.Kind, encoded); err != nil {
				return
			}
			after = entry.ID
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
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
	if !decode(w, r, &request) {
		return
	}
	if !h.project(w, r, request.ProjectID) {
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
	if !decode(w, r, &request) {
		return
	}
	if !h.project(w, r, request.ProjectID) {
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
	if !decode(w, r, &request) {
		return
	}
	if !h.project(w, r, request.ProjectID) {
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
		failure(w, 503, "unavailable", "scaling metric unavailable")
		return
	}
	respond(w, 200, map[string]int{"value": demand})
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
