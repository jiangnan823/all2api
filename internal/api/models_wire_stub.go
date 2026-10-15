//go:build !workbuddy && !full && (qoder || trae)

// models_wire_stub.go keeps the M8 /api/models live-catalog seam compilable
// in qoder/trae-only builds, where the workbuddy platform (and with it the
// real upstream directory in models_wire.go) is excluded. The endpoint still
// registers; the workbuddy half answers from the static name list and the
// source marker stays "local" instead of pretending a live fetch happened.
package api

import (
	"all2api/internal/host"
)

// newModelsCatalog is the stub counterpart of the workbuddy-tagged
// constructor: no live catalog exists in this build, so the seam stays nil
// and handleModels serves the static tables only (source "local").
func newModelsCatalog(h *host.Host) upstreamCatalog {
	return nil
}
