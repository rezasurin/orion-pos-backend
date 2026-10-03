package httpserver

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

// Problem is an RFC 9457 problem details document with a stable machine-readable Code
// (BACKEND_PLAN.md section 4.10). Clients switch on Code, never on Title or Detail.
type Problem struct {
	Type      string      `json:"type"`
	Title     string      `json:"title"`
	Status    int         `json:"status"`
	Code      string      `json:"code"`
	Detail    string      `json:"detail,omitempty"`
	RequestID string      `json:"request_id,omitempty"`
	TenantIDs []uuid.UUID `json:"tenant_ids,omitempty"`
}

// WriteProblem writes p as application/problem+json, filling in the type, title and request id.
func WriteProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	if p.Type == "" {
		p.Type = "about:blank"
	}
	if p.Title == "" {
		p.Title = http.StatusText(p.Status)
	}
	if p.RequestID == "" {
		p.RequestID = middleware.GetReqID(r.Context())
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
