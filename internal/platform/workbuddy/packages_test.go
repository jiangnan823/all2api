//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

package workbuddy

// ListPackages tests (packages.go) run exclusively against the shared taskStub
// httptest fake upstream — no real endpoint is ever contacted (task hard
// constraint). Shapes follow the recorded balance wire: data.Response.Data.
// Accounts[] rows with PackageName / CycleEndTime / CapacityUnit / the cycle
// capacity pair, plus the live-evidence package spread (trial + fission packs
// with independent expiries).

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// resourceReply wraps one Accounts[] row list into the full envelope shape.
func resourceReply(rows string) string {
	return `{"code":0,"msg":"ok","data":{"Response":{"Data":{"Accounts":[` + rows + `]}}}}`
}

func TestListPackagesFefoMultiPackShape(t *testing.T) {
	stub, c, cred := newTaskStub(t)
	// Upstream order deliberately NOT sorted: 11-30 first, 10-31 second, plus
	// a never-expiring pack — the reply must come back FEFO.
	stub.script(pathGetUserResource, taskReply{body: resourceReply(
		`{"CapacityUnit":"credits","PackageName":"十一月裂变包","CycleCapacityRemain":1200,"CycleCapacitySize":1500,"CycleEndTime":"2026-11-30 23:59:59"},` +
			`{"CapacityUnit":"credits","PackageName":"CodeBuddy个人体验版","CycleCapacityRemain":500,"CycleCapacitySize":500,"CycleEndTime":"2026-10-31 23:59:59"},` +
			`{"CapacityUnit":"credits","PackageName":"终身包","CycleCapacityRemain":88,"CycleCapacitySize":100}`)})

	pkgs, err := c.ListPackages(context.Background(), cred)
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	if len(pkgs) != 3 {
		t.Fatalf("packages = %d, want 3", len(pkgs))
	}
	wantNames := []string{"CodeBuddy个人体验版", "十一月裂变包", "终身包"}
	for i, name := range wantNames {
		if pkgs[i].PackageName != name {
			t.Errorf("pkgs[%d].PackageName = %q, want %q (FEFO ascending, never-expiring last)", i, pkgs[i].PackageName, name)
		}
	}
	// Row zero is the frozen wire acceptance row: remain/total/cycle_end.
	if pkgs[0].Remain != 500 || pkgs[0].Total != 500 {
		t.Errorf("pkgs[0] remain/total = %d/%d, want 500/500", pkgs[0].Remain, pkgs[0].Total)
	}
	// 2026-10-31 23:59:59 UTC+8 == 15:59:59 UTC on the same day.
	wantEnd := time.Date(2026, 10, 31, 23, 59, 59, 0, packageEndZone)
	if !pkgs[0].CycleEnd.Equal(wantEnd) {
		t.Errorf("pkgs[0].CycleEnd = %v, want %v (parsed in UTC+8)", pkgs[0].CycleEnd, wantEnd)
	}
	if got := pkgs[0].CycleEnd.UTC(); got.Hour() != 15 || got.Minute() != 59 {
		t.Errorf("pkgs[0].CycleEnd UTC = %v, want 15:59:59Z (8h offset)", got)
	}
	// The never-expiring pack keeps the zero time.
	if !pkgs[2].CycleEnd.IsZero() {
		t.Errorf("pkgs[2].CycleEnd = %v, want zero (no expiry)", pkgs[2].CycleEnd)
	}
	// Same endpoint, empty-object POST as GetUserResource.
	call := stub.callsFor(pathGetUserResource)[0]
	if call.Method != http.MethodPost || call.Body != "{}" {
		t.Errorf("call = %s %s, want POST with the empty object body", call.Method, call.Body)
	}
}

func TestListPackagesFiltersNonCreditsUnits(t *testing.T) {
	stub, c, cred := newTaskStub(t)
	stub.script(pathGetUserResource, taskReply{body: resourceReply(
		`{"CapacityUnit":"tokens","PackageName":"Token钱包","CycleCapacityRemain":999000,"CycleCapacitySize":888000,"CycleEndTime":"2026-12-01 00:00:00"},` +
			`{"CapacityUnit":"Credits","PackageName":"大写单位包","CycleCapacityRemain":7,"CycleCapacitySize":9,"CycleEndTime":"2026-10-20 23:59:59"},` +
			`{"PackageName":"无单位行","CycleCapacityRemain":1}`)})

	pkgs, err := c.ListPackages(context.Background(), cred)
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	// Only the credits rows survive — the token wallet and the unit-less row
	// never leak into the expiry view (case-insensitive match, same fold as
	// GetUserResource).
	if len(pkgs) != 1 {
		t.Fatalf("packages = %d, want 1 (non-credits rows filtered)", len(pkgs))
	}
	if pkgs[0].PackageName != "大写单位包" || pkgs[0].Remain != 7 || pkgs[0].Total != 9 {
		t.Errorf("pkgs[0] = %+v, want 大写单位包 7/9", pkgs[0])
	}
}

