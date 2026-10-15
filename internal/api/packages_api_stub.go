//go:build !workbuddy && !full && (qoder || trae)

// packages_api_stub.go keeps the M9a packages panel endpoints compilable in
// qoder/trae-only builds, where the workbuddy platform (and with it the real
// packages backend in packages_api_wire.go) is excluded. The routes still
// register; the handlers answer an honest build-limitation error instead of
// faking success or touching a client that does not exist here.
package api

import "all2api/internal/host"

// newPackagesBackend is the stub counterpart of the workbuddy-tagged
// constructor: no packages client is wired in this build.
func newPackagesBackend(_ *host.Host) packagesBackend { return nil }
