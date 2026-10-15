//go:build (workbuddy || trae) && !qoder && !full

// credentials_wire_qoder_stub.go keeps cmd/gateway/main.go compilable in
// workbuddy-only and trae-only builds, where the qoder platform (and with it
// the bridge in credentials_wire_qoder.go) is excluded. The gateway starts
// with zero qoder credentials and the sender never wires up.
package host

// WireQoderSender is the no-op counterpart of the qoder-tagged bridge:
// nothing to wire when the platform is not part of this build.
func (s *CredentialStore) WireQoderSender() int { return 0 }

// SeedQoderCredits is the no-op counterpart of the qoder credit seeding.
func (s *CredentialStore) SeedQoderCredits() (int, int) { return 0, 0 }
