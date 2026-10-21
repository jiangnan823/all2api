//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// identity.go ports the desktop client's outbound chat identity — the header
// family the official WorkBuddy desktop app sends on /v2/chat/completions —
// verbatim from the community-maintained reference (workbuddy2api-hub
// wb_identity.py, mitmproxy-verified against 5.8.x). The gateway's original
// minimal fingerprint (auth + brand headers only) predated the 2026-10-15/16
// risk-control tightening: upstream now grades request shape per client, and
// a request without the product/agent/conversation identity family reads as
// a script, not the app.
package workbuddy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"
)

// uuid7 renders an RFC 9562 UUIDv7 (48-bit millisecond timestamp, version 7,
// variant 10) in hyphenated form — the 5.8.x desktop client's
// X-Conversation-ID shape.
func uuid7() string {
	ms := time.Now().UnixMilli() & ((1 << 48) - 1)
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// newMessageID renders a UUIDv7 as bare 32-hex — the client's
// request/message id shape.
func newMessageID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	ms := time.Now().UnixMilli() & ((1 << 48) - 1)
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:])
}

// setChatIdentityHeaders applies the desktop chat identity on top of the
// common/account headers, 5.8.x capture shape. Header semantics per the
// reference: X-Conversation-ID is a hyphenated UUID (new per request on the
// desktop identity); X-Conversation-Request-ID and X-Root-Request-ID share
// one per-turn value while X-Conversation-Message-ID and X-Request-ID share
// a per-request one; the trace family carries the same 32-hex trace id
// across X-Trace-ID / traceparent / b3 / X-B3-* with a fresh 16-hex span.
func setChatIdentityHeaders(req *http.Request) {
	convReqID := newMessageID()
	msgID := newMessageID()
	span := make([]byte, 8)
	_, _ = rand.Read(span)
	spanHex := hex.EncodeToString(span)
	trace := newMessageID()

	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", wireClientVersion)
	req.Header.Set("X-Private-Data", "true")
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Conversation-ID", uuid7())
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Root-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", msgID)
	req.Header.Set("X-Request-ID", msgID)
	req.Header.Set("X-Trace-ID", trace)
	req.Header.Set("traceparent", "00-"+trace+"-"+spanHex+"-01")
	req.Header.Set("b3", trace+"-"+spanHex+"-1")
	req.Header.Set("X-B3-TraceId", trace)
	req.Header.Set("X-B3-SpanId", spanHex)
	req.Header.Set("X-B3-Sampled", "1")
}
