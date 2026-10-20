package circuit

import (
	"testing"
	"time"
)

// forceExpiry moves an open breaker's nextReset into the past so Allow flips
// to half-open without waiting out the window in wall-clock time.
func forceExpiry(b *Breaker) {
	b.mu.Lock()
	b.nextReset = time.Now().Add(-time.Second)
	b.mu.Unlock()
}

// TestConsecutiveTripRecoversAfterBoundedWindow pins the 2026-10-21 incident
// fix: five consecutive 5xx failures used to latch the circuit open forever
// (noAutoRecover) — one ~30-second upstream blip parked workbuddy behind a
// permanently open circuit for six hours. The trip window must be bounded:
// after it closes, a probe flows, a success closes the circuit, and five
// fresh failures re-trip it.
func TestConsecutiveTripRecoversAfterBoundedWindow(t *testing.T) {
	b := NewBreaker("p", Config{ConsecutiveFailures: 5, TripAutoRecoverSec: 1800})

	for i := 0; i < 4; i++ {
		b.Record(500, "a")
	}
	if b.IsTripped() {
		t.Fatal("four failures must not trip a five-failure breaker")
	}
	b.Record(502, "a")
	if !b.IsTripped() || b.Reason() != ReasonConsecutive {
		t.Fatalf("fifth failure must trip: state=%v reason=%q", b.State(), b.Reason())
	}
	if b.Allow() {
		t.Fatal("tripped circuit must block while the window is open")
	}

	forceExpiry(b)
	if !b.Allow() {
		t.Fatal("window closed: a probe must flow (half-open)")
	}
	if b.State() != StateHalfOpen {
		t.Fatalf("state = %v, want half-open after window expiry", b.State())
	}

	// A successful probe closes the circuit and clears the counters.
	b.Record(200, "a")
	if b.State() != StateClosed {
		t.Fatalf("successful probe must close: state=%v", b.State())
	}
	b.Record(500, "a")
	if b.IsTripped() {
		t.Fatal("counters must be cleared on close: one failure cannot re-trip")
	}

	// Five fresh consecutive failures re-trip with the same bounded window.
	for i := 0; i < 5; i++ {
		b.Record(503, "a")
	}
	if !b.IsTripped() {
		t.Fatal("five fresh failures must re-trip")
	}
	b.mu.RLock()
	resetIn := time.Until(b.nextReset)
	b.mu.RUnlock()
	if resetIn <= 0 || resetIn > 31*time.Minute {
		t.Fatalf("re-trip window = %v, want the bounded TripAutoRecoverSec", resetIn)
	}
}

// TestTransportFailuresTripToo: statusCode 0 (no upstream exchange, transport
// error) shares the consecutive-failure bucket with 5xx.
func TestTransportFailuresTripToo(t *testing.T) {
	b := NewBreaker("p", Config{ConsecutiveFailures: 3, TripAutoRecoverSec: 60})
	for i := 0; i < 3; i++ {
		b.Record(0, "a")
	}
	if !b.IsTripped() {
		t.Fatal("transport failures must count toward the trip")
	}
}

// TestNewBreakerNormalizesTripWindow: a hand-built Config without the trip
// window (zero value) must not degenerate into an instant/zero window.
func TestNewBreakerNormalizesTripWindow(t *testing.T) {
	b := NewBreaker("p", Config{ConsecutiveFailures: 2})
	for i := 0; i < 2; i++ {
		b.Record(500, "a")
	}
	if !b.IsTripped() {
		t.Fatal("expected trip")
	}
	b.mu.RLock()
	resetIn := time.Until(b.nextReset)
	b.mu.RUnlock()
	if resetIn <= time.Minute {
		t.Fatalf("normalized window = %v, want the 30min default", resetIn)
	}
}

// TestSuccessClearsConsecutiveCounters: interleaved successes keep a flapping
// upstream from ever accumulating five consecutive failures.
func TestSuccessClearsConsecutiveCounters(t *testing.T) {
	b := NewBreaker("p", Config{ConsecutiveFailures: 5, TripAutoRecoverSec: 60})
	for i := 0; i < 10; i++ {
		b.Record(500, "a")
		b.Record(200, "a")
	}
	if b.IsTripped() {
		t.Fatal("interleaved successes must never trip the breaker")
	}
}
