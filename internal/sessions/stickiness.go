// Package sessions implements conversation-level session stickiness adapted from Panel design.
// Key feature: same conversation_id binds to same account, TTL rolling expiry,
// automatic unbind on failure, optional Redis mirroring for persistence across restarts.
package sessions

import (
	"all2api/internal/pool"
	"sync"
	"time"
)

// Binding maps a conversation to its assigned account within a source.
type Binding struct {
	Platform   string    `json:"platform"`
	Conversation string `json:"conversation"` // conversation_id from client
	AccountID  string    `json:"account_id"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// SessionStickiness manages conversation-to-account bindings per platform.
type SessionStickiness struct {
	mu          sync.RWMutex
	bindings    map[string]map[string]*Binding // platform → conversation → binding
	ttl         time.Duration                  // default TTL for binding
	pool        *pool.WeightedRandomSelector   // reference to account pool
	persistPath string                         // local state file path
	redisURL    string                         // optional Upstash Redis for async mirroring
	lastSync    time.Time
}

// NewSessionStickiness creates a new session stickiness manager.
func NewSessionStickiness(pool *pool.WeightedRandomSelector, ttl time.Duration, persistPath, redisURL string) *SessionStickiness {
	return &SessionStickiness{
		bindings:    make(map[string]map[string]*Binding),
		ttl:         ttl,
		pool:        pool,
		persistPath: persistPath,
		redisURL:    redisURL,
	}
}

// GetBinding returns the current binding for a conversation within a platform.
// Returns nil if no valid binding exists (expired or failed).
func (s *SessionStickiness) GetBinding(platform, conversation string) *Binding {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if platformMap, ok := s.bindings[platform]; ok {
		binding := platformMap[conversation]
		if binding != nil && !binding.ExpiresAt.Before(time.Now()) {
			return binding
		}
	}
	return nil
}

// Bind creates a new binding for a conversation within a platform.
func (s *SessionStickiness) Bind(platform, conversation, accountID string) *Binding {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.bindings[platform] == nil {
		s.bindings[platform] = make(map[string]*Binding)
	}

	binding := &Binding{
		Platform:     platform,
		Conversation: conversation,
		AccountID:    accountID,
		CreatedAt:    time.Now(),
		ExpiresAt:    time.Now().Add(s.ttl),
	}

	s.bindings[platform][conversation] = binding
	s.scheduleSync()
	return binding
}

// Unbind removes a binding (called when the bound account fails or goes into cooldown).
func (s *SessionStickiness) Unbind(platform, conversation string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if platformMap, ok := s.bindings[platform]; ok {
		delete(platformMap, conversation)
	}
	s.scheduleSync()
}

// Refresh extends the TTL for a binding on successful request completion.
func (s *SessionStickiness) Refresh(platform, conversation string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if platformMap, ok := s.bindings[platform]; ok {
		binding := platformMap[conversation]
		if binding != nil {
			binding.ExpiresAt = time.Now().Add(s.ttl)
			s.scheduleSync()
			return true
		}
	}
	return false
}

// ListBindingsForSource returns all active bindings for a platform.
func (s *SessionStickiness) ListBindingsForSource(platform string) []*Binding {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*Binding, 0)
	now := time.Now()
	if platformMap, ok := s.bindings[platform]; ok {
		for _, binding := range platformMap {
			if !binding.ExpiresAt.Before(now) {
				result = append(result, binding)
			}
		}
	}
	return result
}

// Persist saves the current bindings state to disk (atomic write).
func (s *SessionStickiness) Persist() error {
	// TODO: atomic JSON write to s.persistPath
	return nil
}

// Load restores bindings from the state file.
func (s *SessionStickiness) Load() error {
	// TODO: read and parse state file
	// If Redis URL is configured, also attempt async mirror load
	return nil
}

// SyncToRedis pushes the current state to Upstash Redis (async background goroutine).
func (s *SessionStickiness) SyncToRedis() error {
	if s.redisURL == "" {
		return nil
	}
	// TODO: async HTTP PUT to Redis key
	// Key format: "all2api:sessions:<platform>:<hash_of_bindings>"
	// TTL: expires with last binding + buffer
	return nil
}

// ClearExpired removes all expired bindings.
func (s *SessionStickiness) ClearExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for platform, platformMap := range s.bindings {
		for conv, binding := range platformMap {
			if binding.ExpiresAt.Before(now) {
				delete(platformMap, conv)
			}
		}
	}
}

// scheduleSync schedules a background sync if not already pending.
func (s *SessionStickiness) scheduleSync() {
	// Debounce sync by checking lastSync
	elapsed := time.Since(s.lastSync)
	if elapsed < 5*time.Second {
		return
	}
	
	go func() {
		time.Sleep(2 * time.Second) // debounce
		s.Persist()
		if s.redisURL != "" {
			s.SyncToRedis()
		}
		s.mu.Lock()
		s.lastSync = time.Now()
		s.mu.Unlock()
	}()
}
