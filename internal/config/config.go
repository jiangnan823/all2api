package config

import (
	"encoding/json"
	"os"
	"sync"
)

// Config is the root configuration loaded from config.json.
// G1 ruling (PM seq=340): per-source control — including throttle — lives ONLY
// under sources.<id>.*; the former top-level "throttle" map is removed (two
// writable spots with no precedence was the 60x-incident seed).
type Config struct {
	Listen          string                  `json:"listen"`
	APIKey          string                  `json:"api_key"`
	AuthDir         string                  `json:"auth_dir"`
	StateFile       string                  `json:"state_file"`
	ExternalBaseURL string                  `json:"external_base_url"` // upstream proxy path; frontend reads via __ALL2API_BASE__
	NoticeVersion   string                  `json:"notice_version"`    // served via GET /api/config; bump forces re-ack (G3/Q11, global single value per O-3)
	Pool            PoolConfig              `json:"pool"`
	Schedule        ScheduleConfig          `json:"schedule"`
	Sources         map[string]SourceConfig `json:"sources"`
	Router          RouterConfig            `json:"router"`
	ModelMap        map[string]string       `json:"model_map"` // model name → platform/model
}

// PoolConfig pool-level settings adapted from Panel's design.
type PoolConfig struct {
	CreditFloor int   `json:"credit_floor"` // skip accounts below this credit
	MaxRetries  int   `json:"max_retries"`
	CooldownSec int64 `json:"cooldown_sec"`
}

// ThrottleConfig is per-source rate limiting (G2/Q12), JSON path
// sources.<id>.throttle.*. Unit ruling (G1): every rate field is per-MINUTE
// (_rpm, integer). The former per-second fields are retired (see fronted
// tags-table §2 deprecation list) — writing the conservative "6" into a
// per-second field means 360 rpm, 60x the approved budget.
type ThrottleConfig struct {
	SteadyRPM           int           `json:"steady_rpm"`                       // sustained requests per minute — the ONLY steady-rate key
	BurstRPM            int           `json:"burst_rpm"`                        // burst ceiling per minute
	MinGapMS            [2]int        `json:"min_gap_ms"`                       // random inter-request gap [lo,hi] ms (kills machine cadence)
	DailyCap            int           `json:"daily_cap"`                        // requests per local day (quota anchor next-04:00)
	Inflight            int           `json:"inflight"`                         // max concurrent in-flight requests on this source
	Backoff             BackoffConfig `json:"backoff"`                          // per-reason backoff policy
	JitterMinutes       int           `json:"jitter_minutes,omitempty"`         // ± minutes on signin/task schedules, default 15
	HalveBudgetAfter429 bool          `json:"halve_budget_after_429,omitempty"` // Qoder stance: halve remaining daily cap for the day after a 429
}

// BackoffConfig cooldown semantics by trigger reason.
// base_429_sec ≥600 traces to PRD §7.1; not_found_sec=300 is a product default
// (engineering suggestion, adjustable — NOT a platform term, PM O-2 ruling).
type BackoffConfig struct {
	Base429Sec      int64  `json:"base_429_sec"`      // exponential backoff base on 429
	NotFoundSec     int64  `json:"not_found_sec"`     // cooldown when upstream resource disappears
	Exhaust402Until string `json:"exhaust_402_until"` // fixed "next-04:00"; display must stay "额度耗尽·次日04:00重置", NEVER "冷却中" (402 red line)
}

// AdvancedConfig holds opt-in high-risk per-source switches (PM O-6 ruling:
// parent path is sources.<id>.advanced.*). Request-feature/fingerprint
// rewriting defaults to false as a product stance (PM 调研 §4.1 / PRD §3.3 R6):
// enabling it upgrades "灰色使用" to "风控规避" and requires DangerAction
// double-confirm in the panel, default focus on 「保持关闭」.
type AdvancedConfig struct {
	RequestFingerprintRewrite bool `json:"request_fingerprint_rewrite"` // frozen leaf name (UI seq=349 G4); default false
}

