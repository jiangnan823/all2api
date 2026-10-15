package host

// Wiring-layer tests for the schedule seam (sched.go): the pool-backed
// AccountSource platform filtering (including the unknown-taskType empty-batch
// contract), the AccountStore write-back onto real pool state, and the honest
// upstream stub's skip semantics. Deterministic — no sleeps, no cron.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"all2api/internal/pool"
	"all2api/internal/schedule"
)

// newWiredHost returns a selector seeded with the given accounts, plus the
// three schedule constructors bound to it — the exact assembly main.go uses.
func newWiredHost(accs []*pool.Account) (*pool.WeightedRandomSelector, schedule.AccountSource, schedule.AccountStore) {
	sel := pool.NewWeightedRandomSelector(100, 3, 60)
	for _, a := range accs {
		sel.AddAccount(a)
	}
	return sel, NewScheduleSource(sel), NewScheduleStore(sel)
}

func seededAccounts() []*pool.Account {
	return []*pool.Account{
		{ID: "w1", Platform: "workbuddy", Credit: 500, Health: 1.0, Status: "active"},
		{ID: "q1", Platform: "qoder", Credit: 500, Health: 1.0, Status: "active"},
		{ID: "t1", Platform: "trae", Credit: 500, Health: 1.0, Status: "active"},
	}
}

func TestPoolSourceFiltersByTaskPlatform(t *testing.T) {
	_, src, _ := newWiredHost(seededAccounts())
	ctx := context.Background()

	// Since M3b every task is workbuddy-only: the one wired schedule upstream
	// is the workbuddy task client, and the source must not offer accounts to
	// an upstream that cannot serve them.
	for _, task := range []string{schedule.TaskSignin, schedule.TaskActiveCheck, schedule.TaskCatTravel, schedule.TaskTokenKeepalive} {
		batch, err := src.Accounts(ctx, task)
		if err != nil {
			t.Fatalf("%s listing: %v", task, err)
		}
		if len(batch) != 1 || batch[0].ID != "w1" || batch[0].Platform != "workbuddy" {
			t.Fatalf("%s batch = %+v, want only workbuddy w1", task, batch)
		}
	}
}

func TestPoolSourceUnknownTaskTypeIsEmptyBatch(t *testing.T) {
	_, src, _ := newWiredHost(seededAccounts())

	batch, err := src.Accounts(context.Background(), "no_such_task")
	if err != nil {
		t.Fatalf("unknown task listing: %v", err)
	}
	if len(batch) != 0 {
		t.Fatalf("unknown taskType must yield an empty batch, got %d accounts (cross-platform misfire)", len(batch))
	}
}

func TestPoolSourceSnapshotIsValueCopy(t *testing.T) {
	sel, src, _ := newWiredHost(seededAccounts())

	batch, err := src.Accounts(context.Background(), schedule.TaskSignin)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}

	// Mutate the live pool after listing; the returned snapshots must not move.
	sel.SetAccountCooldown("workbuddy", "w1", time.Now().Add(time.Hour))
	for _, acc := range batch {
		if acc.ID == "w1" && acc.IsCooling() {
			t.Fatalf("snapshot aliased live pool state: w1 shows a cooldown it did not have at list time")
		}
	}
}

func TestPoolStoreWriteBackLands(t *testing.T) {
	sel, _, store := newWiredHost(seededAccounts())
	expiry := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cooldownUntil := time.Now().Add(30 * time.Minute)

	if err := store.AddCredit(context.Background(), "workbuddy", "w1", 25); err != nil {
		t.Fatalf("AddCredit: %v", err)
	}
	if err := store.SetStatus(context.Background(), "qoder", "q1", schedule.AccountDisabled); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := store.SetCooldown(context.Background(), "trae", "t1", cooldownUntil); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}
	if err := store.SetExpiry(context.Background(), "workbuddy", "w1", expiry); err != nil {
		t.Fatalf("SetExpiry: %v", err)
	}

	live := map[string]*pool.Account{}
	for _, acc := range sel.ListAccounts("workbuddy") {
		live[acc.ID] = acc
	}
	for _, acc := range sel.ListAccounts("qoder") {
		live[acc.ID] = acc
	}
	for _, acc := range sel.ListAccounts("trae") {
		live[acc.ID] = acc
	}
	if got := live["w1"].Credit; got != 525 {
		t.Fatalf("w1 credit = %d, want 525 (500+25)", got)
	}
	if got := live["w1"].ExpiresAt; !got.Equal(expiry) {
		t.Fatalf("w1 expiry = %v, want %v", got, expiry)
	}
	if got := live["q1"].Status; got != schedule.AccountDisabled {
		t.Fatalf("q1 status = %q, want %q", got, schedule.AccountDisabled)
	}
	if got := live["t1"].Status; got != schedule.AccountCooling {
		t.Fatalf("t1 status = %q, want cooling after SetCooldown", got)
	}
	if got := live["t1"].CooldownEnd; !got.Equal(cooldownUntil) {
		t.Fatalf("t1 cooldown end = %v, want %v", got, cooldownUntil)
	}
}

