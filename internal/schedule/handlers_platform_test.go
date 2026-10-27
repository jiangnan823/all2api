package schedule

// M10-B2 per-platform task assembly tests: BuildPlatformTasks naming/source/
// cron resolution, the workbuddy-only cat_travel capability, and the
// per-platform handler binding.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"all2api/internal/adapter"
)

func TestBuildPlatformTasksNamingSourceAndCrons(t *testing.T) {
	tasks := BuildPlatformTasks([]PlatformCrons{
		{Platform: "workbuddy", Signin: "30 8 * * *", ActiveCheck: "", TokenKeepalive: "15 23 * * *", CatTravel: "45 20 * * *"},
		{Platform: "qoder"},
		{Platform: "trae", Signin: "0 12 * * *"},
		{Platform: ""}, // unnamed stanza: silently skipped, never a nameless task
	})

	want := map[string]struct {
		source string
		cron   string
	}{
		"signin@workbuddy":          {"workbuddy", "30 8 * * *"},           // platform key wins
		"active_check@workbuddy":    {"workbuddy", DefaultActiveCheckCron}, // empty → code-truth default
		"token_keepalive@workbuddy": {"workbuddy", "15 23 * * *"},
		"cat_travel@workbuddy":      {"workbuddy", "45 20 * * *"}, // first-ever cat_travel config key
		"signin@qoder":              {"qoder", DefaultSigninCron},
		"active_check@qoder":        {"qoder", DefaultActiveCheckCron},
		"token_keepalive@qoder":     {"qoder", DefaultTokenKeepaliveCron},
		"signin@trae":               {"trae", "0 12 * * *"},
		"active_check@trae":         {"trae", DefaultActiveCheckCron},
		"token_keepalive@trae":      {"trae", DefaultTokenKeepaliveCron},
	}
	if len(tasks) != len(want) {
		t.Fatalf("built %d tasks, want %d: %+v", len(tasks), len(want), tasks)
	}
	// cat_travel exists ONLY for workbuddy (capability truth, not a per-
	// platform template).
	for _, task := range tasks {
		w, ok := want[task.Name]
		if !ok {
			t.Fatalf("unexpected task %q", task.Name)
		}
		if task.Source != w.source {
			t.Fatalf("%s: source = %q, want %q", task.Name, task.Source, w.source)
		}
		if task.CronSpec != w.cron {
			t.Fatalf("%s: cron = %q, want %q", task.Name, task.CronSpec, w.cron)
		}
		if !task.Enabled {
			t.Fatalf("%s: per-platform tasks default enabled", task.Name)
		}
	}
	if _, ok := want["cat_travel@qoder"]; ok {
		t.Fatal("cat_travel must never register for qoder")
	}
}

func TestPlatformTaskNameRoundTrip(t *testing.T) {
	task, platform, ok := platformOfTaskName(PlatformTaskName(TaskSignin, "workbuddy"))
	if !ok || task != TaskSignin || platform != "workbuddy" {
		t.Fatalf("round trip = %s/%s/%v", task, platform, ok)
	}
	// Bare legacy names report ok=false — RegisterDefaultHandlers territory.
	if _, _, ok := platformOfTaskName(TaskSignin); ok {
		t.Fatal("bare task name must not parse as platform-scoped")
	}
	if _, _, ok := platformOfTaskName("@workbuddy"); ok {
		t.Fatal("empty base task must not parse")
	}
}

func TestRegisterPlatformHandlersBindsBuiltTasks(t *testing.T) {
	tasks := BuildPlatformTasks([]PlatformCrons{
		{Platform: "workbuddy", CatTravel: ""},
		{Platform: "qoder"},
	})
	s := NewScheduler()
	RegisterPlatformHandlers(context.Background(), s, &Deps{}, tasks)

	// Every built task got exactly its handler bound.
	for _, task := range tasks {
		s.mu.RLock()
		_, bound := s.handlers[task.Name]
		s.mu.RUnlock()
		if !bound {
			t.Fatalf("task %q has no handler bound", task.Name)
		}
	}
	// Legacy bare names are out of scope for the platform binder...
	s.AddTask(&Task{Name: TaskSignin, CronSpec: DefaultSigninCron, Enabled: true})
	RegisterPlatformHandlers(context.Background(), s, &Deps{}, []*Task{{Name: TaskSignin}})
	s.mu.RLock()
	_, signinBound := s.handlers[TaskSignin]
	s.mu.RUnlock()
	if signinBound {
		t.Fatal("platform binder must not touch bare legacy names")
	}
	// ...and an unknown base task never binds a guess.
	RegisterPlatformHandlers(context.Background(), s, &Deps{}, []*Task{{Name: "mystery@workbuddy"}})
	s.mu.RLock()
	_, mysteryBound := s.handlers["mystery@workbuddy"]
	s.mu.RUnlock()
	if mysteryBound {
		t.Fatal("unknown base task must not bind")
	}
}

// TestTokenKeepaliveSkipsRestrictedAccount pins the 11140 red line on the
// task side: an account cooling with the risk-control reason must be skipped
// by token_keepalive (the looser predicate) — touching a restricted account
// reads as more abnormal behavior — while a merely rate-limited one still
// gets its refresh.
func TestTokenKeepaliveSkipsRestrictedAccount(t *testing.T) {
	restricted := &Account{ID: "r1", Platform: "workbuddy", Status: AccountCooling,
		CooldownEnd:    time.Now().Add(time.Hour),
		CooldownReason: CooldownReasonRestricted}
	ratelimited := &Account{ID: "r2", Platform: "workbuddy", Status: AccountCooling,
		CooldownEnd: time.Now().Add(time.Minute)}

	if !skipDisabled(restricted) {
		t.Fatal("restricted account must be skipped by token_keepalive")
	}
	if skipDisabled(ratelimited) {
		t.Fatal("rate-limited account must still be refreshed by token_keepalive")
	}
}

// TestWriteFailureParksRestrictedInsteadOfDisable: a credential-expired
// failure whose underlying classification is account_restricted must
// long-park (cooldown), never disable — disable is for genuinely dead
// credentials and would brick the account until a human noticed.
func TestWriteFailureParksRestrictedInsteadOfDisable(t *testing.T) {
	store := &fakeStore{}
	deps := &Deps{Store: store}
	var ue *adapter.UpstreamError
	// The task chain: the classified restriction wrapped by the schedule
	// sentinel, exactly as translateTaskError + %w would produce it.
	ue = &adapter.UpstreamError{Status: 403, Reason: adapter.ReasonAccountRestricted, Message: "workbuddy: request illegal"}
	cause := fmt.Errorf("schedule upstream: token_keepalive: %s: %w", ue.Message, fmt.Errorf("%w: %w", ErrCredentialExpired, ue))

	err := deps.writeFailure(context.Background(), &Account{ID: "a", Platform: "workbuddy"}, cause)
	if err == nil {
		t.Fatal("writeFailure must return non-nil for the restricted cause")
	}
	for _, e := range store.events {
		if len(e) >= 7 && e[:7] == "status:" {
			t.Fatalf("restricted account must not be disabled, got store event %q", e)
		}
	}
	if len(store.events) != 1 || store.events[0][:9] != "cooldown:" {
		t.Fatalf("store events = %v, want exactly one cooldown (the long park)", store.events)
	}
}
