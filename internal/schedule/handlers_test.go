package schedule

// Deterministic handler tests over fake upstream / account source / store:
// normal batch write-back, per-account failure isolation, skip semantics
// (disabled / cooling / unsupported platform), ctx cancellation mid-batch,
// empty source, batch-level failure paths, and BuildDefaultTasks cron
// resolution. No sleeps, no real time dependencies beyond explicit timestamps.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeSource is a canned AccountSource that records which task asked.
type fakeSource struct {
	accounts []*Account
	listErr  error
	lastTask string
}

func (f *fakeSource) Accounts(_ context.Context, taskType string) ([]*Account, error) {
	f.lastTask = taskType
	return f.accounts, f.listErr
}

// fakeUpstream records every op call and delegates behavior to per-op plugs.
type fakeUpstream struct {
	calls       []string // "signin:workbuddy:a1" style op log
	signinFn    func(acc *Account) (SigninResult, error)
	activeFn    func(acc *Account) error
	catFn       func(acc *Account) (CatTravelResult, error)
	keepaliveFn func(acc *Account) (KeepaliveResult, error)
}

// newFakeUpstream returns a fake whose every op succeeds with zero results.
func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{
		signinFn:    func(*Account) (SigninResult, error) { return SigninResult{}, nil },
		activeFn:    func(*Account) error { return nil },
		catFn:       func(*Account) (CatTravelResult, error) { return CatTravelResult{}, nil },
		keepaliveFn: func(*Account) (KeepaliveResult, error) { return KeepaliveResult{}, nil },
	}
}

func (f *fakeUpstream) Signin(_ context.Context, acc *Account) (SigninResult, error) {
	f.calls = append(f.calls, "signin:"+acc.Platform+":"+acc.ID)
	return f.signinFn(acc)
}

func (f *fakeUpstream) ActiveCheck(_ context.Context, acc *Account) error {
	f.calls = append(f.calls, "active_check:"+acc.Platform+":"+acc.ID)
	return f.activeFn(acc)
}

func (f *fakeUpstream) CatTravel(_ context.Context, acc *Account) (CatTravelResult, error) {
	f.calls = append(f.calls, "cat_travel:"+acc.Platform+":"+acc.ID)
	return f.catFn(acc)
}

func (f *fakeUpstream) TokenKeepalive(_ context.Context, acc *Account) (KeepaliveResult, error) {
	f.calls = append(f.calls, "token_keepalive:"+acc.Platform+":"+acc.ID)
	return f.keepaliveFn(acc)
}

// fakeStore logs every write-back as a comparable event string.
type fakeStore struct {
	events []string
}

func (s *fakeStore) AddCredit(_ context.Context, platform, id string, delta int64) error {
	s.events = append(s.events, fmt.Sprintf("credit:%s:%s:%+d", platform, id, delta))
	return nil
}

func (s *fakeStore) SetStatus(_ context.Context, platform, id, status string) error {
	s.events = append(s.events, fmt.Sprintf("status:%s:%s:%s", platform, id, status))
	return nil
}

func (s *fakeStore) SetCooldown(_ context.Context, platform, id string, until time.Time) error {
	s.events = append(s.events, fmt.Sprintf("cooldown:%s:%s:%s", platform, id, until.Format(time.RFC3339)))
	return nil
}

func (s *fakeStore) SetExpiry(_ context.Context, platform, id string, at time.Time) error {
	s.events = append(s.events, fmt.Sprintf("expiry:%s:%s:%s", platform, id, at.Format(time.RFC3339)))
	return nil
}

// newTestDeps wires the fakes into Deps and captures ReportStats runs.
func newTestDeps(src AccountSource, up *fakeUpstream, st *fakeStore) (*Deps, *[]RunStats) {
	runs := &[]RunStats{}
	return &Deps{
		Source:      src,
		Upstream:    up,
		Store:       st,
		ReportStats: func(s RunStats) { *runs = append(*runs, s) },
	}, runs
}

// assertEvents compares the ordered write-back log.
func assertEvents(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("store events mismatch:\n got  %v\n want %v", got, want)
	}
}

