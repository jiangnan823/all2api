package api

// POST /api/config — M2 second batch (ConfigSaveResponse contract,
// api-contract.ts v0.5.1 §配置). The panel form is backend-driven (UI §5.13):
// it renders exactly the FieldDefs delivered by GET /api/config/fields and
// PATCHes back ConfigPatch entries — `[{key, value}, ...]` where key is the
// same JSON path (e.g. "sources.qoder.throttle.steady_rpm").
//
// Contract rulings encoded here (each traceable to the frozen contract or a
// recorded M2 decision — see the PR notes):
//   - ConfigSaveResponse = {ok, applied?, field_errors?}; field-level
//     rejections are 400 with per-field entries (FieldError), never a single
//     "save failed" sentence (v0.5 named type shared with AuthVerifyResponse).
//   - Unknown keys are REJECTED (400, code omitted), not silently ignored:
//     the patchable set is exactly what GET /api/config/fields delivers, and
//     a silent ignore would let a typo'd danger-zone key report success while
//     saving nothing. The frozen contract does not state ignore-vs-reject →
//     conservative reject (M2 decision, flagged to the integrator).
//   - listen and api_key ARE patchable (they are delivered as gateway
//     FieldDefs) but carry effect=restart_required: the value is persisted to
//     disk and hot-swapped into config.Shared, while the HTTP listener keeps
//     serving on the old bind until process restart (a listener cannot
//     re-bind mid-request). The contract defines no blocking semantics for
//     restart_required beyond the display annotation, so save-then-restart is
//     the conservative reading.
//   - ConfigPatch element keys are frozen to {key, value} (anonymous type in
//     the contract); extra JSON fields on an element are unknown → 400.
//
// Concurrency discipline: readers take config.Shared.Get() snapshots (RWMutex
// inside the config package). This handler works on a private clone, validates
// it, swaps it in with a single Update under the write lock, and only then
// touches the disk. A concurrent Get() reader therefore observes either the
// whole old or the whole new struct — never a half-merged one — and a failing
// disk write is followed by an in-memory rollback to the pre-patch clone, so
// memory can never diverge from disk.
//
// Integrator seam: the write-back path is the path recorded by the last
// config.Load (config.Shared.Path), which main.go resolves as
// -config flag → ALL2API_CONFIG env → ./config.json. Only the degenerate
// "booted on defaults because the file was missing" case needs a fallback,
// and it re-resolves env → ./config.json here. The -config flag leg is
// unreachable in that case (a -config run always records its path on Load),
// so parsing os.Args inside a library would be both wrong and untestable —
// if main.go's chain ever changes, this fallback must move with it.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"all2api/internal/adapter"
	"all2api/internal/config"
)

// saveFilePerm / saveDirPerm follow the trail.go posture: config (which
// carries api_key) must not be group/world readable.
const (
	saveFilePerm = 0o600
	saveDirPerm  = 0o700
)

// compiledStringRules holds the per-key textual patterns (table in
// configfields.go), compiled once.
var compiledStringRules = func() map[string]*regexp.Regexp {
	out := make(map[string]*regexp.Regexp, len(stringRules))
	for k, v := range stringRules {
		out[k] = regexp.MustCompile(v)
	}
	return out
}()

// patchEntry is one ConfigPatch element (contract: Array<{key, value}>).
type patchEntry struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

type configSaveResponse struct {
	OK          bool                 `json:"ok"`
	Applied     *panelConfigResponse `json:"applied,omitempty"`
	FieldErrors []adapter.FieldError `json:"field_errors,omitempty"`
}

func fieldErr(key, msg, code string) adapter.FieldError {
	return adapter.FieldError{Key: key, Message: msg, Code: code}
}

func handleConfigSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	var patch []patchEntry
	if err := dec.Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	// The wire type is an array of {key, value}; a bare value, object or null
	// decodes to a nil slice — malformed input, not an empty patch.
	if patch == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "config patch must be an array"})
		return
	}

	// Pass 1 — shape validation, state-free: key known in the delivered set,
	// value a JSON literal of the right type. Nothing is mutated yet.
	if errs := validatePatch(patch); len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, configSaveResponse{OK: false, FieldErrors: errs})
		return
	}

	// Pass 2 — apply onto a private clone and validate value semantics
	// (bounds, formats) against the same spec tables delivery uses. The
	// clone must be independent of `before`: maps are re-deep-copied here so
	// the failure-path rollback still has the untouched pre-patch snapshot.
	before := config.Shared.Clone()
	next := config.Shared.Clone()
	if errs := applyPatch(&next, patch); len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, configSaveResponse{OK: false, FieldErrors: errs})
		return
	}

	config.Shared.Update(next)
	if err := writeConfigFileAtomic(savePath(), next); err != nil {
		// Disk lost the race: roll the hot copy back so memory never diverges
		// from disk. before is exactly the state every concurrent reader saw
		// one Update ago.
		config.Shared.Update(before)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "config write failed"})
		return
	}

	panel, err := buildPanelConfig(config.Shared.Get())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "config unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, configSaveResponse{OK: true, Applied: &panel})
}

