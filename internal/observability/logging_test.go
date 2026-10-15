package observability

// M5a tests for the logWith repair (the writer is now really written, the
// §9.21-b black hole) and the ring sink (capacity discipline, order, snapshot
// isolation, wire-level mapping).

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestLogger(w *bytes.Buffer, ring *Ring) *Logger {
	l := &Logger{level: LevelInfo, writer: w, prefix: "test"}
	if ring != nil {
		l.ring = ring
	}
	return l
}

func TestLogWithWritesWriter(t *testing.T) {
	var buf bytes.Buffer
	l := newTestLogger(&buf, nil)

	logWith(l, LevelInfo, "credit seeding: 3 accounts", "seeded", 3, "failed", 0)
	out := buf.String()
	if !strings.Contains(out, "[INFO] test: credit seeding: 3 accounts seeded=3 failed=0") {
		t.Fatalf("stdout line missing or misformatted: %q", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("stdout line must end with newline: %q", out)
	}

	// Odd key/value tail keeps an explicit placeholder instead of panicking.
	buf.Reset()
	logWith(l, LevelError, "boom", "orphan")
	if !strings.Contains(buf.String(), "boom orphan=EV") {
		t.Fatalf("odd kv tail must render an explicit placeholder: %q", buf.String())
	}
}

func TestLogWithRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	l := newTestLogger(&buf, nil)

	logWith(l, LevelDebug, "quiet", "k", "v")
	if buf.Len() != 0 {
		t.Fatalf("debug below the info floor must not reach the writer: %q", buf.String())
	}
}

func TestLogWithRingSink(t *testing.T) {
	var buf bytes.Buffer
	ring := NewRing(4)
	l := newTestLogger(&buf, ring)

	logWith(l, LevelInfo, "first", "k", 1)
	logWith(l, LevelError, "second")

	entries := ring.Snapshot()
	if len(entries) != 2 {
		t.Fatalf("ring holds %d entries, want 2", len(entries))
	}
	if entries[0].Msg != "first k=1" || entries[0].Level != LevelInfo {
		t.Fatalf("entry 0 wrong: %+v", entries[0])
	}
	if entries[1].Msg != "second" || entries[1].Level != LevelError {
		t.Fatalf("entry 1 wrong: %+v", entries[1])
	}
	if entries[0].TS.After(time.Now()) || entries[0].TS.IsZero() {
		t.Fatalf("entry timestamp must be a real wall-clock now: %+v", entries[0].TS)
	}
	// The stdout line still carries the level decoration the ring drops.
	if !strings.Contains(buf.String(), "[ERROR] test: second") {
		t.Fatalf("stdout decoration lost: %q", buf.String())
	}
}

func TestRingCapacityAndOrder(t *testing.T) {
	ring := NewRing(3)
	for i := 0; i < 5; i++ {
		ring.Append(Entry{TS: time.Now(), Level: LevelInfo, Msg: string(rune('a' + i))})
	}
	got := ring.Snapshot()
	if len(got) != 3 {
		t.Fatalf("ring holds %d, want 3", len(got))
	}
	// Oldest three evicted: c, d, e in order.
	want := []string{"c", "d", "e"}
	for i, w := range want {
		if got[i].Msg != w {
			t.Fatalf("entry %d = %q, want %q", i, got[i].Msg, w)
		}
	}
}

func TestRingSnapshotIsCopy(t *testing.T) {
	ring := NewRing(2)
	ring.Append(Entry{TS: time.Now(), Level: LevelInfo, Msg: "one"})
	got := ring.Snapshot()
	got[0].Msg = "mutated"
	if ring.Snapshot()[0].Msg != "one" {
		t.Fatal("snapshot must be a copy — mutating it must not touch the ring")
	}
}

func TestRingZeroCapacityDropsAll(t *testing.T) {
	ring := NewRing(0)
	ring.Append(Entry{TS: time.Now(), Level: LevelInfo, Msg: "dropped"})
	if got := ring.Snapshot(); len(got) != 0 {
		t.Fatalf("zero-capacity ring must drop everything, got %d", len(got))
	}
}

func TestRingConcurrentAppendSnapshot(t *testing.T) {
	ring := NewRing(64)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				ring.Append(Entry{TS: time.Now(), Level: LevelInfo, Msg: "w"})
				_ = ring.Snapshot()
			}
		}(w)
	}
	wg.Wait()
	if got := ring.Snapshot(); len(got) != 64 {
		t.Fatalf("after 400 appends ring must hold exactly the capacity, got %d", len(got))
	}
}

func TestAttachRingAndWireLevel(t *testing.T) {
	// AttachRing on Default (then detach): the package-level Info/Error path
	// must land in the attached ring too.
	ring := NewRing(8)
	AttachRing(ring)
	Info("attached line", "n", 1)
	Error("attached error")
	AttachRing(nil) // detach before Default is used by any other test

	got := ring.Snapshot()
	if len(got) != 2 {
		t.Fatalf("attached ring holds %d, want 2", len(got))
	}
	if got[0].Msg != "attached line n=1" || got[1].Msg != "attached error" {
		t.Fatalf("entries wrong: %+v", got)
	}

	// Wire mapping: only the two-value panel vocabulary.
	if WireLevel(LevelInfo) != "info" || WireLevel(LevelWarn) != "info" || WireLevel(LevelDebug) != "info" {
		t.Fatal("levels below error must map to info")
	}
	if WireLevel(LevelError) != "error" || WireLevel(LevelFatal) != "error" {
		t.Fatal("error and fatal must map to error")
	}
}