// SourceConfig per-source control including disable switch (一键摘源 = enabled:false + hot reload).
type SourceConfig struct {
	Enabled        bool           `json:"enabled"`
	Throttle       ThrottleConfig `json:"throttle"`
	Advanced       AdvancedConfig `json:"advanced"`
	CredentialsDir string         `json:"credentials_dir"`
}

// ScheduleConfig cron schedule definitions.
type ScheduleConfig struct {
	SigninCron     string `json:"signin_cron"`
	TaskCron       string `json:"task_cron"`
	TokenKeepalive string `json:"token_keepalive_cron"`
}

// RouterConfig routing and responses API feature flag.
type RouterConfig struct {
	ModelMap  map[string]string `json:"model_map"`
	Responses struct {
		Enabled bool `json:"enabled"`
	} `json:"responses_api"`
}

// HotReloadable wraps Config to support live reconfiguration without restart.
type HotReloadable struct {
	mu     sync.RWMutex
	Config Config
	path   string
}

var Shared = &HotReloadable{}

// defaultThrottle returns the conservative per-source frequency budget baseline
// (PRD v0.2 §7.1 + PM 调研 §4 + UI §5.20 defaults):
// WorkBuddy 6/20 rpm · 日600 · 0.1–0.9s · 在途2; Qoder 4/12 · 日300 · 0.2–1.2s · 在途1 · 429后当日减半;
// Trae 2/6 · 日100 · 0.5–2.0s · 在途1. Jitter ±15min. Values are product
// defaults marked "可调整" in the UI, not external facts.
func defaultThrottle(source string) ThrottleConfig {
	t := ThrottleConfig{
		SteadyRPM:     6,
		BurstRPM:      20,
		MinGapMS:      [2]int{100, 900},
		DailyCap:      600,
		Inflight:      2,
		JitterMinutes: 15,
		Backoff: BackoffConfig{
			Base429Sec:      600,
			NotFoundSec:     300,
			Exhaust402Until: "next-04:00",
		},
	}
	switch source {
	case "qoder":
		t.SteadyRPM, t.BurstRPM = 4, 12
		t.MinGapMS = [2]int{200, 1200}
		t.DailyCap, t.Inflight = 300, 1
		t.HalveBudgetAfter429 = true
	case "trae":
		t.SteadyRPM, t.BurstRPM = 2, 6
		t.MinGapMS = [2]int{500, 2000}
		t.DailyCap, t.Inflight = 100, 1
	}
	return t
}

func defaults() Config {
	return Config{
		Listen:        ":7863",
		AuthDir:       "./auths",
		StateFile:     "./data/state.json",
		NoticeVersion: "1",
		ModelMap:      make(map[string]string),
		Sources: map[string]SourceConfig{
			"workbuddy": {Enabled: true, Throttle: defaultThrottle("workbuddy")},
			"qoder":     {Enabled: true, Throttle: defaultThrottle("qoder")},
			// Trae stays disabled by default: platform rules are a black box (调研 §, C 级证据), most-conservative stance.
			"trae": {Enabled: false, Throttle: defaultThrottle("trae")},
		},
		Schedule: ScheduleConfig{
			SigninCron:     "0 9,21 * * *",
			TaskCron:       "0 10 * * *",
			TokenKeepalive: "0 22 * * *", // #nosec G101 -- 值为 cron 表达式，键名属契约冻结面不可改名
		},
		Pool: PoolConfig{
			CreditFloor: 100,
			MaxRetries:  3,
			CooldownSec: 60,
		},
	}
}

func Load(path string) error {
	cfg := defaults()

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}

	Shared.mu.Lock()
	Shared.Config = cfg
	Shared.path = path
	Shared.mu.Unlock()

	return nil
}

// Reload re-reads the config file and replaces in-place. No restart needed.
func (h *HotReloadable) Reload() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	cfg := defaults()
	data, err := os.ReadFile(h.path)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}

	h.Config = cfg
	return nil
}

// Get returns a snapshot of the current config (safe for concurrent read).
func (h *HotReloadable) Get() Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.Config
}
