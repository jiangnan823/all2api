package api

// M10-B2 row-inline lifecycle endpoint tests (disable/enable): wire shape,
// pool state transitions, the enable-does-not-clear-cooldown ruling, and the
// 400/404 path parity with the other "{platform}:{id}" write routes.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"all2api/internal/host"
	"all2api/internal/pool"
)

func newLifecycleMux(t *testing.T, h *host.Host) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	RegisterPanelData(mux, h, nil, nil, nil)
	return mux
}

func postLifecycle(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestDisableEndpointSetsStatus(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t, "qa")
	h.Creds = host.NewCredentialStore(h.Pool)
	h.Pool.AddAccount(&pool.Account{ID: "acc-1", Platform: "qa", Status: "active", Credit: 500})

	rec := postLifecycle(t, newLifecycleMux(t, h), "/api/accounts/qa:acc-1/disable")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"status":"disabled"`) {
		t.Fatalf("disable body must echo the status: %s", rec.Body)
	}
	if got := h.Pool.SnapshotAccounts()[0].Status; got != "disabled" {
		t.Fatalf("pool status = %q, want disabled", got)
	}
}

func TestEnableEndpointSetsActiveButKeepsCooldown(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t, "qa")
	h.Creds = host.NewCredentialStore(h.Pool)
	h.Pool.AddAccount(&pool.Account{ID: "acc-1", Platform: "qa", Status: "disabled", Credit: 500})
	// A running 429 backoff must survive enable — recover owns that unlock.
	h.Pool.SetAccountCooldown("qa", "acc-1", time.Now().Add(time.Minute))

	rec := postLifecycle(t, newLifecycleMux(t, h), "/api/accounts/qa:acc-1/enable")
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status = %d (body %s)", rec.Code, rec.Body)
	}
	acc := h.Pool.SnapshotAccounts()[0]
	if acc.Status != "active" {
		t.Fatalf("pool status = %q, want active", acc.Status)
	}
	if !acc.IsCooling() {
		t.Fatal("enable must NOT clear a running cooldown (recover's job)")
	}
}

func TestLifecyclePathErrors(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t, "qa")
	h.Creds = host.NewCredentialStore(h.Pool)
	mux := newLifecycleMux(t, h)

	// Malformed segment → 400 (splitAccountID convention).
	for _, path := range []string{
		"/api/accounts/no-colon/disable",
		"/api/accounts/:id/enable",
		"/api/accounts/qa:/disable",
	} {
		if rec := postLifecycle(t, mux, path); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", path, rec.Code)
		}
	}
	// Unknown account → 404.
	if rec := postLifecycle(t, mux, "/api/accounts/qa:ghost/disable"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account disable = %d, want 404", rec.Code)
	}
	if rec := postLifecycle(t, mux, "/api/accounts/qa:ghost/enable"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account enable = %d, want 404", rec.Code)
	}
}

func TestLifecycleNilHostRegistersNothing(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	mux := newLifecycleMux(t, nil)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/accounts/qa:a/disable", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nil host must leave the route unregistered (mux 404), got %d", rec.Code)
	}
}
