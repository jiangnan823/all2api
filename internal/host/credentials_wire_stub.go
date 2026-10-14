//go:build !workbuddy && !full && (qoder || trae)

// credentials_wire_stub.go keeps cmd/gateway/main.go compilable in
// qoder/trae-only builds, where the workbuddy platform (and with it the
// credential bridge in credentials_wire.go) is excluded. The gateway starts
// with zero workbuddy credentials and the sender never wires up.
package host

// WireWorkbuddySender is the no-op counterpart of the workbuddy-tagged
// bridge: nothing to wire when the platform is not part of this build.
func (s *CredentialStore) WireWorkbuddySender() int { return 0 }
