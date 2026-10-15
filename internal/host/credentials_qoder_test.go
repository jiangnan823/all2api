package host

// Qoder PAT credential plane tests: load qoder-pat.txt from a t.TempDir
// replica of the auth dir — the real auth dir (and the real PAT file) is
// never read or written (task red line). All PAT values are fake "pt-"
// strings.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"all2api/internal/pool"
)

// fakePAT mints a fake "pt-"-prefixed PAT (static token shape, no JWT
// structure — 67 chars like the documented pt- + 64 hex).
func fakePAT(seed string) string {
	return "pt-" + seed + strings.Repeat("a1b2c3d4", 8)
}

// writeQoderPAT creates qoder-pat.txt in dir and returns its path.
func writeQoderPAT(t *testing.T, dir, pat string) string {
	t.Helper()
	path := filepath.Join(dir, qoderPatFile)
	if err := os.WriteFile(path, []byte(pat+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadQoderPATRegistersPoolAccount(t *testing.T) {
	dir := t.TempDir()
	pat := fakePAT("ACCT1FAKE")
	writeAuthFile(t, dir, "uid-a", fakeAccessToken("ACCT1FAKE"), fakeRefreshToken("REF1FAKE"), 1794232800)
	writeQoderPAT(t, dir, pat)

	p := pool.NewWeightedRandomSelector(0, 3, 60)
	s := NewCredentialStore(p)
	loaded, err := s.LoadAuthDir(dir)
	if err != nil {
		t.Fatalf("LoadAuthDir: %v", err)
	}
	if loaded != 2 {
		t.Fatalf("loaded = %d, want 2 (workbuddy file + qoder PAT)", loaded)
	}
	if s.CountPlatform(WorkbuddyPlatform) != 1 || s.CountPlatform(QoderPlatform) != 1 {
		t.Fatalf("platform counts = wb %d / qoder %d, want 1/1",
			s.CountPlatform(WorkbuddyPlatform), s.CountPlatform(QoderPlatform))
	}

	// Pool registration: qoder platform, fingerprint account id, no expiry.
	snap := p.SnapshotAccounts()
	var qa *pool.Account
	for i := range snap {
		if snap[i].Platform == QoderPlatform {
			qa = &snap[i]
		}
	}
	if qa == nil {
		t.Fatal("qoder account missing from pool")
	}
	wantID := patFingerprint(pat)
	if len(wantID) != 8 {
		t.Fatalf("fingerprint length = %d, want 8 hex chars", len(wantID))
	}
	if qa.ID != wantID {
		t.Errorf("account id = %q, want the PAT fingerprint %q", qa.ID, wantID)
	}
	if !qa.ExpiresAt.IsZero() {
		t.Errorf("account expiry = %v, want zero (static PAT has no expiry)", qa.ExpiresAt)
	}
	if qa.Status != "active" {
		t.Errorf("status = %q, want active", qa.Status)
	}

	// Store lookup round-trips the PAT.
	cred, ok := s.lookup(credKey(QoderPlatform, wantID))
	if !ok || cred.AccessToken != pat {
		t.Error("stored credential does not round-trip the PAT")
	}
}

func TestLoadQoderPATSkipsInvalidFiles(t *testing.T) {
	cases := map[string]string{
		"empty file":   "",
		"whitespace":   "   \n\t\n",
		"wrong prefix": "jwt-abc123",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, qoderPatFile), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			p := pool.NewWeightedRandomSelector(0, 3, 60)
			s := NewCredentialStore(p)
			if _, err := s.LoadAuthDir(dir); err != nil {
				t.Fatalf("LoadAuthDir: %v", err)
			}
			if s.CountPlatform(QoderPlatform) != 0 || len(p.SnapshotAccounts()) != 0 {
				t.Error("invalid PAT file must not register an account")
			}
		})
	}

	// Missing file: silent zero, never an error.
	p := pool.NewWeightedRandomSelector(0, 3, 60)
	s := NewCredentialStore(p)
	if _, err := s.LoadAuthDir(t.TempDir()); err != nil {
		t.Fatalf("LoadAuthDir without qoder-pat.txt: %v", err)
	}
	if s.CountPlatform(QoderPlatform) != 0 {
		t.Error("no qoder-pat.txt must mean zero qoder accounts")
	}
}

