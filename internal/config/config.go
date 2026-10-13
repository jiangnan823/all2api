package config

import (
	"encoding/json"
	"os"
	"sync"
)

// Config is the root configuration loaded from config.json.
type Config struct {
	Listen      string                 `json:"listen"`
	APIKey      string                 `json:"api_key"`
	AuthDir     string                 `json:"auth_dir"`
	StateFile   string                 `json:"state_file"`
	Pool        PoolConfig             `json:"pool"`
	Schedule    ScheduleConfig         `json:"schedule"`
	Throttle    map[string]ThrottleConfig `json:"throttle"`
	Sources     map[string]SourceConfig `json:"sources"`
	Router      RouterConfig           `json:"router"`
	ModelMap    map[string]string       `json:"model_map"` // model name → platform/model
}

// PoolConfig pool-level settings adapted from Panel's design.
type PoolConfig struct {
	CreditFloor int   `json:"credit_floor"` // skip accounts below this credit
	MaxRetries  int   `json:"max_retries"`
	CooldownSec int64 `json:"cooldown_sec"`
}

// ThrottleConfig per-source rate limiting (first-class config, not hardcoded).
type ThrottleConfig struct {
	MaxRPS     float64 `json:"max_rps"`
	BurstSize  int     `json:"burst_size"`
}

// SourceConfig per-source control including disable switch.
type SourceConfig struct {
	Enabled       bool            `json:"enabled"`
	Throttle      ThrottleConfig  `json:"throttle"`
	CredentialsDir string         `json:"credentials_dir"`
}

// ScheduleConfig cron schedule definitions.
type ScheduleConfig struct {
	SigninCron    string `json:"signin_cron"`
	TaskCron      string `json:"task_cron"`
	TokenKeepalive string `json:"token_keepalive_cron"`
}

// RouterConfig routing and responses API feature flag.
type RouterConfig struct {
	ModelMap map[string]string `json:"model_map"`
	Responses struct {
		Enabled  bool `json:"enabled"`
	} `json:"responses_api"`
}

// HotReloadable wraps Config to support live reconfiguration without restart.
type HotReloadable struct {
	mu     sync.RWMutex
	Config Config
	path   string
}

var Shared = &HotReloadable{}

func Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	cfg := Config{
		Listen:      ":7863",
		AuthDir:     "./auths",
		StateFile:   "./data/state.json",
		ModelMap:    make(map[string]string),
		Throttle:    make(map[string]ThrottleConfig),
		Sources:     make(map[string]SourceConfig),
		Schedule: ScheduleConfig{
			SigninCron:    "0 9,21 * * *",
			TaskCron:      "0 10 * * *",
			TokenKeepalive: "0 22 * * *",
		},
		Pool: PoolConfig{
			CreditFloor: 100,
			MaxRetries:  3,
			CooldownSec: 60,
		},
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

	data, err := os.ReadFile(h.path)
	if err != nil {
		return err
	}

	var cfg Config
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
