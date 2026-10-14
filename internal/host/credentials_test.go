package host

// Tests for the credential plane run exclusively against t.TempDir() replicas
// of the documented auth-file shape — the real auth dir on this machine is
// never read or written (task red line). Token values are fake but shaped
// like the real ones (JWT-ish ~1300 chars, opaque ~700 chars).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"all2api/internal/pool"
)

// fakeAccessToken mints a fake JWT-shaped access token (~1300 chars) around a
// distinctive seed. Structural mimicry only — no byte of it is real.
func fakeAccessToken(seed string) string {
	head := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9"    // {"alg":"RS256","typ":"JWT"}
	mid := "eyJpc3MiOiJmYWtlLmlzc3VlciJ9"             // {"iss":"fake.issuer"}
	tail := seed + strings.Repeat("A", 1150) + ".sig" // fake signature block
	return head + "." + mid + "." + tail
}

// fakeRefreshToken mints a fake opaque refresh token (~700 chars).
func fakeRefreshToken(seed string) string {
	return seed + strings.Repeat("R", 700)
}

// writeAuthFile creates one workbuddy-<uid>.json in dir with the documented
// real-world shape and returns its path.
func writeAuthFile(t *testing.T, dir, uid, access, refresh string, expiresAt int64) string {
	t.Helper()
	return writeAuthFileDoc(t, dir, uid, access, refresh, expiresAt, nil)
}

