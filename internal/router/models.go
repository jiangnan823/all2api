package router

import (
	"context"
	"net/http"
)

// PublicModel is one servable model as the /v1/models list reports it: the
// bare OpenAI-style id (what clients put in the request "model" field) and
// the platform that owns it. The panel catalog (api package) is the source —
// the router never imports it (cycle), so the list arrives injected.
type PublicModel struct {
	ID      string // bare model name, e.g. "deepseek-v4-flash"
	OwnedBy string // platform: "workbuddy", "qoder", "trae"
}

// DefaultModelLister is the process-wide model-list source, wired by main to
// the api package's catalog (same data GET /api/models serves). Nil until
// wired — the endpoint then answers an honest empty list rather than 501, so
// clients' connectivity probes still pass.
var DefaultModelLister func(ctx context.Context) []PublicModel

// modelObjectWire is one row of the OpenAI model object.
type modelObjectWire struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // constant "model"
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type modelListWire struct {
	Object string            `json:"object"` // constant "list"
	Data   []modelObjectWire `json:"data"`
}

// modelListCreated is a fixed timestamp for every row: the catalog has no
// per-model creation facts and clients only check list membership, so a
// constant keeps responses deterministic.
const modelListCreated = int64(1735689600) // 2025-01-01T00:00:00Z

// handleV1Models serves GET /v1/models — the OpenAI catalog probe most
// clients use as their "test connection". Auth matches the rest of /v1
// (authorize); the body is the OpenAI list envelope over the injected
// catalog. A nil lister yields an empty list with 200 — the endpoint exists,
// the gateway just serves no named models yet.
func (rt *Router) handleV1Models(w http.ResponseWriter, r *http.Request) {
	if !authorize(w, r) {
		return
	}
	// Through the cache: /v1/models is the connectivity probe most clients
	// hammer, and the underlying panel catalog build can dial upstreams.
	rows := []modelObjectWire{}
	for _, m := range rt.catalogRows(r.Context()) {
		if m.ID == "" {
			continue
		}
		rows = append(rows, modelObjectWire{
			ID:      m.ID,
			Object:  "model",
			Created: modelListCreated,
			OwnedBy: m.OwnedBy,
		})
	}
	writeJSON(w, http.StatusOK, modelListWire{Object: "list", Data: rows})
}
