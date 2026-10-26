package qoder

import (
	"testing"

	"all2api/internal/adapter"
)

// TestNormalizeDevicePair pins the rotation reply shapes seen across planes
// and versions: flat dt- fields, the data-nested variant, session-shaped
// tokens that must NOT become the stored device credential, and the
// keep-old-refresh fallback.
func TestNormalizeDevicePair(t *testing.T) {
	// Flat, canonical: a fresh dt- plus a rotated drt-.
	dt, rt, _ := normalizeDevicePair(map[string]any{
		"token": "dt-new", "refresh_token": "drt-new",
	}, "drt-old")
	if dt != "dt-new" || rt != "drt-new" {
		t.Fatalf("flat: dt=%q rt=%q", dt, rt)
	}

	// Data-nested variant.
	dt, rt, _ = normalizeDevicePair(map[string]any{
		"data": map[string]any{
			"accessToken": "dt-nested", "refreshToken": "drt-nested",
			"expireTime": float64(123456),
		},
	}, "drt-old")
	if dt != "dt-nested" || rt != "drt-nested" {
		t.Fatalf("nested: dt=%q rt=%q", dt, rt)
	}

	// Session-shaped reply: the jt- token must not be taken as the device
	// credential; the refresh token still rotates.
	dt, rt, _ = normalizeDevicePair(map[string]any{
		"token": "jt-session", "refresh_token": "drt-rotated",
	}, "drt-old")
	if dt != "" {
		t.Fatalf("session token leaked into the device credential: %q", dt)
	}
	if rt != "drt-rotated" {
		t.Fatalf("rt = %q, want the rotated value", rt)
	}

	// Missing refresh keeps the old one.
	dt, rt, _ = normalizeDevicePair(map[string]any{"device_token": "dt-only"}, "drt-keep")
	if dt != "dt-only" || rt != "drt-keep" {
		t.Fatalf("fallback: dt=%q rt=%q", dt, rt)
	}
}

// TestBuildParametersClampsMaxTokens pins the upstream cap (community issue
// #101: "Range of max_tokens should be [1, 131072]" — larger values are a
// hard 400): the gateway clamps instead of relaying a doomed request.
func TestBuildParametersClampsMaxTokens(t *testing.T) {
	over := 200000
	req := adapter.ChatRequest{MaxTokens: &over}
	p, err := buildParameters(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(p) != `{"max_tokens":131072}` {
		t.Fatalf("parameters = %s, want the clamped max_tokens", p)
	}
	// At-or-under the cap passes through untouched.
	at := 131072
	p, err = buildParameters(adapter.ChatRequest{MaxTokens: &at})
	if err != nil {
		t.Fatal(err)
	}
	if string(p) != `{"max_tokens":131072}` {
		t.Fatalf("parameters = %s, want the at-cap value verbatim", p)
	}
}
