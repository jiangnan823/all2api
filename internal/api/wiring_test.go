package api

// M2 B2 wiring tests: GET /api/time contract shape, deterministic configfields
// delivery order, min_gap_ms single array-valued key, jitter per-source
// filter, and always-present throttle wire keys (omitempty retired per
// PM ruling 3). Failsafe-before-fix order property: ten byte-identical
// deliveries cannot come from a randomised map ranging.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"all2api/internal/config"
)

func loadB2Config(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := config.Load(path); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
}

func doGet(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	Register(mux, nil) // nil host: recover route irrelevant for these surfaces
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeGroups(t *testing.T, rec *httptest.ResponseRecorder) []configGroup {
	t.Helper()
	var groups []configGroup
	if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil {
		t.Fatalf("decode ConfigGroup list: %v (body %s)", err, rec.Body)
	}
	return groups
}

func groupField(t *testing.T, groups []configGroup, group, key string) *fieldDef {
	t.Helper()
	for _, g := range groups {
		if g.Group != group {
			continue
		}
		for i := range g.Fields {
			if g.Fields[i].Key == key {
				return &g.Fields[i]
			}
		}
	}
	return nil
}

func TestServerTimeEndpoint(t *testing.T) {
	loadB2Config(t, `{"listen":":0"}`)

	rec := doGet(t, "/api/time")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/time: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var resp serverTimeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode ServerTimeResponse: %v", err)
	}
	if drift := time.Now().Unix() - resp.Now; drift < 0 || drift > 5 {
		t.Fatalf("server clock drift out of bound: %d", drift)
	}
	if _, err := time.Parse(localLayout, resp.Local); err != nil {
		t.Fatalf("local not in TimeLocal layout %q: %q (%v)", localLayout, resp.Local, err)
	}
	if strings.TrimSpace(resp.Timezone) == "" {
		t.Fatal("timezone annotation must be non-empty")
	}

	mux := http.NewServeMux()
	Register(mux, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/time", nil)
	recPost := httptest.NewRecorder()
	mux.ServeHTTP(recPost, req)
	if recPost.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/time: got %d, want 405", recPost.Code)
	}
}

func TestConfigFieldsDeterministicDelivery(t *testing.T) {
	// Built-in defaults configure workbuddy/qoder/trae — sorted ids: qoder, trae, workbuddy.
	loadB2Config(t, `{"listen":":0"}`)

	first := doGet(t, "/api/config/fields")
	if first.Code != http.StatusOK {
		t.Fatalf("GET /api/config/fields: got %d", first.Code)
	}
	firstBody := first.Body.Bytes()
	for i := 1; i <= 9; i++ {
		again := doGet(t, "/api/config/fields")
		if !bytes.Equal(firstBody, again.Body.Bytes()) {
			t.Fatalf("delivery %d differs from delivery 0 — order is not deterministic", i)
		}
	}

	var want []string
	want = append(want, "gateway", "routing", "pooling", "schedule")
	for _, id := range []string{"qoder", "trae", "workbuddy"} {
		want = append(want, "sources_throttle."+id, "danger."+id)
	}
	got := make([]string, 0, len(want))
	for _, g := range decodeGroups(t, first) {
		got = append(got, g.Group)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("group order:\n got %v\nwant %v", got, want)
	}
}

func TestMinGapMsSingleArrayKeyNoSplitKeys(t *testing.T) {
	loadB2Config(t, `{"listen":":0"}`)
	groups := decodeGroups(t, doGet(t, "/api/config/fields"))

	var gap *fieldDef
	for _, g := range groups {
		for i := range g.Fields {
			f := &g.Fields[i]
			if strings.HasSuffix(f.Key, ".lo") || strings.HasSuffix(f.Key, ".hi") {
				t.Fatalf("split key %q must not be delivered after B2 ruling 2", f.Key)
			}
			if f.Key == "sources.qoder.throttle.min_gap_ms" {
				gap = f
			}
		}
	}
	if gap == nil {
		t.Fatal("min_gap_ms must be delivered as one array-valued field")
	}
	if gap.Type != "number" || gap.Unit != "ms" {
		t.Fatalf("min_gap_ms must stay type=number unit=ms inside the frozen enum: %+v", gap)
	}
	if gap.Min == nil || *gap.Min != 0 || gap.Max == nil || *gap.Max != 60000 {
		t.Fatalf("min/max must bound both [lo,hi] components: %+v", gap)
	}
	if gap.Description == "" {
		t.Fatal("array shape must be documented via the description copy key")
	}
}

func TestJitterFieldDefSourceFilter(t *testing.T) {
	loadB2Config(t, `{"listen":":0"}`)
	groups := decodeGroups(t, doGet(t, "/api/config/fields"))

	const jk = "sources.%s.throttle.jitter_minutes"
	for _, id := range []string{"workbuddy", "qoder"} { // signin-posture sources
		if groupField(t, groups, "sources_throttle."+id, fmt.Sprintf(jk, id)) == nil {
			t.Fatalf("%s must expose jitter_minutes (B2 ruling 6)", id)
		}
	}
	if groupField(t, groups, "sources_throttle.trae", fmt.Sprintf(jk, "trae")) != nil {
		t.Fatal("trae (no timed signin task) must not expose jitter_minutes")
	}
}

func TestThrottleWireKeysAlwaysPresent(t *testing.T) {
	// Zero-valued jitter/halve physically vanished from the wire pre-B2
	// (omitempty). B2 ruling 3: response always carries them...
	loadB2Config(t, `{"listen":":0","sources":{"trae":{"enabled":true,"throttle":{"steady_rpm":0,"burst_rpm":0,"min_gap_ms":[0,0],"daily_cap":0,"inflight":0,"backoff":{"base_429_sec":0,"not_found_sec":0,"exhaust_402_until":""},"jitter_minutes":0,"halve_budget_after_429":false}}}}`)
	b, err := json.Marshal(config.Shared.Get().Sources["trae"].Throttle)
	if err != nil {
		t.Fatalf("marshal throttle: %v", err)
	}
	for _, key := range []string{`"jitter_minutes"`, `"halve_budget_after_429"`} {
		if !bytes.Contains(b, []byte(key)) {
			t.Fatalf("zero-valued %s must still be on the wire (B2 ruling 3): %s", key, b)
		}
	}
	// ...while the contract absent-defaults keep working on the READ side:
	// a file that never mentions jitter_minutes (no "sources" key at all —
	// note encoding/json replaces whole map entries, it does not merge them)
	// still yields the built-in default 15.
	loadB2Config(t, `{"listen":":0"}`)
	if got := config.Shared.Get().Sources["workbuddy"].Throttle.JitterMinutes; got != 15 {
		t.Fatalf("absent jitter_minutes must keep the contract default 15 on read, got %d", got)
	}
}
