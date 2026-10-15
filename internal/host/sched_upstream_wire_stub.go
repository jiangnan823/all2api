//go:build !workbuddy && !full && (qoder || trae)

// sched_upstream_wire_stub.go keeps the schedule upstream honest in
// qoder/trae-only builds, where the workbuddy platform (and with it the real
// task client in sched_upstream_wire.go) is excluded. Every operation wraps
// schedule.ErrUnsupportedTask — the handler batch loop classifies that as
// Skipped, so no scheduled traffic is faked and no account state is invented.
package host

import "all2api/internal/schedule"

// newScheduleUpstream is the stub counterpart of the workbuddy-tagged
// constructor: no platform client is wired in this build.
func newScheduleUpstream(_ *CredentialStore) schedule.Upstream {
	return stubUpstream{}
}
