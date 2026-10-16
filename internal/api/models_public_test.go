package api

import (
	"context"
	"slices"
	"testing"
)

// TestPublicModelListBareAndNamespaced pins the /v1/models id contract: every
// catalog row also appears under its namespaced id (the precision form two
// platforms sharing one model rely on), and bare ids stay deduped.
func TestPublicModelListBareAndNamespaced(t *testing.T) {
	rows := []modelWire{
		{Platform: "qoder", Provider: "qoder", ID: "qoder/DeepSeek-V4-Pro", Model: "DeepSeek-V4-Pro"},
		{Platform: "workbuddy", Provider: "workbuddy", ID: "workbuddy/deepseek-v4-pro", Model: "deepseek-v4-pro"},
		{Platform: "workbuddy", Provider: "workbuddy", ID: "workbuddy/kimi-k3", Model: "kimi-k3"},
	}
	oldBuild := buildModelRowsHook
	buildModelRowsHook = func(context.Context, upstreamCatalog, bool) ([]modelWire, string) {
		return rows, "mixed"
	}
	t.Cleanup(func() { buildModelRowsHook = oldBuild })

	out := PublicModelList(context.Background(), nil)
	ids := make([]string, 0, len(out))
	for _, m := range out {
		ids = append(ids, m.ID)
	}
	for _, want := range []string{
		"DeepSeek-V4-Pro", "deepseek-v4-pro", "kimi-k3", // bare, deduped
		"qoder/DeepSeek-V4-Pro", "workbuddy/deepseek-v4-pro", "workbuddy/kimi-k3", // precision forms
	} {
		if !slices.Contains(ids, want) {
			t.Fatalf("PublicModelList missing %q; got %v", want, ids)
		}
	}
	if n := len(ids); n != 6 {
		t.Fatalf("ids = %d rows (%v), want 6", n, ids)
	}
	// Deterministic delivery: sorted by id.
	if !slices.IsSorted(ids) {
		t.Fatalf("ids not sorted: %v", ids)
	}
}
