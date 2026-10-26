//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

package workbuddy

import (
	"net/http"
	"regexp"
	"testing"
)

var (
	hyphenUUID7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	bareUUID7   = regexp.MustCompile(`^[0-9a-f]{12}7[0-9a-f]{3}[89ab][0-9a-f]{3}[0-9a-f]{12}$`)
	spanHex16   = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// TestChatIdentityHeaderFamily pins the desktop identity ported from the
// community reference: product/agent headers, the conversation id pairings
// (turn id shared by Request/Root, message id shared by Message/Request) and
// the trace family consistency.
func TestChatIdentityHeaderFamily(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	setChatIdentityHeaders(req, "uid-fixture")
	h := req.Header

	for k, want := range map[string]string{
		"X-Agent-Intent":  "craft",
		"X-Agent-Purpose": "conversation",
		"X-Agent-Type":    "main",
		"X-IDE-Name":      "WorkBuddy",
		"X-IDE-Type":      "WorkBuddy",
		"X-IDE-Version":   wireClientVersion,
		"X-Private-Data":  "true",
		"X-Product":       "SaaS",
		"X-B3-Sampled":    "1",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	if !hyphenUUID7.MatchString(h.Get("X-Conversation-ID")) {
		t.Errorf("X-Conversation-ID = %q, want hyphenated UUIDv7", h.Get("X-Conversation-ID"))
	}
	turn := h.Get("X-Conversation-Request-ID")
	if !bareUUID7.MatchString(turn) {
		t.Errorf("X-Conversation-Request-ID = %q, want bare UUIDv7", turn)
	}
	if h.Get("X-Root-Request-ID") != turn {
		t.Errorf("X-Root-Request-ID must share the turn id")
	}
	msg := h.Get("X-Conversation-Message-ID")
	if !bareUUID7.MatchString(msg) {
		t.Errorf("X-Conversation-Message-ID = %q, want bare UUIDv7", msg)
	}
	if h.Get("X-Request-ID") != msg {
		t.Errorf("X-Request-ID must share the message id")
	}

	trace := h.Get("X-Trace-ID")
	if !bareUUID7.MatchString(trace) {
		t.Errorf("X-Trace-ID = %q, want bare UUIDv7", trace)
	}
	span := h.Get("X-B3-SpanId")
	if !spanHex16.MatchString(span) {
		t.Errorf("X-B3-SpanId = %q, want 16 hex", span)
	}
	if h.Get("X-B3-TraceId") != trace {
		t.Errorf("X-B3-TraceId must equal X-Trace-ID")
	}
	// Direct map reads: the lowercase wire keys are deliberately
	// non-canonical (Set would uppercase them).
	if tp := h["traceparent"]; len(tp) != 1 || tp[0] != "00-"+trace+"-"+span+"-01" {
		t.Errorf("traceparent = %v, want the W3C shape over the same trace/span", tp)
	}
	if b3 := h["b3"]; len(b3) != 1 || b3[0] != trace+"-"+span+"-1" {
		t.Errorf("b3 = %v, want the single-header shape", b3)
	}

	// Stable device fingerprints: present, 36-hex, and identical for the
	// same uid across requests while differing per uid.
	machine := h.Get("X-Machine-ID")
	sessionID := h.Get("X-Session-ID")
	if len(machine) != 36 || len(sessionID) != 36 || machine == sessionID {
		t.Fatalf("machine/session = %q/%q, want two distinct 36-hex ids", machine, sessionID)
	}
	req1b, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	setChatIdentityHeaders(req1b, "uid-fixture")
	if req1b.Header.Get("X-Machine-ID") != machine || req1b.Header.Get("X-Session-ID") != sessionID {
		t.Error("stable headers must be identical for the same uid across requests")
	}
	reqOther, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	setChatIdentityHeaders(reqOther, "uid-other")
	if reqOther.Header.Get("X-Machine-ID") == machine {
		t.Error("different uids must produce different machine ids")
	}

	// A second application must produce fresh ids (per-request generation).
	req2, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	setChatIdentityHeaders(req2, "uid-fixture")
	if req2.Header.Get("X-Conversation-ID") == h.Get("X-Conversation-ID") {
		t.Error("conversation id must be generated per request")
	}
	if req2.Header.Get("X-Trace-ID") == trace {
		t.Error("trace id must be generated per request")
	}
	if !hyphenUUID7.MatchString(req2.Header.Get("X-Conversation-ID")) {
		t.Error("second conversation id malformed")
	}
}
