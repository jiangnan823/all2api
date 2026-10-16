package api

import (
	"context"
	"sort"
	"strings"

	"all2api/internal/host"
	"all2api/internal/router"
)

// buildModelRowsHook is the test seam over the catalog source; nil in
// production (the real buildModelRows runs).
var buildModelRowsHook func(ctx context.Context, cat upstreamCatalog, refresh bool) ([]modelWire, string)

// PublicModelList is the /v1/models source: the same catalog GET /api/models
// serves (static qoder + live-or-static workbuddy), served twice over —
//
//  1. bare ids ("deepseek-v4-flash"), deduped first-wins in the catalog's
//     provider-then-id order, for clients that just want a working name;
//  2. namespaced ids ("workbuddy/deepseek-v4-flash", "qoder/DeepSeek-V4-Pro"),
//     one per catalog row, for callers that must pin a platform: two
//     platforms often carry the same model, and the bare name's owner can
//     drift with pool occupancy, while the namespaced form routes exactly
//     (router dispatch strips the prefix before the upstream call).
//
// Never fails: catalog shortfalls degrade to the static tables, an empty
// gateway yields an empty list.
func PublicModelList(ctx context.Context, h *host.Host) []router.PublicModel {
	var cat upstreamCatalog
	if h != nil {
		cat = newModelsCatalog(h)
	}
	build := buildModelRows
	if buildModelRowsHook != nil {
		build = buildModelRowsHook
	}
	rows, _ := build(ctx, cat, false)
	seen := map[string]bool{}
	out := []router.PublicModel{}
	for _, m := range rows {
		bare := strings.TrimPrefix(m.ID, m.Provider+"/")
		if bare != "" && !seen[bare] {
			seen[bare] = true
			out = append(out, router.PublicModel{ID: bare, OwnedBy: m.Provider})
		}
		if m.ID != "" && m.ID != bare {
			out = append(out, router.PublicModel{ID: m.ID, OwnedBy: m.Provider})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