// savePath returns the file the running process loaded from, or — when boot
// fell back to defaults because the file was absent — the env/default leg of
// main.go's resolution chain (see file docs for the seam note).
func savePath() string {
	if p := config.Shared.Path(); p != "" {
		return p
	}
	if p := os.Getenv("ALL2API_CONFIG"); p != "" {
		return p
	}
	return "./config.json"
}

// validatePatch checks patch shape per entry, before anything is mutated:
// key known, value a JSON literal, JSON type matching the FieldDef type.
// It never reads config state, so its verdict cannot race a concurrent save.
func validatePatch(patch []patchEntry) []adapter.FieldError {
	var errs []adapter.FieldError
	seen := make(map[string]bool, len(patch))
	for _, e := range patch {
		if seen[e.Key] {
			errs = append(errs, fieldErr(e.Key, "config.duplicate_key", ""))
			continue
		}
		seen[e.Key] = true

		kind, ok := patchTargetType(e.Key)
		if !ok {
			errs = append(errs, fieldErr(e.Key, "config.unknown_key", ""))
			continue
		}
		var v any
		if err := json.Unmarshal(e.Value, &v); err != nil {
			errs = append(errs, fieldErr(e.Key, "config.invalid_json", ""))
			continue
		}
		if !valueMatchesType(v, kind) {
			errs = append(errs, fieldErr(e.Key, "config.type_mismatch", ""))
		}
	}
	return errs
}

