package host

// accounts_remove.go implements the M6a account removal and selection-pause
// plane — the write-side mirror of accounts_add.go:
//
//   - RemoveAccount deletes the credential FILE first (the source of truth),
//     then clears the store entry and the pool account. A failed file delete
//     leaves every in-memory trace intact so the panel can retry: clearing
//     memory while the file survives would resurrect the account on the next
//     restart.
//   - SetPaused flips the pool's selection pause switch (Select skips paused
//     accounts).
//
// Frozen constraint: neither operation ever reads or writes config.json —
// account lifecycle is purely a credential-plane concern.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrAccountUnknown reports that the platform:accountID pair has no stored
// credential. The panel endpoint maps it to 404 (errors.Is).
var ErrAccountUnknown = errors.New("account not found")

// RemoveAccount removes one account everywhere: the credential file on disk
// (workbuddy: workbuddy-<uid>.json; qoder: the shared qoder-pat.txt — the PAT
// file holds exactly one account), the store entry and the pool account.
// Returns ErrAccountUnknown when the platform/accountID pair is not stored.
func (s *CredentialStore) RemoveAccount(platform, accountID string) error {
	key := credKey(platform, accountID)
	cred, ok := s.lookup(key)
	if !ok {
		return fmt.Errorf("%w: %s/%s", ErrAccountUnknown, platform, accountID)
	}

	s.mu.RLock()
	dir := s.dir
	s.mu.RUnlock()

	// Resolve the credential file exactly like the rotation write-back does:
	// the recorded per-credential path first, the auth_dir layout as fallback
	// (panel-added workbuddy credentials and startup-loaded PATs carry no
	// Path in their store entry).
	path := cred.Path
	if path == "" {
		if dir == "" {
			return fmt.Errorf("credentials: no auth file path recorded for %s", key)
		}
		if platform == QoderPlatform {
			path = filepath.Join(dir, qoderPatFile)
		} else {
			path = filepath.Join(dir, authFilePrefix+accountID+authFileSuffix)
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("credentials: remove %s: %w", path, err)
	}
	// A file that is already gone is the requested end state — fall through
	// and clear the memory side (an idempotent re-remove converges).

	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
	if s.pool != nil {
		s.pool.RemoveAccount(platform, accountID)
	}
	return nil
}

// SetPaused writes the pool account's selection pause switch. Returns false
// when the account is unknown (the panel endpoint maps that to 404).
func (s *CredentialStore) SetPaused(platform, accountID string, paused bool) bool {
	if s.pool == nil {
		return false
	}
	return s.pool.SetAccountPaused(platform, accountID, paused)
}
