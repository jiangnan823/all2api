//go:build !workbuddy && !full && (qoder || trae)

// growth_api_stub.go keeps the M7a growth panel endpoints compilable in
// qoder/trae-only builds, where the workbuddy platform (and with it the real
// growth backend in growth_api_wire.go) is excluded. The routes still
// register; the handlers answer an honest build-limitation error instead of
// faking success or touching a client that does not exist here.
package api

import "all2api/internal/host"

// newGrowthBackend is the stub counterpart of the workbuddy-tagged
// constructor: no growth client is wired in this build.
func newGrowthBackend(_ *host.Host) growthBackend { return nil }
