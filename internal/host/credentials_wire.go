//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// credentials_wire.go bridges the host CredentialStore onto the workbuddy
// sender: the store IS the sender's CredentialSource (per-account lookup) and
// its refresh sink (store update → pool expiry sync → atomic auth-file
// write-back). Compiled under the same build tags as the workbuddy platform;
// qoder/trae-only builds get the no-op stub in credentials_wire_stub.go.
package host

import (
	"context"
	"errors"
	"fmt"

	"all2api/internal/adapter"
	"all2api/internal/platform/workbuddy"
	"all2api/internal/pool"
)

// Compile-time proof the store satisfies both sender seams.
var (
	_ workbuddy.CredentialSource = (*CredentialStore)(nil)
	_ workbuddy.CredentialSink   = (*CredentialStore)(nil)
)

// CredentialFor implements workbuddy.CredentialSource: value-snapshot lookup
// by platform/accountID. An unknown account is an explicit error — never a
// fabricated credential, never a panic; the sender folds it into the
// auth_expired failure the router's retry loop understands.
func (s *CredentialStore) CredentialFor(_ context.Context, acct *pool.Account) (*workbuddy.Credential, error) {
	if acct == nil {
		return nil, errors.New("credential store: no account given")
	}
	cred, ok := s.lookup(credKey(acct.Platform, acct.ID))
	if !ok {
		return nil, fmt.Errorf("credential store: no credential for %s/%s", acct.Platform, acct.ID)
	}
	return &workbuddy.Credential{
		AccessToken:  cred.AccessToken,
		RefreshToken: cred.RefreshToken,
		UID:          cred.UID,
		EnterpriseID: cred.EnterpriseID,
		Domain:       cred.Domain,
		Global:       cred.Global,
		ExpiresAt:    cred.ExpiresAt,
	}, nil
}

// AcceptRefreshed implements the sender's refresh sink
// (workbuddy.CredentialSink): store update first (the replaying send resolves
// the new tokens from here on its next lookup), pool expiry sync second,
// atomic file write-back last. A disk failure is logged and swallowed — the
// in-memory store already serves the rotated tokens, so the request chain
// continues; the file catches up on the next successful rotation or restart.
func (s *CredentialStore) AcceptRefreshed(_ context.Context, acct *pool.Account, cred *workbuddy.Credential) error {
	if acct == nil || cred == nil {
		return errors.New("credential store: refresh sink needs account and credential")
	}
	key := credKey(acct.Platform, acct.ID)
	prev, _ := s.lookup(key)
	updated := fileCred{
		AccessToken:  cred.AccessToken,
		RefreshToken: cred.RefreshToken,
		UID:          cred.UID,
		EnterpriseID: cred.EnterpriseID,
		Domain:       cred.Domain,
		Global:       cred.Global,
		ExpiresAt:    cred.ExpiresAt,
		Path:         prev.Path, // same auth file name and location as at load time
	}
	if updated.UID == "" {
		updated.UID = acct.ID
	}
	s.put(key, updated)
	if s.pool != nil {
		s.pool.SetAccountExpiry(acct.Platform, acct.ID, cred.ExpiresAt)
	}
	if err := s.writeBack(key, updated); err != nil {
		// Log the file name only — never token material.
		fmt.Printf("credentials: write-back %s: %v (kept in-memory)\n",
			authFilePrefix+updated.UID+authFileSuffix, err)
	}
	return nil
}

// WireWorkbuddySender injects the store into the registered workbuddy adapter
// as credential source + refresh sink. Production base URLs stay realm
// defaults (zero-value ClientOptions is production-shaped). Returns 1 when the
// adapter was found and configured, 0 otherwise.
func (s *CredentialStore) WireWorkbuddySender() int {
	a := adapter.Default.Get(WorkbuddyPlatform)
	if a == nil {
		return 0
	}
	type senderConfigurator interface {
		Configure(workbuddy.ClientOptions)
	}
	cfg, ok := a.(senderConfigurator)
	if !ok {
		return 0
	}
	cfg.Configure(workbuddy.ClientOptions{
		Credentials: s,
		Refresher:   s,
	})
	return 1
}
