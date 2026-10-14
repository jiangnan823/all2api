package api

// M2 B2 wiring tests: GET /api/time contract shape, deterministic configfields
// delivery order, min_gap_ms single array-valued key, jitter per-source
// filter, and always-present throttle wire keys (omitempty retired per
// PM ruling 3). Failsafe-before-fix order property: ten byte-identical
// deliveries cannot come from a randomised map ranging.

import (
	"bytes"
	"context"
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
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
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
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/time", nil)
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

// B2b: §5.20 range sign-off v0.1 landing. The FieldDef min/max delivered here
// is the SINGLE source of truth for editable throttle bounds — lower bounds
// adopt the backend values unchanged, four upper bounds narrowed (steady
// 10000→120, burst 10000→300, daily_cap 1000000→10000, inflight 64→32).
// Every throttle column also carries a description copy KEY (never inline
// text, UI §5.16) whose name must match fronted's zh.ts table 1:1.
func TestThrottleSignoffBoundsExact(t *testing.T) {
	loadB2Config(t, `{"listen":":0"}`)
	groups := decodeGroups(t, doGet(t, "/api/config/fields"))

	type bound struct {
		key  string
		min  float64
		max  float64
		desc string
	}
	cols := []bound{
		{"steady_rpm", 1, 120, "config.field.steady_rpm.desc"},
		{"burst_rpm", 1, 300, "config.field.burst_rpm.desc"},
		{"min_gap_ms", 0, 60000, "config.field.min_gap_ms.desc"},
		{"daily_cap", 1, 10000, "config.field.daily_cap.desc"},
		{"inflight", 1, 32, "config.field.inflight.desc"},
		{"backoff.base_429_sec", 600, 86400, "config.field.base_429_sec.desc"},
		{"backoff.not_found_sec", 0, 3600, "config.field.not_found_sec.desc"},
	}
	for _, id := range []string{"qoder", "trae", "workbuddy"} {
		for _, c := range cols {
			f := groupField(t, groups, "sources_throttle."+id, "sources."+id+".throttle."+c.key)
			if f == nil {
				t.Fatalf("%s.%s must be delivered", id, c.key)
			}
			if f.Min == nil || *f.Min != c.min || f.Max == nil || *f.Max != c.max {
				t.Fatalf("%s.%s bound drift: got [%v,%v], want [%v,%v] (sign-off v0.1 row)",
					id, c.key, deref(f.Min), deref(f.Max), c.min, c.max)
			}
			if f.Description != c.desc {
				t.Fatalf("%s.%s description must be the sign-off copy key %q, got %q",
					id, c.key, c.desc, f.Description)
			}
		}
	}
	// jitter is source-filtered (B2 ruling 6): present with signed bounds only
	// for the signin sources, absent for trae (coverage in
	// TestJitterFieldDefSourceFilter).
	for _, id := range []string{"workbuddy", "qoder"} {
		f := groupField(t, groups, "sources_throttle."+id, "sources."+id+".throttle.jitter_minutes")
		if f == nil {
			t.Fatalf("%s jitter must be delivered", id)
		}
		if f.Min == nil || *f.Min != 0 || f.Max == nil || *f.Max != 120 {
			t.Fatalf("jitter_minutes bound drift: got [%v,%v], want [0,120]", deref(f.Min), deref(f.Max))
		}
		if f.Description != "config.field.jitter_minutes.desc" {
			t.Fatalf("jitter description must be config.field.jitter_minutes.desc, got %q", f.Description)
		}
	}
}

func deref(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}

// Sign-off test point ④: boundary pairs against the delivered FieldDef bounds
// (in-range max accepted, max+1 rejected as "range"). The assertion is placed
// on the bound authority itself so frontend and backend boundary tables can
// never diverge — the validation predicate mirrors what both sides must use.
func TestThrottleBoundaryPairs(t *testing.T) {
	loadB2Config(t, `{"listen":":0"}`)
	groups := decodeGroups(t, doGet(t, "/api/config/fields"))

	inRange := func(f *fieldDef, v float64) bool {
		return f.Min != nil && f.Max != nil && v >= *f.Min && v <= *f.Max
	}
	// {field, atMax must pass, overMax must fail} — 120/121, 300/301, 10000/10001, 32/33.
	pairs := []struct {
		key     string
		atMax   float64
		overMax float64
	}{
		{"steady_rpm", 120, 121},
		{"burst_rpm", 300, 301},
		{"daily_cap", 10000, 10001},
		{"inflight", 32, 33},
	}
	for _, pr := range pairs {
		f := groupField(t, groups, "sources_throttle.qoder", "sources.qoder.throttle."+pr.key)
		if f == nil {
			t.Fatalf("%s missing", pr.key)
		}
		if !inRange(f, pr.atMax) {
			t.Fatalf("%s: %v is the signed-off max and must be valid", pr.key, pr.atMax)
		}
		if inRange(f, pr.overMax) {
			t.Fatalf("%s: %v exceeds the signed-off max and must be rejected (code=range)", pr.key, pr.overMax)
		}
	}
	// Narrowing must not have swallowed any recommended default (config.go
	// defaultThrottle): WB 6/20/600/2, Qoder 4/12/300/1, Trae 2/6/100/1.
	for _, id := range []string{"qoder", "trae", "workbuddy"} {
		th := config.Shared.Get().Sources[id].Throttle
		for _, v := range []struct {
			key string
			val float64
		}{
			{"steady_rpm", float64(th.SteadyRPM)},
			{"burst_rpm", float64(th.BurstRPM)},
			{"daily_cap", float64(th.DailyCap)},
			{"inflight", float64(th.Inflight)},
		} {
			f := groupField(t, groups, "sources_throttle."+id, "sources."+id+".throttle."+v.key)
			if f == nil || !inRange(f, v.val) {
				t.Fatalf("built-in default %s.%s=%v must stay inside the narrowed bounds", id, v.key, v.val)
			}
		}
	}
}
