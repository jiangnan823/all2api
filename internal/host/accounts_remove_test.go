package host

// M6a remove/pause tests: the credential FILE is deleted before any in-memory
// state clears (a failed delete must leave store + pool intact so the panel
// can retry), the store entry and pool account disappear together, an unknown
// account reports ErrAccountUnknown, and SetPaused flips the pool switch.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"all2api/internal/pool"
)

// removeFixture loads one workbuddy auth file plus one qoder PAT file from a
// temp auth dir into a fresh store over a fresh pool.
func removeFixture(t *testing.T) (*CredentialStore, *pool.WeightedRandomSelector, string) {
	t.Helper()
	dir := t.TempDir()
	body := `{"account":{"uid":"wb-del","nickname":"del"},"auth":{"accessToken":"tok","expiresAt":0,"realm":"cn"}}`
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-wb-del.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("seed workbuddy auth file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, qoderPatFile), []byte("pt-removetest\n"), 0o600); err != nil {
		t.Fatalf("seed pat file: %v", err)
	}
	sel := pool.NewWeightedRandomSelector(0, 3, 60)
	store := NewCredentialStore(sel)
	if loaded, err := store.LoadAuthDir(dir); err != nil || loaded != 2 {
		t.Fatalf("LoadAuthDir: loaded=%d err=%v, want 2/nil", loaded, err)
	}
	return store, sel, dir
}

func poolHasAccount(sel *pool.WeightedRandomSelector, platform, id string) bool {
	for _, acc := range sel.SnapshotAccounts() {
		if acc.Platform == platform && acc.ID == id {
			return true
		}
	}
	return false
}

func TestRemoveAccountWorkbuddyDeletesFileStorePool(t *testing.T) {
	store, sel, dir := removeFixture(t)
	path := filepath.Join(dir, "workbuddy-wb-del.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture file must exist before remove: %v", err)
	}

	if err := store.RemoveAccount(WorkbuddyPlatform, "wb-del"); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("credential file must be deleted, stat err = %v", err)
	}
	if got := store.CountPlatform(WorkbuddyPlatform); got != 0 {
		t.Fatalf("store entries = %d, want 0 for workbuddy", got)
	}
	if got := store.Count(); got != 1 {
		t.Fatalf("store count = %d, want 1 (qoder credential untouched)", got)
	}
	if poolHasAccount(sel, WorkbuddyPlatform, "wb-del") {
		t.Fatalf("pool account must be removed")
	}
	// Removing an already-removed account is the 404 case, not a silent ok.
	if err := store.RemoveAccount(WorkbuddyPlatform, "wb-del"); !errors.Is(err, ErrAccountUnknown) {
		t.Fatalf("double remove err = %v, want ErrAccountUnknown", err)
	}
}

func TestRemoveAccountQoderDeletesSharedPatFile(t *testing.T) {
	store, sel, dir := removeFixture(t)
	uid := patFingerprint("pt-removetest")

	if err := store.RemoveAccount(QoderPlatform, uid); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, qoderPatFile)); !os.IsNotExist(err) {
		t.Fatalf("qoder-pat.txt must be deleted, stat err = %v", err)
	}
	if got := store.Count(); got != 1 {
		t.Fatalf("store count = %d, want 1 (workbuddy credential untouched)", got)
	}
	if poolHasAccount(sel, QoderPlatform, uid) {
		t.Fatalf("pool account must be removed")
	}
	// The auth dir no longer re-registers the qoder account on the next
	// start; the untouched workbuddy file still loads.
	fresh := NewCredentialStore(pool.NewWeightedRandomSelector(0, 3, 60))
	if loaded, err := fresh.LoadAuthDir(dir); err != nil || loaded != 1 {
		t.Fatalf("post-remove LoadAuthDir: loaded=%d err=%v, want 1/nil (workbuddy only)", loaded, err)
	}
}

func TestRemoveAccountUnknownIsSentinel(t *testing.T) {
	store, _, _ := removeFixture(t)
	err := store.RemoveAccount(WorkbuddyPlatform, "wb-ghost")
	if !errors.Is(err, ErrAccountUnknown) {
		t.Fatalf("err = %v, want ErrAccountUnknown", err)
	}
	err = store.RemoveAccount("trae", "wb-del") // wrong platform namespace
	if !errors.Is(err, ErrAccountUnknown) {
		t.Fatalf("cross-platform err = %v, want ErrAccountUnknown", err)
	}
}

func TestRemoveAccountToleratesVanishedFile(t *testing.T) {
	store, sel, dir := removeFixture(t)
	// The file is already gone (manual deletion): the requested end state is
	// reached, so remove must succeed and still clear the memory side.
	if err := os.Remove(filepath.Join(dir, "workbuddy-wb-del.json")); err != nil {
		t.Fatalf("manual pre-remove: %v", err)
	}
	if err := store.RemoveAccount(WorkbuddyPlatform, "wb-del"); err != nil {
		t.Fatalf("RemoveAccount over a vanished file must converge, got %v", err)
	}
	if store.CountPlatform(WorkbuddyPlatform) != 0 || poolHasAccount(sel, WorkbuddyPlatform, "wb-del") {
		t.Fatalf("memory side must be cleared even when the file was already gone")
	}
}

func TestRemoveAccountFileFailureKeepsMemory(t *testing.T) {
	sel := pool.NewWeightedRandomSelector(0, 3, 60)
	store := NewCredentialStore(sel)
	// A non-empty directory stands in for an undeletable file: os.Remove on a
	// directory that still holds an entry fails on every platform — the
	// "file delete failed" contract case.
	stuck := t.TempDir()
	if err := os.WriteFile(filepath.Join(stuck, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatalf("occupy dir: %v", err)
	}
	store.put(credKey(WorkbuddyPlatform, "wb-stuck"), fileCred{UID: "wb-stuck", Path: stuck})
	store.pool.AddAccount(&pool.Account{ID: "wb-stuck", Platform: WorkbuddyPlatform, Status: "active"})

	err := store.RemoveAccount(WorkbuddyPlatform, "wb-stuck")
	if err == nil || errors.Is(err, ErrAccountUnknown) {
		t.Fatalf("undeletable target must return a real error, got %v", err)
	}
	if store.CountPlatform(WorkbuddyPlatform) != 1 {
		t.Fatalf("store entry must survive a failed file delete (retryable state)")
	}
	if !poolHasAccount(sel, WorkbuddyPlatform, "wb-stuck") {
		t.Fatalf("pool account must survive a failed file delete")
	}
}

func TestStoreSetPausedFlipsPoolSwitch(t *testing.T) {
	store, sel, _ := removeFixture(t)

	if !store.SetPaused(WorkbuddyPlatform, "wb-del", true) {
		t.Fatalf("pause: want true (account known)")
	}
	for _, acc := range sel.SnapshotAccounts() {
		if acc.Platform == WorkbuddyPlatform && !acc.Paused {
			t.Fatalf("workbuddy account must read back paused=true, got %+v", acc)
		}
	}
	if !store.SetPaused(WorkbuddyPlatform, "wb-del", false) {
		t.Fatalf("resume: want true")
	}
	for _, acc := range sel.SnapshotAccounts() {
		if acc.Platform == WorkbuddyPlatform && acc.Paused {
			t.Fatalf("resumed account must read back paused=false")
		}
	}
	if store.SetPaused(QoderPlatform, "ghost", true) {
		t.Fatalf("unknown account must report false")
	}
}