func TestPoolStoreUnknownAccountErrors(t *testing.T) {
	_, _, store := newWiredHost(seededAccounts())
	ctx := context.Background()

	for name, err := range map[string]error{
		"AddCredit":   store.AddCredit(ctx, "workbuddy", "ghost", 1),
		"SetStatus":   store.SetStatus(ctx, "workbuddy", "ghost", schedule.AccountActive),
		"SetCooldown": store.SetCooldown(ctx, "workbuddy", "ghost", time.Now()),
		"SetExpiry":   store.SetExpiry(ctx, "workbuddy", "ghost", time.Now()),
	} {
		if err == nil {
			t.Fatalf("%s on a vanished account must error (surfaces in RunStats.Failed), got nil", name)
		}
	}
}

// TestStubUpstreamSkipSemantics runs the real handler batch against the stub
// upstream (NewScheduleStubUpstream — the qoder/trae-build production shape)
// over a real pool: every op reports ErrUnsupportedTask, so handlers must
// count Skipped, write nothing back, and keep the batch success.
func TestStubUpstreamSkipSemantics(t *testing.T) {
	sel := pool.NewWeightedRandomSelector(100, 3, 60)
	sel.AddAccount(&pool.Account{ID: "w1", Platform: "workbuddy", Credit: 500, Health: 1.0, Status: "active"})

	deps := &schedule.Deps{
		Source:          NewScheduleSource(sel),
		Upstream:        NewScheduleStubUpstream(),
		Store:           NewScheduleStore(sel),
		FailureCooldown: 60 * time.Second,
	}
	var runs []schedule.RunStats
	deps.ReportStats = func(s schedule.RunStats) { runs = append(runs, s) }

	// All four handlers share the same stub outcome.
	for task, handler := range map[string]schedule.TaskHandler{
		schedule.TaskSignin:         schedule.NewSigninHandler(deps),
		schedule.TaskActiveCheck:    schedule.NewActiveCheckHandler(deps),
		schedule.TaskCatTravel:      schedule.NewCatTravelHandler(deps),
		schedule.TaskTokenKeepalive: schedule.NewTokenKeepaliveHandler(deps),
	} {
		if err := handler(nil); err != nil {
			t.Fatalf("%s batch against stub upstream must succeed (quiet skip), got %v", task, err)
		}
	}

	want := map[string]schedule.RunStats{
		schedule.TaskSignin:         {Task: schedule.TaskSignin, Total: 1, Skipped: 1},
		schedule.TaskActiveCheck:    {Task: schedule.TaskActiveCheck, Total: 1, Skipped: 1},
		schedule.TaskTokenKeepalive: {Task: schedule.TaskTokenKeepalive, Total: 1, Skipped: 1},
		schedule.TaskCatTravel:      {Task: schedule.TaskCatTravel, Total: 1, Skipped: 1},
	}
	if len(runs) != 4 {
		t.Fatalf("ReportStats called %d times, want 4", len(runs))
	}
	for _, got := range runs {
		wantStats, ok := want[got.Task]
		if !ok {
			t.Fatalf("unexpected task in stats: %q", got.Task)
		}
		if got != wantStats {
			t.Fatalf("%s stats = %+v, want %+v (stub must skip quietly, no failed count)", got.Task, got, wantStats)
		}
	}

	// Nothing was written back: w1 stays active with credit 500, no cooldown.
	for _, acc := range sel.ListAccounts("workbuddy") {
		if acc.Status != "active" || acc.Credit != 500 || !acc.CooldownEnd.IsZero() {
			t.Fatalf("stub skip must not write back, w1 now = %+v", acc)
		}
	}
}

