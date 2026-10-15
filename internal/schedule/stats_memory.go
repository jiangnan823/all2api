package schedule

// stats_memory.go is the M5a RunStats recall: ReportStats' only consumer used
// to be a stdout Printf, so GET /api/tasks had no last-run data to serve. The
// integrator (cmd/gateway/main.go) stores the function returned here into
// Deps.ReportStats — every finished batch (success, per-account-mixed, or
// error-aborted) lands in the map keyed by task name, keeping only the most
// recent run per task.
//
// Concurrency: ReportStats is called inline from handler goroutines while the
// HTTP handler reads concurrently — one RWMutex, no lock escalation, callers
// never blocked on each other (handlers are already serialized per task by
// the scheduler, but the four tasks run concurrently with each other).

import (
	"sync"
	"time"
)

// StatsMemory keeps the most recent RunStats per task name. Zero value is
// ready to use; all methods are safe for concurrent use.
type StatsMemory struct {
	mu     sync.RWMutex
	latest map[string]RunStats
	at     map[string]time.Time
}

// NewStatsMemory builds an empty recall store.
func NewStatsMemory() *StatsMemory {
	return &StatsMemory{
		latest: make(map[string]RunStats),
		at:     make(map[string]time.Time),
	}
}

// Recorder returns the Deps.ReportStats-compatible sink: it remembers s and
// the wall-clock time of the recording, then delegates to next when non-nil
// (main keeps its stdout banner by passing the old Printf through).
func (m *StatsMemory) Recorder(next func(RunStats)) func(RunStats) {
	return func(s RunStats) {
		now := time.Now()
		m.mu.Lock()
		m.latest[s.Task] = s
		m.at[s.Task] = now
		m.mu.Unlock()
		if next != nil {
			next(s)
		}
	}
}

// LastRun returns the most recent RunStats recorded for task and when it was
// recorded. ok=false means the task has never finished a batch — the panel
// renders last_run: null.
func (m *StatsMemory) LastRun(task string) (s RunStats, at time.Time, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok = m.latest[task]
	if !ok {
		return RunStats{}, time.Time{}, false
	}
	return s, m.at[task], true
}
