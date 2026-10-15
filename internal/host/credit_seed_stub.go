//go:build !workbuddy && !full && (qoder || trae)

// credit_seed_stub.go: the workbuddy billing endpoints are not part of this
// build, so there is nothing to seed from — the gateway starts with zero
// workbuddy accounts anyway.
package host

// SeedCredits is the no-op counterpart of the workbuddy-tagged seeding.
func (s *CredentialStore) SeedCredits() (int, int) { return 0, 0 }
