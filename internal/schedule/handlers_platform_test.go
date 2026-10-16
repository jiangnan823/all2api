package schedule

// M10-B2 per-platform task assembly tests: BuildPlatformTasks naming/source/
// cron resolution, the workbuddy-only cat_travel capability, and the
// per-platform handler binding.

import (
	"context"
	"testing"
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
