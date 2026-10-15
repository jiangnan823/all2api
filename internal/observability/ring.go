package observability

// ring.go implements the M5a panel-log sink: a fixed-capacity in-process ring
// of recent log entries served by GET /api/logs. The capacity is a named
// constant (DefaultRingCapacity), not a config key: the buffer is diagnostic
// memory, and growing it is a D2 decision, not a silent default.
//
// The ring is an optional sink: logWith appends only when a ring is attached
// (AttachRing, done once by cmd/gateway/main.go), so bare Loggers — tests,
// tooling — pay nothing. Entries are value copies and Snapshot returns a
// fresh slice, so the HTTP handler can marshal without holding any lock.

import (
	"sync"
	"time"
)

// DefaultRingCapacity is the ring size main attaches for GET /api/logs: the
// latest 500 operational lines (credit seeding, schedule RunStats, recover
// outcomes, startup banner). Fixed by contract — see the ring.go header.
const DefaultRingCapacity = 500

// Entry is one log line as stored in the ring and served by GET /api/logs.
// Msg is the message body (msg plus key=value pairs) without the level and
// prefix decoration the stdout line carries — the entry's Level is its own
// wire field.
type Entry struct {
	TS    time.Time
	Level Level
	Msg   string
}

// Ring is a fixed-capacity FIFO of Entries; the oldest entry is evicted once
// the capacity is reached. All methods are safe for concurrent use.
type Ring struct {
	mu       sync.Mutex
	capacity int
	buf      []Entry
	head     int // index of the oldest entry once buf is full
}

// NewRing builds a ring holding at most capacity entries. A non-positive
// capacity degenerates to a drop-all sink (Append is a no-op) — an honest
// zero instead of an unbounded buffer.
func NewRing(capacity int) *Ring {
	return &Ring{capacity: capacity}
}

// Append stores one entry, evicting the oldest when the ring is full.
func (r *Ring) Append(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.capacity <= 0 {
		return
	}
	if len(r.buf) < r.capacity {
		r.buf = append(r.buf, e)
		return
	}
	// Full: overwrite the oldest slot (head) and advance. buf[head] is always
	// the oldest entry while full, so the ordered invariant holds unchanged.
	r.buf[r.head] = e
	r.head = (r.head + 1) % r.capacity
}

// Snapshot returns a copy of the buffered entries, oldest first. Mutating the
// returned slice never affects the ring.
func (r *Ring) Snapshot() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, len(r.buf))
	for i := range r.buf {
		out[i] = r.buf[(r.head+i)%r.capacity]
	}
	return out
}

// Capacity returns the configured maximum number of entries.
func (r *Ring) Capacity() int {
	return r.capacity
}

// AttachRing attaches r as the Default logger's ring sink (nil detaches).
// Intended to be called once at startup, before the lines that should land
// in GET /api/logs are emitted.
func AttachRing(r *Ring) {
	Default.mu.Lock()
	Default.ring = r
	Default.mu.Unlock()
}

// WireLevel maps a Level onto the two-value panel wire vocabulary of
// GET /api/logs ("level"): everything below Error is "info", Error and Fatal
// are "error".
func WireLevel(l Level) string {
	if l >= LevelError {
		return "error"
	}
	return "info"
}
