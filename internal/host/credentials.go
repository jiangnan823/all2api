// credentials.go implements the M3 credential plane: workbuddy auth files on
// disk → in-memory CredentialStore → pool account registration, plus the
// atomic auth-file write-back after a token rotation.
//
// Security red lines (task contract):
//   - credential material lives ONLY in this store's unexported fields and in
//     the auth files themselves. pool.Account carries no secrets; nothing here
//     ever logs, serializes, or state-files a token. credentials_test.go pins
//     this with substring assertions over the pool snapshot, the store and a
//     state-shaped document.
//   - tests never touch the real auth dir: everything runs against t.TempDir()
//     replicas of the documented file shape
//     {"account":{"enterpriseId","nickname","uid"},"auth":{"accessToken",
//     "domain","expiresAt","realm","refreshToken"}}.
//
// The workbuddy-typed bridge (CredentialSource + refresh sink + sender
// wiring) lives in credentials_wire.go, compiled under the same build tags as
// the workbuddy platform; qoder/trae-only builds get the no-op stub in
// credentials_wire_stub.go.
package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"all2api/internal/pool"
)

// WorkbuddyPlatform is the platform identifier auth files register under
// (pool.Account.Platform value).
const WorkbuddyPlatform = "workbuddy"

// auth file naming: workbuddy-<uid>.json.
const (
	authFilePrefix = "workbuddy-"
	authFileSuffix = ".json"
)

// fileCred is one loaded credential snapshot plus the auth-file path for
// write-back. Unexported on purpose: json.Marshal of the store yields "{}",
// so credential material cannot leak through any serialization path (pinned
// by TestStateSerializationNeverCarriesTokens).
type fileCred struct {
	AccessToken  string
	RefreshToken string
	UID          string
	EnterpriseID string
	Domain       string
	Global       bool
	ExpiresAt    time.Time
	Path         string // auth file this credential came from (write-back target)
}

// CredentialStore keeps one credential per platform/accountID in memory and
// owns the atomic auth-file write-back after a rotation. It is the production
// implementation behind the workbuddy sender's CredentialSource and refresh
// sink.
type CredentialStore struct {
	pool *pool.WeightedRandomSelector
	dir  string // auth dir remembered by LoadAuthDir (write-back fallback)

	mu      sync.RWMutex
	entries map[string]fileCred // "platform/accountID" → credential
}

// NewCredentialStore builds an empty store over the shared pool: accounts
// loaded later register there, and refresh write-backs keep the pool's
// ExpiresAt in sync. pool may be nil in tests that only exercise file parsing.
func NewCredentialStore(p *pool.WeightedRandomSelector) *CredentialStore {
	return &CredentialStore{
		pool:    p,
		entries: make(map[string]fileCred),
	}
}

// credKey builds the store key for one account.
func credKey(platform, accountID string) string {
	return platform + "/" + accountID
}

// Count returns the number of stored credentials.
func (s *CredentialStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// put replaces the credential for one account (load-time and rotation-time).
func (s *CredentialStore) put(key string, cred fileCred) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = cred
}

// lookup returns a value snapshot of one credential; callers can never mutate
// store state through the returned copy.
func (s *CredentialStore) lookup(key string) (fileCred, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.entries[key]
	return c, ok
}

// LoadAuthDir scans dir for workbuddy-*.json auth files and registers every
// well-formed account: a secret-free pool.Account (Account has no token
// fields by design) plus the credential in the store. A malformed file is
// skipped and logged by name — never fatal to the batch. A missing directory
// loads zero accounts and returns nil, so callers decide whether that is
// worth an info line instead of a fatal. The returned count is the number of
// auth files successfully loaded.
func (s *CredentialStore) LoadAuthDir(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("credentials: read auth dir %s: %w", dir, err)
	}

	s.mu.Lock()
	s.dir = dir
	s.mu.Unlock()

	loaded := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), authFilePrefix) || !strings.HasSuffix(e.Name(), authFileSuffix) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		cred, perr := parseAuthFile(path)
		if perr != nil {
			// Tolerant batch: skip and record the file name plus the failure
			// class. json parse errors name fields/offsets, never contents.
			fmt.Printf("credentials: skip %s: %v\n", e.Name(), perr)
			continue
		}
		s.put(credKey(WorkbuddyPlatform, cred.UID), cred)
		if s.pool != nil {
			s.pool.AddAccount(&pool.Account{
				ID:        cred.UID,
				Platform:  WorkbuddyPlatform,
				ExpiresAt: cred.ExpiresAt,
				Status:    "active",
			})
		}
		loaded++
	}
	return loaded, nil
}