func TestListPackagesTolerantRowFacts(t *testing.T) {
	stub, c, cred := newTaskStub(t)
	// Missing numbers keep zero; empty/garbled CycleEndTime means "never
	// expires" — one malformed pack must not hide the others.
	stub.script(pathGetUserResource, taskReply{body: resourceReply(
		`{"CapacityUnit":"credits","PackageName":"数字残缺包","CycleEndTime":"2026-11-01 23:59:59"},` +
			`{"CapacityUnit":"credits","PackageName":"日期残缺包","CycleCapacityRemain":10,"CycleCapacitySize":20,"CycleEndTime":"not-a-date"},` +
			`{"CapacityUnit":"credits","PackageName":"空日期包","CycleCapacityRemain":30,"CycleCapacitySize":40,"CycleEndTime":""}`)})

	pkgs, err := c.ListPackages(context.Background(), cred)
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	if len(pkgs) != 3 {
		t.Fatalf("packages = %d, want 3 (malformed facts never drop the row)", len(pkgs))
	}
	if pkgs[0].Remain != 0 || pkgs[0].Total != 0 {
		t.Errorf("pkgs[0] remain/total = %d/%d, want 0/0", pkgs[0].Remain, pkgs[0].Total)
	}
	for pos, want := range map[int]string{1: "日期残缺包", 2: "空日期包"} {
		if !pkgs[pos].CycleEnd.IsZero() {
			t.Errorf("pkgs[%d] (%s) CycleEnd = %v, want zero", pos, want, pkgs[pos].CycleEnd)
		}
	}
	// FEFO across the mixed shape: the one dated pack first, then the
	// never-expiring pair in upstream order.
	if pkgs[0].PackageName != "数字残缺包" || pkgs[1].PackageName != "日期残缺包" || pkgs[2].PackageName != "空日期包" {
		t.Errorf("order = %s,%s,%s — want the dated pack first, upstream order kept after",
			pkgs[0].PackageName, pkgs[1].PackageName, pkgs[2].PackageName)
	}
}

func TestListPackagesMissingDataErrors(t *testing.T) {
	// Same tolerance contract as GetUserResource: any missing envelope path is
	// an explicit error — never a panic, never a fabricated empty success.
	for name, body := range map[string]string{
		"no data":        `{"code":0,"msg":"ok"}`,
		"no Response":    `{"code":0,"msg":"ok","data":{}}`,
		"no Data":        `{"code":0,"msg":"ok","data":{"Response":{}}}`,
		"no Accounts":    `{"code":0,"msg":"ok","data":{"Response":{"Data":{}}}}`,
		"no credits row": resourceReply(`{"CapacityUnit":"tokens","CycleCapacityRemain":1}`),
		"not json":       `<html>gateway noise</html>`,
	} {
		stub, c, cred := newTaskStub(t)
		stub.script(pathGetUserResource, taskReply{body: body})
		pkgs, err := c.ListPackages(context.Background(), cred)
		if err == nil {
			t.Fatalf("%s: err = nil, want an explicit shape error", name)
		}
		if pkgs != nil {
			t.Fatalf("%s: pkgs = %v, want nil", name, pkgs)
		}
		if !strings.Contains(err.Error(), "workbuddy:") {
			t.Fatalf("%s: err = %v, want the workbuddy-framed reason", name, err)
		}
	}
}

func TestListPackagesUpstreamFailurePassesThrough(t *testing.T) {
	stub, c, cred := newTaskStub(t)
	stub.script(pathGetUserResource, taskReply{status: http.StatusInternalServerError, body: `{"code":500,"msg":"billing down"}`})
	if _, err := c.ListPackages(context.Background(), cred); err == nil {
		t.Fatal("err = nil, want the classified upstream failure")
	}
}
