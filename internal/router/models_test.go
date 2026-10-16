package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"all2api/internal/config"
)

func TestV1ModelsListsCatalog(t *testing.T) {
	rt := &Router{models: func(context.Context) []PublicModel {
		return []PublicModel{
			{ID: "DeepSeek-V4-Pro", OwnedBy: "qoder"},
			{ID: "deepseek-v4-flash", OwnedBy: "workbuddy"},
			{ID: "qoder/DeepSeek-V4-Pro", OwnedBy: "qoder"},
			{ID: "workbuddy/deepseek-v4-pro", OwnedBy: "workbuddy"},
			// An empty id row must be dropped, not served as "".
			{ID: "", OwnedBy: "qoder"},
		}
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", rt.handleV1Models)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got modelListWire
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Object != "list" {
		t.Fatalf("object = %q, want list", got.Object)
	}
	if len(got.Data) != 4 {
		t.Fatalf("data rows = %d, want 4 (empty id dropped)", len(got.Data))
	}
	m := got.Data[0]
	if m.ID != "DeepSeek-V4-Pro" || m.Object != "model" || m.OwnedBy != "qoder" || m.Created == 0 {
		t.Fatalf("row0 = %+v, want id/object/created/owned_by populated", m)
	}
}

func TestV1ModelsEnforcesAPIKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`{"listen":":0","api_key":"secret-k"}`), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := config.Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(func() {
		// config.Shared is process-global: restore an explicit keyless config
		// so later tests don't inherit this one's api_key.
		empty := filepath.Join(dir, "no-key.json")
		if err := os.WriteFile(empty, []byte(`{"listen":":0"}`), 0o600); err != nil {
			t.Fatalf("restore config: %v", err)
		}
		_ = config.Load(empty)
	})

	rt := &Router{models: func(context.Context) []PublicModel { return nil }}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", rt.handleV1Models)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without key = %d, want 401", resp.StatusCode)
	}

	req, err := http.NewRequestWithContext(context.Background(), "GET", srv.URL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret-k")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET with key: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status with key = %d, want 200", resp2.StatusCode)
	}
}

func TestV1ModelsNilListerYieldsEmptyList(t *testing.T) {
	// No rt.models and a nil DefaultModelLister: the endpoint still answers
	// 200 with an empty OpenAI list — the surface exists, the catalog is
	// simply unwired (fresh-router stance).
	old := DefaultModelLister
	DefaultModelLister = nil
	t.Cleanup(func() { DefaultModelLister = old })

	rt := &Router{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", rt.handleV1Models)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got modelListWire
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Object != "list" || len(got.Data) != 0 {
		t.Fatalf("body = %+v, want an empty list", got)
	}
}