func hasPrefixEvent(events []string, prefix string) bool {
	for _, e := range events {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func activeAccounts() []*Account {
	return []*Account{
		{ID: "a1", Platform: "workbuddy", Status: AccountActive},
		{ID: "a2", Platform: "workbuddy", Status: AccountActive},
		{ID: "a3", Platform: "qoder", Status: AccountActive},
	}
}

func TestSigninNormalBatchWritesBack(t *testing.T) {
	src := &fakeSource{accounts: activeAccounts()}
	up := newFakeUpstream()
	up.signinFn = func(*Account) (SigninResult, error) {
		return SigninResult{CreditDelta: 10}, nil
	}
	store := &fakeStore{}
	deps, runs := newTestDeps(src, up, store)

	handler := NewSigninHandler(deps)
	// handler(nil) mirrors how the scheduler invokes handlers today (execute()).
	if err := handler(nil); err != nil {
		t.Fatalf("signin batch: %v", err)
	}

	if src.lastTask != TaskSignin {
		t.Fatalf("source asked for task %q, want %q", src.lastTask, TaskSignin)
	}
	assertEvents(t, store.events, []string{
		"credit:workbuddy:a1:+10", "status:workbuddy:a1:active",
		"credit:workbuddy:a2:+10", "status:workbuddy:a2:active",
		"credit:qoder:a3:+10", "status:qoder:a3:active",
	})
	if len(*runs) != 1 {
		t.Fatalf("ReportStats called %d times, want 1", len(*runs))
	}
	want := RunStats{Task: TaskSignin, Total: 3, Success: 3}
	if (*runs)[0] != want {
		t.Fatalf("stats = %+v, want %+v", (*runs)[0], want)
	}
}

func TestSingleAccountFailureDoesNotAbortBatch(t *testing.T) {
	src := &fakeSource{accounts: activeAccounts()}
	up := newFakeUpstream()
	up.signinFn = func(acc *Account) (SigninResult, error) {
		if acc.ID == "a2" {
			return SigninResult{}, errors.New("upstream boom")
		}
		return SigninResult{CreditDelta: 5}, nil
	}
	store := &fakeStore{}
	deps, runs := newTestDeps(src, up, store)
	deps.FailureCooldown = 5 * time.Minute

	// handler(ctx) proves a real context flows through the untyped argument.
	if err := handlerWithCtx(t, context.Background(), NewSigninHandler(deps)); err != nil {
		t.Fatalf("batch with one failed account must still succeed: %v", err)
	}

	// a2 is cooled down, a1 and a3 are untouched by its failure. The exact
	// cooldown end time is clock-derived, so it is matched by prefix.
	wantPrefix := []string{
		"credit:workbuddy:a1:+5", "status:workbuddy:a1:active",
		"cooldown:workbuddy:a2:",
		"credit:qoder:a3:+5", "status:qoder:a3:active",
	}
	if len(store.events) != len(wantPrefix) {
		t.Fatalf("store events %v, want %d entries", store.events, len(wantPrefix))
	}
	for i, prefix := range wantPrefix {
		if !strings.HasPrefix(store.events[i], prefix) {
			t.Fatalf("store event[%d] = %q, want prefix %q (all: %v)", i, store.events[i], prefix, store.events)
		}
	}
	want := RunStats{Task: TaskSignin, Total: 3, Success: 2, Failed: 1}
	if (*runs)[0] != want {
		t.Fatalf("stats = %+v, want %+v", (*runs)[0], want)
	}
}

func TestCredentialExpiredDisablesInsteadOfCooling(t *testing.T) {
	src := &fakeSource{accounts: activeAccounts()}
	up := newFakeUpstream()
	up.signinFn = func(acc *Account) (SigninResult, error) {
		if acc.ID == "a1" {
			return SigninResult{}, fmt.Errorf("signin: %w", ErrCredentialExpired)
		}
		return SigninResult{CreditDelta: 1}, nil
	}
	store := &fakeStore{}
	deps, runs := newTestDeps(src, up, store)

	if err := NewSigninHandler(deps)(nil); err != nil {
		t.Fatalf("credential expiry on one account must not fail the batch: %v", err)
	}

	if !hasPrefixEvent(store.events, "status:workbuddy:a1:disabled") {
		t.Fatalf("expired credential must disable a1, got events %v", store.events)
	}
	if hasPrefixEvent(store.events, "cooldown:workbuddy:a1:") {
		t.Fatalf("expired credential must NOT cool a1 (disable wins), got events %v", store.events)
	}
	want := RunStats{Task: TaskSignin, Total: 3, Success: 2, Failed: 1}
	if (*runs)[0] != want {
		t.Fatalf("stats = %+v, want %+v", (*runs)[0], want)
	}
}

func TestSigninSkipsDisabledCoolingAndUnsupported(t *testing.T) {
	src := &fakeSource{accounts: []*Account{
		{ID: "d1", Platform: "workbuddy", Status: AccountDisabled},
		{ID: "c1", Platform: "workbuddy", Status: AccountActive, CooldownEnd: time.Now().Add(time.Hour)},
		{ID: "u1", Platform: "qoder", Status: AccountActive},
		{ID: "ok1", Platform: "workbuddy", Status: AccountActive},
	}}
	up := newFakeUpstream()
	up.signinFn = func(acc *Account) (SigninResult, error) {
		if acc.ID == "u1" {
			return SigninResult{}, fmt.Errorf("cat on qoder: %w", ErrUnsupportedTask)
		}
		return SigninResult{CreditDelta: 3}, nil
	}
	store := &fakeStore{}
	deps, runs := newTestDeps(src, up, store)

	if err := NewSigninHandler(deps)(nil); err != nil {
		t.Fatalf("skip-only batch: %v", err)
	}

	// u1 reaches the upstream (unsupported is discovered by the call itself)
	// but is then skipped — nothing is written back for it; ok1 is the only
	// full success.
	assertEvents(t, up.calls, []string{"signin:qoder:u1", "signin:workbuddy:ok1"})
	assertEvents(t, store.events, []string{
		"credit:workbuddy:ok1:+3", "status:workbuddy:ok1:active",
	})
	want := RunStats{Task: TaskSignin, Total: 4, Success: 1, Skipped: 3}
	if (*runs)[0] != want {
		t.Fatalf("stats = %+v, want %+v", (*runs)[0], want)
	}
}

func TestActiveCheckAndCatTravelWriteBack(t *testing.T) {
	acUp := newFakeUpstream()
	acStore := &fakeStore{}
	acDeps, acRuns := newTestDeps(&fakeSource{accounts: activeAccounts()}, acUp, acStore)
	if err := NewActiveCheckHandler(acDeps)(nil); err != nil {
		t.Fatalf("active_check batch: %v", err)
	}
	assertEvents(t, acStore.events, []string{
		"status:workbuddy:a1:active",
		"status:workbuddy:a2:active",
		"status:qoder:a3:active",
	})
	if want := (RunStats{Task: TaskActiveCheck, Total: 3, Success: 3}); (*acRuns)[0] != want {
		t.Fatalf("active_check stats = %+v, want %+v", (*acRuns)[0], want)
	}

	catUp := newFakeUpstream()
	catUp.catFn = func(*Account) (CatTravelResult, error) { return CatTravelResult{CreditDelta: 7}, nil }
	catStore := &fakeStore{}
	catDeps, catRuns := newTestDeps(&fakeSource{accounts: activeAccounts()}, catUp, catStore)
	if err := NewCatTravelHandler(catDeps)(nil); err != nil {
		t.Fatalf("cat_travel batch: %v", err)
	}
	assertEvents(t, catStore.events, []string{
		"credit:workbuddy:a1:+7", "status:workbuddy:a1:active",
		"credit:workbuddy:a2:+7", "status:workbuddy:a2:active",
		"credit:qoder:a3:+7", "status:qoder:a3:active",
	})
	if want := (RunStats{Task: TaskCatTravel, Total: 3, Success: 3}); (*catRuns)[0] != want {
		t.Fatalf("cat_travel stats = %+v, want %+v", (*catRuns)[0], want)
	}
}

func TestTokenKeepaliveRunsWhileCoolingAndWritesExpiry(t *testing.T) {
	fixedExpiry := time.Date(2026, 10, 15, 22, 0, 0, 0, time.UTC)
	src := &fakeSource{accounts: []*Account{
		{ID: "a1", Platform: "workbuddy", Status: AccountActive},
		// Cooling accounts still get keepalive: expiry is orthogonal to cooldown.
		{ID: "a2", Platform: "workbuddy", Status: AccountCooling, CooldownEnd: time.Now().Add(time.Hour)},
		{ID: "a3", Platform: "qoder", Status: AccountDisabled},
	}}
	up := newFakeUpstream()
	up.keepaliveFn = func(acc *Account) (KeepaliveResult, error) {
		if acc.ID == "a1" {
			return KeepaliveResult{ExpiresAt: fixedExpiry}, nil
		}
		return KeepaliveResult{}, nil // a2: platform-managed expiry
	}
	store := &fakeStore{}
	deps, runs := newTestDeps(src, up, store)

	if err := NewTokenKeepaliveHandler(deps)(nil); err != nil {
		t.Fatalf("token_keepalive batch: %v", err)
	}

	assertEvents(t, up.calls, []string{
		"token_keepalive:workbuddy:a1",
		"token_keepalive:workbuddy:a2", // cooling does not skip keepalive
	})
	// Only a1's detected expiry lands; a2's zero expiry writes nothing.
	assertEvents(t, store.events, []string{"expiry:workbuddy:a1:" + fixedExpiry.Format(time.RFC3339)})
	want := RunStats{Task: TaskTokenKeepalive, Total: 3, Success: 2, Skipped: 1}
	if (*runs)[0] != want {
		t.Fatalf("stats = %+v, want %+v", (*runs)[0], want)
	}
}

func TestEmptyAccountSourceIsANoopBatch(t *testing.T) {
	for _, batch := range [][]*Account{{}, nil} {
		src := &fakeSource{accounts: batch}
		up := newFakeUpstream()
		store := &fakeStore{}
		deps, runs := newTestDeps(src, up, store)

		if err := NewSigninHandler(deps)(nil); err != nil {
			t.Fatalf("empty batch (accounts=%v): %v", batch, err)
		}
		assertEvents(t, store.events, nil)
		if want := (RunStats{Task: TaskSignin}); (*runs)[0] != want {
			t.Fatalf("stats = %+v, want %+v", (*runs)[0], want)
		}
	}
}

func TestContextCancelMidBatchExitsEarly(t *testing.T) {
	src := &fakeSource{accounts: activeAccounts()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	up := newFakeUpstream()
	up.signinFn = func(acc *Account) (SigninResult, error) {
		if acc.ID == "a2" {
			cancel() // shutdown signal lands mid-batch, after a2's own op
		}
		return SigninResult{CreditDelta: 1}, nil
	}
	store := &fakeStore{}
	deps, runs := newTestDeps(src, up, store)

	handler := NewSigninHandler(deps)
	err := handler(ctx) // pass the cancellable ctx through the untyped argument
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled batch error = %v, want context.Canceled", err)
	}

	// a1 and a2 completed; a3 was never attempted.
	assertEvents(t, up.calls, []string{"signin:workbuddy:a1", "signin:workbuddy:a2"})
	if want := (RunStats{Task: TaskSignin, Total: 3, Success: 2}); (*runs)[0] != want {
		t.Fatalf("stats = %+v, want %+v (stats still reported on abort)", (*runs)[0], want)
	}
}

func TestSourceErrorFailsWholeTask(t *testing.T) {
	listErr := errors.New("pool unavailable")
	src := &fakeSource{listErr: listErr}
	up := newFakeUpstream()
	store := &fakeStore{}
	deps, runs := newTestDeps(src, up, store)

	err := NewSigninHandler(deps)(nil)
	if !errors.Is(err, listErr) {
		t.Fatalf("source listing error must fail the task, got %v", err)
	}
	if want := (RunStats{Task: TaskSignin}); (*runs)[0] != want {
		t.Fatalf("stats = %+v, want %+v", (*runs)[0], want)
	}
}

func TestNilDepsFailsTaskWithName(t *testing.T) {
	if err := NewSigninHandler(nil)(nil); err == nil {
		t.Fatalf("nil deps must fail the task, got nil")
	}
	err := NewActiveCheckHandler(&Deps{})(nil)
	if err == nil || !strings.Contains(err.Error(), "Source") {
		t.Fatalf("partial deps error must name the missing field, got %v", err)
	}
}

func TestBuildDefaultTasksCronResolution(t *testing.T) {
	tasks := BuildDefaultTasks(nil)
	cronByName := map[string]string{}
	for _, task := range tasks {
		cronByName[task.Name] = task.CronSpec
		if !task.Enabled {
			t.Fatalf("task %q must default to enabled", task.Name)
		}
	}
	wantDefault := map[string]string{
		TaskSignin:         DefaultSigninCron,
		TaskActiveCheck:    DefaultActiveCheckCron,
		TaskCatTravel:      DefaultCatTravelCron,
		TaskTokenKeepalive: DefaultTokenKeepaliveCron,
	}
	if len(tasks) != len(wantDefault) {
		t.Fatalf("BuildDefaultTasks built %d tasks, want %d", len(tasks), len(wantDefault))
	}
	for name, wantCron := range wantDefault {
		if cronByName[name] != wantCron {
			t.Fatalf("task %q cron = %q, want %q", name, cronByName[name], wantCron)
		}
	}

	// Config overrides land for the three config-backed tasks; cat_travel has
	// no config key and must keep its code-truth spec (recorded mismatch).
	overridden := BuildDefaultTasks(&Deps{Crons: Crons{
		Signin:         "0 8 * * *",
		ActiveCheck:    "0 11 * * *",
		TokenKeepalive: "0 23 * * *",
	}})
	cronByName = map[string]string{}
	for _, task := range overridden {
		cronByName[task.Name] = task.CronSpec
	}
	if cronByName[TaskSignin] != "0 8 * * *" ||
		cronByName[TaskActiveCheck] != "0 11 * * *" ||
		cronByName[TaskTokenKeepalive] != "0 23 * * *" {
		t.Fatalf("config cron overrides not applied: %v", cronByName)
	}
	if cronByName[TaskCatTravel] != DefaultCatTravelCron {
		t.Fatalf("cat_travel has no config key; override must be impossible, got %q", cronByName[TaskCatTravel])
	}
}

func TestRegisterDefaultHandlersBindsAllFour(t *testing.T) {
	s := NewScheduler()
	RegisterDefaultHandlers(context.Background(), s, &Deps{})

	for _, name := range []string{TaskSignin, TaskActiveCheck, TaskCatTravel, TaskTokenKeepalive} {
		if s.handlers[name] == nil {
			t.Fatalf("handler for %q not registered", name)
		}
	}
}

func TestRegisteredHandlerBaseCtxExitsOnShutdown(t *testing.T) {
	// The scheduler invokes handlers as handler(nil); the registered wrapper
	// must fall back to the base context so cancelling it (gateway shutdown)
	// stops an in-flight batch mid-loop.
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()

	src := &fakeSource{accounts: activeAccounts()}
	up := newFakeUpstream()
	up.signinFn = func(acc *Account) (SigninResult, error) {
		if acc.ID == "a2" {
			cancelBase() // shutdown lands mid-batch
		}
		return SigninResult{CreditDelta: 1}, nil
	}
	store := &fakeStore{}
	deps, runs := newTestDeps(src, up, store)

	s := NewScheduler()
	RegisterDefaultHandlers(baseCtx, s, deps)

	err := s.handlers[TaskSignin](nil) // exactly how execute() calls it
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("registered handler under cancelled base = %v, want context.Canceled", err)
	}
	assertEvents(t, up.calls, []string{"signin:workbuddy:a1", "signin:workbuddy:a2"})
	if want := (RunStats{Task: TaskSignin, Total: 3, Success: 2}); (*runs)[0] != want {
		t.Fatalf("stats = %+v, want %+v", (*runs)[0], want)
	}
}

// handlerWithCtx invokes a TaskHandler with a real context through its
// untyped argument (the forward-compatible path of resolveCtx).
func handlerWithCtx(t *testing.T, ctx context.Context, h TaskHandler) error {
	t.Helper()
	return h(ctx)
}