func TestStubUpstreamErrorsWrapSentinel(t *testing.T) {
	up := NewScheduleStubUpstream()
	acc := &schedule.Account{ID: "w1", Platform: "workbuddy", Status: schedule.AccountActive}
	ctx := context.Background()

	if _, err := up.Signin(ctx, acc); !errors.Is(err, schedule.ErrUnsupportedTask) {
		t.Fatalf("stub Signin error = %v, want ErrUnsupportedTask wrap", err)
	}
	if err := up.ActiveCheck(ctx, acc); !errors.Is(err, schedule.ErrUnsupportedTask) {
		t.Fatalf("stub ActiveCheck error = %v, want ErrUnsupportedTask wrap", err)
	}
	if _, err := up.CatTravel(ctx, acc); !errors.Is(err, schedule.ErrUnsupportedTask) {
		t.Fatalf("stub CatTravel error = %v, want ErrUnsupportedTask wrap", err)
	}
	if _, err := up.TokenKeepalive(ctx, acc); !errors.Is(err, schedule.ErrUnsupportedTask) {
		t.Fatalf("stub TokenKeepalive error = %v, want ErrUnsupportedTask wrap", err)
	}
}

// TestDepsAssemblyContract pins the main.go wiring shape: a Deps built exactly
// like the gateway builds it passes validate-time usage for all four handlers
// (non-nil seams, and the handler set registers under the stable task names).
func TestDepsAssemblyContract(t *testing.T) {
	_, src, store := newWiredHost(seededAccounts())
	deps := &schedule.Deps{
		Source:          src,
		Upstream:        NewScheduleUpstream(nil), // real upstream shape; ops are not invoked here
		Store:           store,
		FailureCooldown: 60 * time.Second,
		Crons: schedule.Crons{
			Signin:         "0 8 * * *",
			ActiveCheck:    "0 11 * * *",
			TokenKeepalive: "0 23 * * *",
		},
	}

	s := schedule.NewScheduler()
	schedule.RegisterDefaultHandlers(context.Background(), s, deps)
	tasks := schedule.BuildDefaultTasks(deps)
	for _, task := range tasks {
		s.AddTask(task)
	}

	got := map[string]schedule.Task{}
	for _, t := range s.GetTasks() {
		got[t.Name] = *t
	}
	for name, wantCron := range map[string]string{
		schedule.TaskSignin:         "0 8 * * *",
		schedule.TaskActiveCheck:    "0 11 * * *",
		schedule.TaskCatTravel:      schedule.DefaultCatTravelCron, // no config key
		schedule.TaskTokenKeepalive: "0 23 * * *",
	} {
		task, ok := got[name]
		if !ok {
			t.Fatalf("task %q not registered", name)
		}
		if task.CronSpec != wantCron {
			t.Fatalf("task %q cron = %q, want %q", name, task.CronSpec, wantCron)
		}
		if !task.Enabled {
			t.Fatalf("task %q must default enabled", name)
		}
	}
}

// TestPoolWriteBackConcurrentSafety hammers the four scheduler setters and the
// request-path mutators concurrently against one account — the applyAccount
// lock discipline must hold (run under -race).
func TestPoolWriteBackConcurrentSafety(t *testing.T) {
	sel := pool.NewWeightedRandomSelector(100, 3, 60)
	sel.AddAccount(&pool.Account{ID: "w1", Platform: "workbuddy", Credit: 1000, Health: 1.0, Status: "active"})

	const goroutines = 8
	const iterations = 200
	var wg sync.WaitGroup
	start := make(chan struct{})

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				sel.ApplyAccountCredit("workbuddy", "w1", 1)
				sel.SetAccountStatus("workbuddy", "w1", "active")
				sel.SetAccountCooldown("workbuddy", "w1", time.Now().Add(time.Minute))
				sel.SetAccountExpiry("workbuddy", "w1", time.Now().Add(time.Hour))
				sel.RecoverAccount("workbuddy", "w1") // clears the cooldown again
				// Interleave a value snapshot read (the source path).
				_ = sel.SnapshotAccounts()
				sel.UpdateHealth("workbuddy", "w1", "success", time.Millisecond)
			}
		}()
	}
	close(start)
	wg.Wait()

	// Every ApplyAccountCredit must have landed: 8 × 200 × +1.
	for _, acc := range sel.ListAccounts("workbuddy") {
		if acc.Credit != 1000+goroutines*iterations {
			t.Fatalf("credit = %d, want %d (lost updates under concurrency)", acc.Credit, 1000+goroutines*iterations)
		}
	}
}
