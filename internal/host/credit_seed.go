//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// credit_seed.go closes the credit loop at startup (M3b): auth files carry no
// balance value, so accounts register with Credit=0 and the default
// credit_floor=100 would filter every one of them out of request-path
// selection. SeedCredits queries each account's real balance once
// (POST {billing}/v2/billing/meter/get-user-resource, protocol report §2) and
// writes the absolute value into the pool.
//
// Best-effort by contract: a failed account is one log line and stays at 0
// (blocked below the floor) — no invented balances, never fatal to startup.
// It runs synchronously before serving: the pool is unusable until seeded
// anyway, and a down billing endpoint answers transport errors in milliseconds
// rather than hanging (the task client carries a bounded transport timeout).
package host

import (
	"context"
	"fmt"
	"sync"

	"all2api/internal/platform/workbuddy"
	"all2api/internal/pool"
)

// seedConcurrency bounds the parallel balance queries. Four at a time keeps
// the fan-out polite against the upstream's serial per-account engine.
const seedConcurrency = 4

// SeedCredits seeds every stored account's pool Credit from the billing
// balance endpoint. Returns (seeded, failed).
func (s *CredentialStore) SeedCredits() (int, int) {
	return s.seedCreditsWith(workbuddy.NewTaskClient(workbuddy.TaskClientOptions{
		Credentials: s,
		Refresher:   s,
	}))
}

// seedCreditsWith is the options-carrying form tests use to point the task
// client at an httptest stub. One worker per account, seedConcurrency at a
// time; counters synchronize under mu.
func (s *CredentialStore) seedCreditsWith(c *workbuddy.TaskClient) (int, int) {
	accts := s.accountIdentities()
	if len(accts) == 0 {
		return 0, 0
	}

	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		sem    = make(chan struct{}, seedConcurrency)
		seeded int
		failed int
	)
	ctx := context.Background()

	for _, a := range accts {
		wg.Add(1)
		sem <- struct{}{}
		go func(platform, id string) {
			defer wg.Done()
			defer func() { <-sem }()

			cred, err := s.CredentialFor(ctx, &pool.Account{ID: id, Platform: platform})
			if err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				fmt.Printf("credit seed: %s/%s: %v\n", platform, id, err)
				return
			}
			credits, err := c.GetUserResource(ctx, cred)
			if err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				fmt.Printf("credit seed: %s/%s: %v\n", platform, id, err)
				return
			}
			if s.pool == nil || !s.pool.SetAccountCredit(platform, id, credits) {
				mu.Lock()
				failed++
				mu.Unlock()
				fmt.Printf("credit seed: %s/%s not in pool (balance %d discarded)\n", platform, id, credits)
				return
			}
			mu.Lock()
			seeded++
			mu.Unlock()
			fmt.Printf("credit seed: %s/%s = %d credits\n", platform, id, credits)
		}(a.Platform, a.ID)
	}
	wg.Wait()
	return seeded, failed
}