// valueMatchesType mirrors the FieldDef type enum for patch values. Numeric
// kinds additionally require integer values: every numeric config field in
// the frozen surface is integer-semantics (G1: rpm 整数), and a silent
// 4.5→4 truncation would report success for a value the panel never meant.
// Note number_array: min_gap_ms is one array-valued field [lo,hi] (M2 B2
// ruling 2).
func valueMatchesType(v any, kind string) bool {
	switch kind {
	case "number":
		f, ok := v.(float64)
		return ok && integralValue(f)
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "cron":
		s, ok := v.(string)
		return ok && validCron(s)
	case "number_array":
		arr, ok := v.([]any)
		if !ok || len(arr) != 2 {
			return false
		}
		for _, item := range arr {
			f, ok := item.(float64)
			if !ok || !integralValue(f) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// integralValue is the integer-semantics predicate shared by the shape gate
// and the apply gate (defense in depth — applyOne re-checks via
// integralOrErr so the two gates can never diverge).
func integralValue(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f == math.Trunc(f)
}

// patchTargetType resolves a wire key to its FieldDef type. Delivery-set
// parity is by construction: throttle columns come from the shared spec
// tables (configfields.go), the per-source filters re-use the same
// jitterSupported / halveBudgetSupported predicates as the delivery path,
// and the global groups mirror the configfields.go group tables 1:1.
func patchTargetType(key string) (string, bool) {
	// Global groups (gateway / routing / pooling / schedule).
	for _, s := range globalStringSpecs {
		if key == s {
			return "string", true
		}
	}
	for _, s := range globalBoolSpecs {
		if key == s {
			return "boolean", true
		}
	}
	for _, s := range cronSpecs {
		if key == s {
			return "cron", true
		}
	}
	for _, sp := range poolSpecs {
		if key == sp.key {
			return "number", true
		}
	}

	// Per-source keys: sources.<id>.(throttle.*|advanced.request_fingerprint_rewrite|enabled)
	rest, ok := strings.CutPrefix(key, "sources.")
	if !ok {
		return "", false
	}
	id, leaf, found := strings.Cut(rest, ".")
	if !found || id == "" || leaf == "" {
		return "", false
	}
	switch leaf {
	case "enabled":
		return "boolean", true
	case "advanced.request_fingerprint_rewrite":
		return "boolean", true
	case "throttle.min_gap_ms":
		// One array-valued column inside the shared numeric table.
		if _, ok := lookupThrottleSpec(id, leaf); ok {
			return "number_array", true
		}
		return "", false
	}
	if _, ok := lookupThrottleSpec(id, leaf); ok {
		return "number", true
	}
	if leaf == "throttle.halve_budget_after_429" && halveBudgetSupported(id) {
		return "boolean", true
	}
	return "", false
}

// lookupThrottleSpec finds the shared spec for "<id> + throttle leaf",
// applying the same per-source filter as delivery.
func lookupThrottleSpec(id, leaf string) (*throttleBoundSpec, bool) {
	suffix := "throttle." + strings.TrimPrefix(leaf, "throttle.")
	for i := range throttleColumnSpecs {
		if throttleColumnSpecs[i].key == suffix {
			return &throttleColumnSpecs[i], true
		}
	}
	if jitterSupported(id) && jitterSpec.key == suffix {
		return &jitterSpec, true
	}
	return nil, false
}

// applyPatch mutates the private clone and validates value semantics
// (bounds, textual formats) against the shared spec tables. Called only
// after validatePatch passed, but re-checks defensively.
func applyPatch(cfg *config.Config, patch []patchEntry) []adapter.FieldError {
	var errs []adapter.FieldError
	for _, e := range patch {
		if fe := applyOne(cfg, e); fe != nil {
			errs = append(errs, *fe)
		}
	}
	return errs
}

// applyOne applies a single (validated) entry to the clone.
func applyOne(cfg *config.Config, e patchEntry) *adapter.FieldError {
	var v any
	if err := json.Unmarshal(e.Value, &v); err != nil {
		fe := fieldErr(e.Key, "config.invalid_json", "")
		return &fe
	}

	// ---- global groups ----
	for _, k := range globalStringSpecs {
		if e.Key != k {
			continue
		}
		s, _ := v.(string)
		if re := compiledStringRules[k]; re != nil && !re.MatchString(s) {
			fe := fieldErr(k, "config.invalid_format", "")
			return &fe
		}
		switch k {
		case "listen":
			cfg.Listen = s
		case "api_key":
			cfg.APIKey = strings.TrimSpace(s)
		case "external_base_url":
			cfg.ExternalBaseURL = strings.TrimSpace(s)
		}
		return nil
	}
	for _, k := range globalBoolSpecs {
		if e.Key != k {
			continue
		}
		b, _ := v.(bool)
		switch k {
		case "router.responses_api.enabled":
			cfg.Router.Responses.Enabled = b
		}
		return nil
	}
	for _, k := range cronSpecs {
		if e.Key != k {
			continue
		}
		s, _ := v.(string)
		if !validCron(s) {
			fe := fieldErr(k, "config.invalid_cron", "")
			return &fe
		}
		switch k {
		case "schedule.signin_cron":
			cfg.Schedule.SigninCron = s
		case "schedule.task_cron":
			cfg.Schedule.TaskCron = s
		case "schedule.token_keepalive_cron":
			cfg.Schedule.TokenKeepalive = s
		}
		return nil
	}
	for i := range poolSpecs {
		sp := &poolSpecs[i]
		if e.Key != sp.key {
			continue
		}
		n, _ := v.(float64)
		if fe := integralOrErr(sp.key, n); fe != nil {
			return fe
		}
		if n < sp.min || n > sp.max {
			fe := fieldErr(sp.key, "config.out_of_range", adapter.CodeRange)
			return &fe
		}
		switch sp.key {
		case "pool.credit_floor":
			cfg.Pool.CreditFloor = int(n)
		case "pool.max_retries":
			cfg.Pool.MaxRetries = int(n)
		case "pool.cooldown_sec":
			cfg.Pool.CooldownSec = int64(n)
		}
		return nil
	}

	// ---- per-source groups ----
	rest, ok := strings.CutPrefix(e.Key, "sources.")
	if !ok {
		fe := fieldErr(e.Key, "config.unknown_key", "")
		return &fe
	}
	id, leaf, found := strings.Cut(rest, ".")
	if !found || id == "" || leaf == "" {
		fe := fieldErr(e.Key, "config.unknown_key", "")
		return &fe
	}
	src, exists := cfg.Sources[id]
	if !exists {
		fe := fieldErr(e.Key, "config.unknown_source", "")
		return &fe
	}

	switch leaf {
	case "enabled":
		b, _ := v.(bool)
		src.Enabled = b
		cfg.Sources[id] = src
		return nil
	case "advanced.request_fingerprint_rewrite":
		b, _ := v.(bool)
		src.Advanced.RequestFingerprintRewrite = b
		cfg.Sources[id] = src
		return nil
	}

	spec, ok := lookupThrottleSpec(id, leaf)
	if !ok {
		if leaf == "throttle.halve_budget_after_429" && halveBudgetSupported(id) {
			b, _ := v.(bool)
			src.Throttle.HalveBudgetAfter429 = b
			cfg.Sources[id] = src
			return nil
		}
		fe := fieldErr(e.Key, "config.unknown_key", "")
		return &fe
	}

	if leaf == "throttle.min_gap_ms" {
		arr, ok := v.([]any)
		if !ok || len(arr) != 2 {
			fe := fieldErr(e.Key, "config.invalid_format", "")
			return &fe
		}
		lo, loOK := arr[0].(float64)
		hi, hiOK := arr[1].(float64)
		if !loOK || !hiOK {
			fe := fieldErr(e.Key, "config.type_mismatch", "")
			return &fe
		}
		if fe := integralOrErr(e.Key, lo); fe != nil {
			return fe
		}
		if fe := integralOrErr(e.Key, hi); fe != nil {
			return fe
		}
		if lo < spec.min || lo > spec.max || hi < spec.min || hi > spec.max {
			fe := fieldErr(e.Key, "config.out_of_range", adapter.CodeRange)
			return &fe
		}
		if lo > hi { // interval, not coordinates: normalize the order
			lo, hi = hi, lo
		}
		src.Throttle.MinGapMS = [2]int{int(lo), int(hi)}
		cfg.Sources[id] = src
		return nil
	}

	n, _ := v.(float64)
	if fe := integralOrErr(e.Key, n); fe != nil {
		return fe
	}
	if n < spec.min || n > spec.max {
		fe := fieldErr(e.Key, "config.out_of_range", adapter.CodeRange)
		return &fe
	}
	switch leaf {
	case "throttle.steady_rpm":
		src.Throttle.SteadyRPM = int(n)
	case "throttle.burst_rpm":
		src.Throttle.BurstRPM = int(n)
	case "throttle.daily_cap":
		src.Throttle.DailyCap = int(n)
	case "throttle.inflight":
		src.Throttle.Inflight = int(n)
	case "throttle.jitter_minutes":
		src.Throttle.JitterMinutes = int(n)
	case "throttle.backoff.base_429_sec":
		src.Throttle.Backoff.Base429Sec = int64(n)
	case "throttle.backoff.not_found_sec":
		src.Throttle.Backoff.NotFoundSec = int64(n)
	default:
		fe := fieldErr(e.Key, "config.unknown_key", "")
		return &fe
	}
	cfg.Sources[id] = src
	return nil
}

// integralOrErr rejects fractional numbers with a dedicated per-field error:
// every numeric config field in the frozen surface is integer-semantics
// (G1: rpm 整数), and a silent 4.5→4 truncation would report success for a
// value the panel never meant. Non-finite values cannot pass JSON decoding
// into float64 except via overflow; they are rejected all the same.
func integralOrErr(key string, f float64) *adapter.FieldError {
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		fe := fieldErr(key, "config.not_an_integer", "")
		return &fe
	}
	return nil
}

// validCron enforces exactly the cron shape the scheduler can honor: five
// whitespace-separated fields of *, or comma-separated integers inside each
// field's bounds. The runtime parser (internal/schedule.parseCronField)
// supports nothing beyond that — it silently ignores range/step tokens and
// out-of-range numbers, so accepting "0-30 * * * *" or "0 25 * * *" at save
// time would write a spec that never fires as meant. A malformed save is a
// 400, not a silently dead schedule. One check serves both gates: the shape
// gate (validatePatch) and the apply gate (applyOne) must not disagree.
func validCron(s string) bool {
	fields := strings.Fields(s)
	if len(fields) != 5 {
		return false
	}
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	for i, f := range fields {
		for _, part := range strings.Split(f, ",") {
			if part == "*" {
				continue
			}
			n, err := strconv.Atoi(part)
			if err != nil || n < bounds[i][0] || n > bounds[i][1] {
				return false
			}
		}
	}
	return true
}

// writeConfigFileAtomic writes cfg to path via temp file + rename in the same
// directory, creating the directory when needed. A failure at any point
// leaves the original file untouched (os.Rename over the target is atomic on
// POSIX and same-volume Windows NTFS); the caller rolls back the hot copy.
func writeConfigFileAtomic(path string, cfg config.Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if mkErr := os.MkdirAll(dir, saveDirPerm); mkErr != nil {
		return fmt.Errorf("create config dir: %w", mkErr)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, saveFilePerm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename config: %w", err)
	}
	tmpName = "" // renamed: the deferred cleanup must not delete the live file
	return nil
}
