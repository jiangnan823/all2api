//go:build !workbuddy && !full && (qoder || trae)

// auth_login_stub.go keeps the M8 browser-login endpoints compilable in
// qoder/trae-only builds, where the workbuddy platform (and with it the real
// login backend in auth_login_wire.go) is excluded. The routes still
// register; the handlers answer an honest build-limitation error instead of
// faking success or touching a client that does not exist here.
package api

import "all2api/internal/host"

// newLoginBackend is the stub counterpart of the workbuddy-tagged
// constructor: no login client is wired in this build.
func newLoginBackend(_ *host.Host) loginBackend { return nil }
