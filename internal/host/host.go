// Package host wires the runtime control plane: per-source circuit breakers
// and the account pool. Breaker parameters are derived from the frozen
// throttle surface (sources.<id>.throttle.backoff.*) so flow-control
// configuration has exactly one landing spot — config.json.
package host

import (
	"sync"

	"all2api/internal/circuit"
	"all2api/internal/config"
	"all2api/internal/pool"
)

// Host owns the per-source breakers and the shared account pool.
type Host struct {
	Pool     *pool.WeightedRandomSelector
	mu       sync.RWMutex
	breakers map[string]*circuit.Breaker
}

// New builds the host from the loaded config: one breaker per configured
// source plus the weighted-random account pool from pool.* settings.
func New(cfg config.Config) *Host {
	h := &Host{
		Pool: pool.NewWeightedRandomSelector(
			int64(cfg.Pool.CreditFloor), cfg.Pool.MaxRetries, cfg.Pool.CooldownSec),
		breakers: make(map[string]*circuit.Breaker, len(cfg.Sources)),
	}
	for id, src := range cfg.Sources {
		h.breakers[id] = circuit.NewBreaker(id, breakerConfig(src))
	}
	return h
}

// breakerConfig maps the frozen per-source backoff keys onto circuit.Config.
// base_429_sec → InitialBackoffSec (PRD §7.1 floor 600), not_found_sec →
// CooldownSec (the 404 short-cooldown path). Non-positive values keep the
// circuit.DefaultConfig baseline.
func breakerConfig(src config.SourceConfig) circuit.Config {
	c := circuit.DefaultConfig()
	if src.Throttle.Backoff.Base429Sec > 0 {
		c.InitialBackoffSec = src.Throttle.Backoff.Base429Sec
	}
	if src.Throttle.Backoff.NotFoundSec > 0 {
		c.CooldownSec = src.Throttle.Backoff.NotFoundSec
	}
	return c
}

// Breaker returns the circuit breaker for a configured source.
func (h *Host) Breaker(source string) (*circuit.Breaker, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	b, ok := h.breakers[source]
	return b, ok
}