// authFileDoc mirrors the on-disk auth file shape exactly:
//
//	{"account":{"enterpriseId","nickname","uid"},
//	 "auth":{"accessToken","domain","expiresAt","realm","refreshToken"}}
//
// expiresAt is unix seconds. Write-back rewrites through a raw map instead so
// unknown fields survive (writeBackAuthFile).
type authFileDoc struct {
	Account struct {
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
		UID          string `json:"uid"`
	} `json:"account"`
	Auth struct {
		AccessToken  string `json:"accessToken"`
		Domain       string `json:"domain"`
		ExpiresAt    int64  `json:"expiresAt"`
		Realm        string `json:"realm"`
		RefreshToken string `json:"refreshToken"`
	} `json:"auth"`
}

// parseAuthFile reads and validates one auth file. Error messages are failure
// classes (json syntax, missing uid/token) safe to log — they never echo file
// contents.
func parseAuthFile(path string) (fileCred, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fileCred{}, fmt.Errorf("read: %w", err)
	}
	var doc authFileDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fileCred{}, fmt.Errorf("parse: %w", err)
	}
	if strings.TrimSpace(doc.Account.UID) == "" {
		return fileCred{}, errors.New("account.uid missing")
	}
	if strings.TrimSpace(doc.Auth.AccessToken) == "" {
		return fileCred{}, errors.New("auth.accessToken missing")
	}
	cred := fileCred{
		AccessToken:  doc.Auth.AccessToken,
		RefreshToken: doc.Auth.RefreshToken,
		UID:          doc.Account.UID,
		EnterpriseID: doc.Account.EnterpriseID,
		Domain:       doc.Auth.Domain,
		Global:       strings.EqualFold(strings.TrimSpace(doc.Auth.Realm), "global"),
		Path:         path,
	}
	if doc.Auth.ExpiresAt > 0 {
		cred.ExpiresAt = time.Unix(doc.Auth.ExpiresAt, 0)
	}
	return cred, nil
}

// writeBack replaces the rotated tokens inside the credential's auth file:
// same file name, same JSON structure (unknown fields preserved via the raw
// map rewrite in writeBackAuthFile), atomic temp+rename. Called after the
// store is already updated, so a failed disk write is a log line, never a
// broken request chain.
func (s *CredentialStore) writeBack(key string, cred fileCred) error {
	path := cred.Path
	if path == "" {
		s.mu.RLock()
		dir := s.dir
		s.mu.RUnlock()
		if dir == "" {
			return fmt.Errorf("credentials: no auth file path recorded for %s", key)
		}
		path = filepath.Join(dir, authFilePrefix+cred.UID+authFileSuffix)
	}
	return writeBackAuthFile(path, cred)
}

// writeBackAuthFile performs the atomic rewrite of one auth file: the current
// document is re-read as a raw map (structure and unknown fields preserved),
// only the auth block's rotated fields are replaced, and the result lands via
// temp file + rename in the same directory.
func writeBackAuthFile(path string, cred fileCred) error {
	doc := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		// Deliberately tolerant: an unparseable or vanished file falls through
		// to a minimal rebuild below (uid is re-stamped from the credential).
		_ = json.Unmarshal(raw, &doc)
	}
	account, ok := doc["account"].(map[string]any)
	if !ok {
		account = map[string]any{}
		doc["account"] = account
	}
	if uid, _ := account["uid"].(string); strings.TrimSpace(uid) == "" {
		account["uid"] = cred.UID
	}
	auth, ok := doc["auth"].(map[string]any)
	if !ok {
		auth = map[string]any{}
		doc["auth"] = auth
	}
	auth["accessToken"] = cred.AccessToken
	auth["refreshToken"] = cred.RefreshToken
	if !cred.ExpiresAt.IsZero() {
		auth["expiresAt"] = cred.ExpiresAt.Unix()
	}
	if cred.Domain != "" {
		auth["domain"] = cred.Domain
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("credentials: marshal: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), authFilePrefix+"*.tmp")
	if err != nil {
		return fmt.Errorf("credentials: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName) // best-effort cleanup when rename did not consume it
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("credentials: write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("credentials: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("credentials: rename %s → %s: %w", tmpName, path, err)
	}
	tmpName = "" // consumed by the rename; deferred cleanup must not run
	return nil
}