// writeAuthFileDoc is writeAuthFile with extra keys merged into the auth block
// (structure-preservation probes for write-back).
func writeAuthFileDoc(t *testing.T, dir, uid, access, refresh string, expiresAt int64, extraAuth map[string]any) string {
	t.Helper()
	auth := map[string]any{
		"accessToken":  access,
		"domain":       "www.codebuddy.cn",
		"expiresAt":    expiresAt,
		"realm":        "cn",
		"refreshToken": refresh,
	}
	for k, v := range extraAuth {
		auth[k] = v
	}
	doc := map[string]any{
		"account": map[string]any{
			"enterpriseId": "",
			"nickname":     "worker-" + uid,
			"uid":          uid,
		},
		"auth": auth,
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "workbuddy-"+uid+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAuthDirRegistersPoolAccounts(t *testing.T) {
	dir := t.TempDir()
	const (
		expA = int64(1794232800)
		expB = int64(1794232801)
	)
	writeAuthFile(t, dir, "uid-a", fakeAccessToken("ACCT1FAKE"), fakeRefreshToken("REF1FAKE"), expA)
	writeAuthFile(t, dir, "uid-b", fakeAccessToken("ACCT2FAKE"), fakeRefreshToken("REF2FAKE"), expB)

	p := pool.NewWeightedRandomSelector(100, 3, 60)
	s := NewCredentialStore(p)
	loaded, err := s.LoadAuthDir(dir)
	if err != nil {
		t.Fatalf("LoadAuthDir: %v", err)
	}
	if loaded != 2 || s.Count() != 2 {
		t.Fatalf("loaded = %d, count = %d, want 2/2", loaded, s.Count())
	}

	// Pool registration: active, secret-free, expiry carried over.
	snap := p.SnapshotAccounts()
	if len(snap) != 2 {
		t.Fatalf("pool accounts = %d, want 2", len(snap))
	}
	byID := map[string]pool.Account{}
	for _, a := range snap {
		byID[a.ID] = a
	}
	for id, want := range map[string]int64{"uid-a": expA, "uid-b": expB} {
		acc, ok := byID[id]
		if !ok {
			t.Fatalf("account %s missing from pool", id)
		}
		if acc.Platform != WorkbuddyPlatform || acc.Status != "active" {
			t.Errorf("account %s = %s/%s, want workbuddy/active", id, acc.Platform, acc.Status)
		}
		if acc.ExpiresAt.Unix() != want {
			t.Errorf("account %s expires = %d, want %d", id, acc.ExpiresAt.Unix(), want)
		}
	}

	// Store lookup (in-package: CredentialFor lives behind the workbuddy tag).
	cred, ok := s.lookup(credKey(WorkbuddyPlatform, "uid-a"))
	if !ok {
		t.Fatal("uid-a missing from store")
	}
	if cred.AccessToken != fakeAccessToken("ACCT1FAKE") || cred.RefreshToken != fakeRefreshToken("REF1FAKE") {
		t.Error("stored credential does not round-trip the file content")
	}
	if cred.Global {
		t.Error("realm cn must not set Global")
	}
	if cred.Domain != "www.codebuddy.cn" || cred.UID != "uid-a" {
		t.Errorf("identity fields wrong: uid=%q domain=%q", cred.UID, cred.Domain)
	}
	if !strings.HasSuffix(cred.Path, "workbuddy-uid-a.json") {
		t.Errorf("write-back path = %q, want the loaded file", cred.Path)
	}
}

func TestLoadAuthDirSkipsMalformedFiles(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "uid-a", fakeAccessToken("ACCT1FAKE"), fakeRefreshToken("REF1FAKE"), 1794232800)
	// broken json
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// missing uid
	noUID := map[string]any{"auth": map[string]any{"accessToken": fakeAccessToken("X"), "refreshToken": fakeRefreshToken("X")}}
	if data, err := json.Marshal(noUID); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(filepath.Join(dir, "workbuddy-nouid.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// missing access token
	noTok := map[string]any{"account": map[string]any{"uid": "uid-t"}, "auth": map[string]any{"refreshToken": fakeRefreshToken("X")}}
	if data, err := json.Marshal(noTok); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(filepath.Join(dir, "workbuddy-notoken.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// wrong prefix and a directory — both ignored
	if err := os.WriteFile(filepath.Join(dir, "other-uid-a.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "workbuddy-dir.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	p := pool.NewWeightedRandomSelector(100, 3, 60)
	s := NewCredentialStore(p)
	loaded, err := s.LoadAuthDir(dir)
	if err != nil {
		t.Fatalf("LoadAuthDir: %v", err)
	}
	if loaded != 1 || s.Count() != 1 {
		t.Fatalf("loaded = %d, count = %d, want 1/1 (only the well-formed file)", loaded, s.Count())
	}
	if got := p.SnapshotAccounts(); len(got) != 1 || got[0].ID != "uid-a" {
		t.Fatalf("pool = %+v, want only uid-a", got)
	}
}

func TestLoadAuthDirMissingDirGraceful(t *testing.T) {
	p := pool.NewWeightedRandomSelector(100, 3, 60)
	s := NewCredentialStore(p)
	loaded, err := s.LoadAuthDir(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing dir must degrade to (0, nil), got %v", err)
	}
	if loaded != 0 || s.Count() != 0 {
		t.Fatalf("loaded = %d, count = %d, want 0/0", loaded, s.Count())
	}
}

// TestStateSerializationNeverCarriesTokens pins the security red line: the
// pool snapshot, the store itself, the host and a state-shaped document all
// serialize without any token substring. If someone later adds a token field
// to pool.Account or exports the store's credentials, this fails first.
func TestStateSerializationNeverCarriesTokens(t *testing.T) {
	dir := t.TempDir()
	secretA := fakeAccessToken("ACCT1FAKE")
	secretR := fakeRefreshToken("REF1FAKE")
	writeAuthFile(t, dir, "uid-a", secretA, secretR, 1794232800)

	p := pool.NewWeightedRandomSelector(100, 3, 60)
	s := NewCredentialStore(p)
	if _, err := s.LoadAuthDir(dir); err != nil {
		t.Fatal(err)
	}

	accountsJSON, err := json.Marshal(p.SnapshotAccounts())
	if err != nil {
		t.Fatal(err)
	}
	storeJSON, err := json.Marshal(s) //nolint:staticcheck // SA9005 is the point: the store must serialize to "{}" — this line IS the red-line pin.
	if err != nil {
		t.Fatal(err)
	}
	hostJSON, err := json.Marshal(&Host{Pool: p, Creds: s})
	if err != nil {
		t.Fatal(err)
	}
	stateLike, err := json.Marshal(map[string]any{
		"accounts":      json.RawMessage(accountsJSON),
		"credentials":   json.RawMessage(storeJSON),
		"consent_trail": []string{"ack"},
	})
	if err != nil {
		t.Fatal(err)
	}

	blobs := map[string]string{
		"pool snapshot":    string(accountsJSON),
		"credential store": string(storeJSON),
		"host":             string(hostJSON),
		"state document":   string(stateLike),
	}
	for where, blob := range blobs {
		for name, secret := range map[string]string{"access token": secretA, "refresh token": secretR, "seed": "ACCT1FAKE"} {
			if strings.Contains(blob, secret) {
				t.Errorf("%s: %s material leaked into serialization", where, name)
			}
		}
	}
}

func TestWriteBackReplacesTokensAndPreservesStructure(t *testing.T) {
	dir := t.TempDir()
	const (
		oldAccess  = "OLD-ACCESS-FAKE"
		oldRefresh = "OLD-REFRESH-FAKE"
	)
	path := writeAuthFileDoc(t, dir, "uid-a", oldAccess, oldRefresh, 1794232800,
		map[string]any{"stickyNote": "keep-me"}) // unknown field: must survive

	p := pool.NewWeightedRandomSelector(100, 3, 60)
	s := NewCredentialStore(p)
	if loaded, err := s.LoadAuthDir(dir); err != nil || loaded != 1 {
		t.Fatalf("LoadAuthDir = %d, %v", loaded, err)
	}
	cred, _ := s.lookup(credKey(WorkbuddyPlatform, "uid-a"))
	cred.AccessToken = fakeAccessToken("NEWACC")
	cred.RefreshToken = fakeRefreshToken("NEWREF")
	cred.ExpiresAt = time.Unix(1900000000, 0)
	if err := s.writeBack(credKey(WorkbuddyPlatform, "uid-a"), cred); err != nil {
		t.Fatalf("writeBack: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, oldAccess) || strings.Contains(text, oldRefresh) {
		t.Error("old token material survived write-back")
	}
	if !strings.Contains(text, fakeAccessToken("NEWACC")) || !strings.Contains(text, fakeRefreshToken("NEWREF")) {
		t.Error("rotated tokens missing from written file")
	}
	if !strings.Contains(text, "keep-me") {
		t.Error("unknown auth fields were dropped by write-back")
	}
	if !strings.Contains(text, "worker-uid-a") || !strings.Contains(text, `"realm": "cn"`) {
		t.Error("account block / untouched auth fields not preserved")
	}

	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("written file is not JSON: %v", err)
	}
	auth := doc["auth"].(map[string]any)
	if auth["accessToken"] != fakeAccessToken("NEWACC") || auth["refreshToken"] != fakeRefreshToken("NEWREF") {
		t.Error("auth block not updated in place")
	}
	if auth["expiresAt"] != float64(1900000000) {
		t.Errorf("expiresAt = %v, want 1900000000", auth["expiresAt"])
	}

	// Atomic swap: no temp leftovers in the auth dir.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestWriteBackWithoutRecordedPathErrors(t *testing.T) {
	s := NewCredentialStore(nil)
	err := s.writeBack(WorkbuddyPlatform+"/ghost", fileCred{UID: "ghost"})
	if err == nil {
		t.Fatal("expected an explicit error, not silent success")
	}
	if !strings.Contains(err.Error(), "no auth file path") {
		t.Errorf("err = %v, want the explicit no-path message", err)
	}
}