func TestQoderPATFingerprintStableAndDistinct(t *testing.T) {
	a := fakePAT("ACCT1FAKE")
	b := fakePAT("ACCT2FAKE")
	fpA, fpA2, fpB := patFingerprint(a), patFingerprint(a), patFingerprint(b)
	if fpA != fpA2 {
		t.Error("fingerprint must be stable for the same PAT")
	}
	if fpA == fpB {
		t.Error("distinct PATs must not collide on the fingerprint")
	}
	// Reload with the same PAT: same account id (stable identity across
	// restarts), no duplicate pool entries.
	dir := t.TempDir()
	writeQoderPAT(t, dir, a)
	p := pool.NewWeightedRandomSelector(0, 3, 60)
	s := NewCredentialStore(p)
	if _, err := s.LoadAuthDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadAuthDir(dir); err != nil {
		t.Fatal(err)
	}
	if got := len(p.SnapshotAccounts()); got != 1 {
		t.Errorf("pool accounts after double load = %d, want 1 (AddAccount upsert)", got)
	}
}

// TestQoderPATNeverSerializes pins the security red line for the qoder
// credential: the PAT must not leak into the pool snapshot, the store
// serialization, the host, or a state-shaped document.
func TestQoderPATNeverSerializes(t *testing.T) {
	dir := t.TempDir()
	pat := fakePAT("ACCT1FAKE")
	writeQoderPAT(t, dir, pat)

	p := pool.NewWeightedRandomSelector(0, 3, 60)
	s := NewCredentialStore(p)
	if _, err := s.LoadAuthDir(dir); err != nil {
		t.Fatal(err)
	}

	accountsJSON, err := json.Marshal(p.SnapshotAccounts())
	if err != nil {
		t.Fatal(err)
	}
	storeJSON, err := json.Marshal(s) //nolint:staticcheck // SA9005 is the point: the store must serialize to "{}".
	if err != nil {
		t.Fatal(err)
	}
	hostJSON, err := json.Marshal(&Host{Pool: p, Creds: s})
	if err != nil {
		t.Fatal(err)
	}
	stateLike, err := json.Marshal(map[string]any{
		"accounts":    json.RawMessage(accountsJSON),
		"credentials": json.RawMessage(storeJSON),
	})
	if err != nil {
		t.Fatal(err)
	}
	for where, blob := range map[string]string{
		"pool snapshot":    string(accountsJSON),
		"credential store": string(storeJSON),
		"host":             string(hostJSON),
		"state document":   string(stateLike),
	} {
		if strings.Contains(blob, pat) {
			t.Errorf("%s: qoder PAT material leaked into serialization", where)
		}
	}
}

// TestQoderAccountSelectableWithoutExpiry pins the Select semantics: a
// zero-expiry qoder account is a normal candidate (Select filters on status,
// credit floor and cooldown — never on a zero ExpiresAt).
func TestQoderAccountSelectableWithoutExpiry(t *testing.T) {
	dir := t.TempDir()
	writeQoderPAT(t, dir, fakePAT("ACCT1FAKE"))

	p := pool.NewWeightedRandomSelector(0, 3, 60)
	s := NewCredentialStore(p)
	if _, err := s.LoadAuthDir(dir); err != nil {
		t.Fatal(err)
	}
	acct, err := p.Select(t.Context(), QoderPlatform)
	if err != nil || acct == nil {
		t.Fatalf("Select(qoder) = %v, %v — want the PAT account", acct, err)
	}
	if acct.Platform != QoderPlatform || !acct.ExpiresAt.IsZero() {
		t.Errorf("selected account = %+v", acct)
	}
}

// TestQoderCreditIdentitiesExcludedFromWorkbuddySeed: the workbuddy billing
// seed must not pick up qoder accounts (its endpoints cannot serve a PAT).
func TestQoderCreditIdentitiesExcludedFromWorkbuddySeed(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "uid-a", fakeAccessToken("ACCT1FAKE"), fakeRefreshToken("REF1FAKE"), 1794232800)
	writeQoderPAT(t, dir, fakePAT("ACCT1FAKE"))

	s := NewCredentialStore(pool.NewWeightedRandomSelector(0, 3, 60))
	if _, err := s.LoadAuthDir(dir); err != nil {
		t.Fatal(err)
	}
	ids := s.identitiesByPlatform(WorkbuddyPlatform)
	if len(ids) != 1 || ids[0].ID != "uid-a" {
		t.Errorf("workbuddy identities = %+v, want only uid-a", ids)
	}
}
