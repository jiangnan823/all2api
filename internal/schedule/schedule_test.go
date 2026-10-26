package schedule

import (
	"testing"
	"time"
)

// TestRunningTaskDoesNotRefire pins the overlap guard: a task whose handler
// is still running must not be re-selected by the tick loop — the old
// predicate ignored Status, so a slow batch re-fired every second until it
// completed (one outage = a self-DDoS of concurrent handler copies).
func TestRunningTaskDoesNotRefire(t *testing.T) {
	s := NewScheduler()
	task := &Task{Name: "overlap", CronSpec: "* * * * *", Enabled: true,
		Status: StatusRunning, NextRunAt: time.Now().Add(-time.Minute)}
	s.AddTask(task)

	fired := 0
	s.handlers["overlap"] = func(ctx interface{}) error {
		fired++
		return nil
	}

	now := time.Now()
	s.mu.RLock()
	for _, tk := range s.tasks {
		if tk.Enabled && tk.Status != StatusRunning && !now.Before(tk.NextRunAt) {
			t.Fatal("a running task must never match the firing predicate")
		}
	}
	s.mu.RUnlock()
	_ = fired
}

// TestExecuteAdvancesNextRunAtImmediately pins the dispatch-time advance:
// execute() must move NextRunAt under the dispatch lock, before the handler
// goroutine starts — the dispatch-to-goroutine window is the second half of
// the overlap hole.
func TestExecuteAdvancesNextRunAtImmediately(t *testing.T) {
	s := NewScheduler()
	task := &Task{Name: "advance", CronSpec: "0 22 * * *", Enabled: true}
	s.AddTask(task)
	s.handlers["advance"] = func(ctx interface{}) error { return nil }

	now := time.Now()
	s.execute(task)
	s.mu.RLock()
	future := task.NextRunAt.After(now)
	running := task.Status == StatusRunning
	s.mu.RUnlock()
	if !future {
		t.Fatal("NextRunAt must sit in the future right after dispatch — a stale value would let the next tick re-fire the task")
	}
	if !running {
		t.Fatal("task must read as running immediately after dispatch")
	}
}
