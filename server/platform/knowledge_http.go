package platform

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"net/http"
	"time"
)

func (h *HTTPServer) knowledgeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/knowledge/search", h.knowledgeSearch)
	mux.HandleFunc("POST /v1/knowledge/inspect", h.knowledgeInspect)
	mux.HandleFunc("POST /v1/knowledge/import", h.knowledgeImport)
	mux.HandleFunc("POST /v1/knowledge/change", h.knowledgeChange)
	mux.HandleFunc("POST /v1/knowledge/active", h.knowledgeActive)
}
func knowledgeFailure(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrRetrievalBusy) {
		failure(w, 503, "retrieval_busy", "retrieval capacity unavailable")
		return
	}
	invalid(w, err)
}
func (h *HTTPServer) knowledgeSearch(w http.ResponseWriter, r *http.Request) {
	var in retrieval.Search
	if !decode(w, r, &in) {
		return
	}
	if !h.knowledgeReadAuth(w, r, in.Scope) {
		return
	}
	out, err := h.Store.SearchKnowledge(r.Context(), in)
	if err != nil {
		knowledgeFailure(w, err)
		return
	}
	respond(w, 200, out)
}
func (h *HTTPServer) knowledgeReadAuth(w http.ResponseWriter, r *http.Request, sc retrieval.Scope) bool {
	// A worker credential may only read its exact project; a tenant credential
	// permits projects owned by that tenant, checked again in every store query.
	if !validKnowledgeScope(sc) {
		failure(w, 404, "not_found", "resource not found")
		return false
	}
	credential := token(r)
	if len(credential) != 64 {
		failure(w, 404, "not_found", "resource not found")
		return false
	}
	if sameToken(credential, h.OperatorToken) {
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	var worker, tenant []byte
	err := h.Store.knowledge.QueryRow(ctx, `SELECT p.worker_token_hash,t.token_hash FROM projects p JOIN tenants t ON t.id=p.tenant_id WHERE p.tenant_id=$1 AND p.id=$2`, sc.TenantID, sc.ProjectID).Scan(&worker, &tenant)
	if err != nil {
		failure(w, 404, "not_found", "resource not found")
		return false
	}
	hash := sha256.Sum256([]byte(credential))
	if subtle.ConstantTimeCompare(hash[:], worker) == 1 || subtle.ConstantTimeCompare(hash[:], tenant) == 1 {
		return true
	}
	failure(w, 404, "not_found", "resource not found")
	return false
}
func (h *HTTPServer) knowledgeInspect(w http.ResponseWriter, r *http.Request) {
	var in struct {
		retrieval.Scope
		VersionID string `json:"version_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !h.knowledgeReadAuth(w, r, in.Scope) {
		return
	}
	out, err := h.Store.InspectKnowledge(r.Context(), in.Scope, in.VersionID)
	if err != nil {
		knowledgeFailure(w, err)
		return
	}
	respond(w, 200, out)
}
func (h *HTTPServer) knowledgeImport(w http.ResponseWriter, r *http.Request) {
	if !h.operator(w, r) {
		return
	}
	var in retrieval.Import
	if !decode(w, r, &in) {
		return
	}
	id, err := h.Store.ImportKnowledge(r.Context(), in)
	if err != nil {
		knowledgeFailure(w, err)
		return
	}
	respond(w, 200, map[string]string{"version_id": id})
}
func (h *HTTPServer) knowledgeChange(w http.ResponseWriter, r *http.Request) {
	if !h.operator(w, r) {
		return
	}
	var in retrieval.Change
	if !decode(w, r, &in) {
		return
	}
	out, err := h.Store.ChangeKnowledge(r.Context(), in)
	if err != nil {
		knowledgeFailure(w, err)
		return
	}
	respond(w, 200, out)
}

func (h *HTTPServer) knowledgeActive(w http.ResponseWriter, r *http.Request) {
	var in retrieval.Scope
	if !decode(w, r, &in) {
		return
	}
	if !h.knowledgeReadAuth(w, r, in) {
		return
	}
	out, err := h.Store.ActiveKnowledge(r.Context(), in)
	if err != nil {
		knowledgeFailure(w, err)
		return
	}
	respond(w, 200, out)
}
