package host

// M10-B2 scheduling-assembly tests: the per-platform cron resolution
// (SchedulePlatformTasks: platform key wins, empty falls back to the frozen
// global key) and the platform-scoped account batches ("<task>@<platform>" —
// the run covers exactly its own platform's accounts).

import (
	"context"
	"testing"
	"time"

	"all2api/internal/config"
	"all2api/internal/schedule"
)

func TestSchedulePlatformTasksFallbackResolution(t *testing.T) {
	cfg := config.Config{
		Sources: map[string]config.SourceConfig{
			"trae": {}, "workbuddy": {}, "qoder": {}, // deliberately unsorted input
		},
		Schedule: config.ScheduleConfig{
			SigninCron:      "0 9,21 * * *",
			TaskCron:        "0 10 * * *",
			TokenKeepalive:  "0 22 * * *",
			SigninCronQoder: "30 7 * * *",
			CatTravelCron:   "15 20 * * *",
		},
	}

	specs := SchedulePlatformTasks(cfg)
	if len(specs) != 3 {
		t.Fatalf("got %d platform specs, want 3", len(specs))
	}
	// Deterministic sorted-by-id delivery.
	if specs[0].Platform != "qoder" || specs[1].Platform != "trae" || specs[2].Platform != "workbuddy" {
		t.Fatalf("platform order = %s,%s,%s — must be sorted ids", specs[0].Platform, specs[1].Platform, specs[2].Platform)
	}

	// qoder: its platform signin key wins; the other two fall back to globals.
	if specs[0].Signin != "30 7 * * *" {
		t.Fatalf("qoder signin = %q, want the platform key", specs[0].Signin)
	}
	if specs[0].ActiveCheck != "0 10 * * *" || specs[0].TokenKeepalive != "0 22 * * *" {
		t.Fatalf("qoder fallbacks = %q/%q, want the global keys", specs[0].ActiveCheck, specs[0].TokenKeepalive)
	}
	// trae: no platform keys at all → pure global fallback.
	if specs[1].Signin != "0 9,21 * * *" || specs[1].ActiveCheck != "0 10 * * *" || specs[1].TokenKeepalive != "0 22 * * *" {
		t.Fatalf("trae fallbacks drifted: %+v", specs[1])
	}
	// workbuddy: global fallbacks + the workbuddy-only cat_travel key.
	if specs[2].Signin != "0 9,21 * * *" {
		t.Fatalf("workbuddy signin = %q, want global fallback", specs[2].Signin)
	}
	if specs[2].CatTravel != "15 20 * * *" {
		t.Fatalf("workbuddy cat_travel = %q, want the cat_travel_cron key", specs[2].CatTravel)
	}
}

func TestSchedulePlatformTasksEndToEndNamesAndCrons(t *testing.T) {
	cfg := config.Config{
		Sources: map[string]config.SourceConfig{"workbuddy": {}, "qoder": {}},
		Schedule: config.ScheduleConfig{
			SigninCron:          "0 9,21 * * *",
			SigninCronWorkbuddy: "0 8 * * *",
		},
	}
	tasks := schedule.BuildPlatformTasks(SchedulePlatformTasks(cfg))
	got := map[string]string{}
	for _, task := range tasks {
		got[task.Name] = task.CronSpec
	}
	if got["signin@workbuddy"] != "0 8 * * *" {
		t.Fatalf("signin@workbuddy = %q, want the platform key", got["signin@workbuddy"])
	}
	if got["signin@qoder"] != "0 9,21 * * *" {
		t.Fatalf("signin@qoder = %q, want the global fallback", got["signin@qoder"])
	}
	if _, ok := got["cat_travel@workbuddy"]; !ok {
		t.Fatal("workbuddy must register its cat_travel task")
	}
	if _, ok := got["cat_travel@qoder"]; ok {
		t.Fatal("qoder must never register cat_travel")
	}
}

func TestPoolSourcePlatformScopedTaskNames(t *testing.T) {
	_, src, _ := newWiredHost(seededAccounts())
	ctx := context.Background()

	// A platform-scoped run covers exactly that platform — even where the
	// legacy capability table would have refused (qoder is not workbuddy-only
	// territory; the registration decides capability, the source obeys scope).
	batch, err := src.Accounts(ctx, schedule.PlatformTaskName(schedule.TaskSignin, "qoder"))
	if err != nil {
		t.Fatalf("scoped listing: %v", err)
	}
	if len(batch) != 1 || batch[0].ID != "q1" || batch[0].Platform != "qoder" {
		t.Fatalf("qoder-scoped batch = %+v, want only q1", batch)
	}

	batch, err = src.Accounts(ctx, schedule.PlatformTaskName(schedule.TaskCatTravel, "workbuddy"))
	if err != nil {
		t.Fatalf("workbuddy-scoped listing: %v", err)
	}
	if len(batch) != 1 || batch[0].ID != "w1" {
		t.Fatalf("workbuddy-scoped batch = %+v, want only w1", batch)
	}

	// A scope with no accounts answers empty, never cross-platform leakage.
	batch, err = src.Accounts(ctx, schedule.PlatformTaskName(schedule.TaskSignin, "nosuch"))
	if err != nil || len(batch) != 0 {
		t.Fatalf("unknown-platform scope = %+v err %v, want empty batch", batch, err)
	}
}

func TestPoolSourceScopedBatchMirrorsCooldown(t *testing.T) {
	sel, src, _ := newWiredHost(seededAccounts())
	// A cooled account keeps its identity in the batch — the handler's skip
	// predicate needs the live cooldown, not a filtered-out row.
	if !sel.SetAccountCooldown("qoder", "q1", time.Now().Add(time.Minute)) {
		t.Fatal("cooldown setup failed")
	}
	batch, err := src.Accounts(context.Background(), schedule.PlatformTaskName(schedule.TaskTokenKeepalive, "qoder"))
	if err != nil {
		t.Fatalf("scoped listing: %v", err)
	}
	if len(batch) != 1 || batch[0].ID != "q1" || !batch[0].IsCooling() {
		t.Fatalf("batch = %+v, want cooling q1 present (skip decisions stay with the handler)", batch)
	}
}
