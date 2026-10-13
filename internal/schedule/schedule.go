// Package schedule implements the cron-based task scheduler adapted from Panel design.
// Four independent scheduled tasks: signin(09/21), active_check(10), cat_travel(09/21),
// token_keepalive(22). Each has its own cron spec and on/off switch.
package schedule

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Status represents a task execution status.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSuccess   Status = "success"
	StatusFailed    Status = "failed"
	StatusSkipped   Status = "skipped"
)

// Task represents a single scheduled job entry.
type Task struct {
	Name      string    `json:"name"`
	Source    string    `json:"source"`   // platform source: workbuddy, qoder, trae
	CronSpec  string    `json:"cron_spec"`
	Enabled   bool      `json:"enabled"`
	Status    Status    `json:"status"`     // last run result
	LastRunAt time.Time `json:"last_run_at"`
	NextRunAt time.Time `json:"next_run_at"`
	Duration  time.Duration `json:"duration,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// Scheduler manages all platform-specific schedules.
type Scheduler struct {
	mu       sync.RWMutex
	tasks    map[string]*Task // name → Task
	stopCh   chan struct{}
	stopped  chan struct{}
	handlers map[string]TaskHandler
}

// TaskHandler is called when a scheduled task fires.
type TaskHandler func(ctx interface{}) error

// NewScheduler creates an empty task manager.
func NewScheduler() *Scheduler {
	return &Scheduler{
		tasks:    make(map[string]*Task),
		handlers: make(map[string]TaskHandler),
		stopCh:   make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

// Start begins periodic execution of all enabled tasks.
func (s *Scheduler) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.tasks) == 0 {
		close(s.stopped)
		return nil
	}

	s.initNextRuns()

	go s.tickLoop()
	return nil
}

// Stop gracefully stops all running tasks.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.stopCh:
		return
	default:
	}
	close(s.stopCh)
	<-s.stopped
}

// AddTask registers a new scheduled task with an optional handler.
func (s *Scheduler) AddTask(task *Task) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.tasks[task.Name] = task
	if task.CronSpec != "" && task.NextRunAt.IsZero() {
		task.NextRunAt = s.nextOccurrence(time.Now(), task.CronSpec)
	}
}

// RegisterHandler binds a handler function to a task name.
// Called by platform adapters during init().
func (s *Scheduler) RegisterHandler(name string, handler TaskHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[name] = handler
}

// RemoveTask removes a scheduled task by name.
func (s *Scheduler) RemoveTask(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.tasks, name)
	delete(s.handlers, name)
}

// GetTasks returns a snapshot of all tasks.
func (s *Scheduler) GetTasks() []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		cp := *t // copy
		result = append(result, &cp)
	}
	return result
}

// IsEnabled returns whether the named task is enabled.
func (s *Scheduler) IsEnabled(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if t, ok := s.tasks[name]; ok {
		return t.Enabled
	}
	return false
}

// SetEnabled changes the enabled state of a task.
func (s *Scheduler) SetEnabled(name string, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if t, ok := s.tasks[name]; ok {
		t.Enabled = enabled
	}
}

// EnableAll enables every registered task.
func (s *Scheduler) EnableAll() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, t := range s.tasks {
		t.Enabled = true
	}
}

// DisableAll disables every registered task.
func (s *Scheduler) DisableAll() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, t := range s.tasks {
		t.Enabled = false
	}
}

// tickLoop is the main scheduling loop — checks every second if any task should fire.
func (s *Scheduler) tickLoop() {
	defer close(s.stopped)

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			now := time.Now()
			s.mu.RLock()
			var needsFiring []*Task
			for _, task := range s.tasks {
				if task.Enabled && !now.Before(task.NextRunAt) {
					needsFiring = append(needsFiring, task)
				}
			}
			s.mu.RUnlock()

			for _, task := range needsFiring {
				s.execute(task)
			}
		}
	}
}

// execute runs the actual task logic in a goroutine to avoid blocking the
// scheduler. A3 (adversarial review): every shared-field access happens under
// the write lock and the function never returns holding the lock — the old
// form unlocked a mutex it did not hold (runtime fatal), wrote task.Status
// without any lock, and returned still locked (deadlock on the next tick).
func (s *Scheduler) execute(task *Task) {
	start := time.Now()

	s.mu.Lock()
	task.Status = StatusRunning
	handler, ok := s.handlers[task.Name]
	if !ok {
		// No handler registered yet (stub)
		task.Status = StatusSuccess
		task.LastRunAt = start
		task.Duration = time.Since(start).Round(time.Millisecond)
		task.NextRunAt = s.nextOccurrence(time.Now(), task.CronSpec)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	go func() {
		err := handler(nil)
		elapsed := time.Since(start)

		s.mu.Lock()
		defer s.mu.Unlock()

		task.LastRunAt = start
		task.Duration = elapsed.Round(time.Millisecond)
		if err != nil {
			task.Error = err.Error()
			task.Status = StatusFailed
		} else {
			task.Status = StatusSuccess
		}
		task.NextRunAt = s.nextOccurrence(time.Now(), task.CronSpec)
	}()
}

// nextOccurrence computes the next fire time after now based on a cron expression.
// Simplified parser supporting common patterns:
// "0 9,21 * * *" → 09:00 and 21:00 daily
// "0 10 * * *" → 10:00 daily
// "0 22 * * *" → 22:00 daily
func (s *Scheduler) nextOccurrence(after time.Time, cronSpec string) time.Time {
	parts := strings.Fields(cronSpec)
	if len(parts) < 5 {
		return after.Add(24 * time.Hour)
	}

	minutes, _ := parseCronField(parts[0], 0, 59)
	hours, _ := parseCronField(parts[1], 0, 23)

	nextTime := after
	// Skip to tomorrow if we've passed today's last occurrence
	lastHour := hours[len(hours)-1]
	lastMin := minutes[len(minutes)-1]
	if after.Hour() > lastHour ||
		(after.Hour() == lastHour && after.Minute() >= lastMin) {
		nextTime = nextTime.AddDate(0, 0, 1)
	}
	// Reset to midnight
	nextTime = time.Date(nextTime.Year(), nextTime.Month(), nextTime.Day(), 0, 0, 0, 0, after.Location())

	for _, h := range hours {
		for _, m := range minutes {
			candidate := nextTime.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
			if !candidate.Before(after) {
				return candidate
			}
		}
	}

	// Fallback: tomorrow first occurrence
	firstH := hours[0]
	firstM := minutes[0]
	return nextTime.Add((24 + firstH) * time.Hour + time.Duration(firstM) * time.Minute)
}

// parseCronField parses a cron field (supports *, number, comma-separated).
func parseCronField(field string, min, max int) ([]int, error) {
	var result []int

	for _, part := range splitByComma(field) {
		part = strings.TrimSpace(part)
		if part == "*" || part == "" {
			continue
		}
		if n, err := strconv.Atoi(part); err == nil {
			if n >= min && n <= max {
				result = append(result, n)
			}
		}
	}

	if len(result) == 0 {
		return []int{min}, nil
	}
	sort.Ints(result)
	return result, nil
}
