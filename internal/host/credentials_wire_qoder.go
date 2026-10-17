//go:build qoder || full || (!qoder && !trae && !workbuddy)

// credentials_wire_qoder.go bridges the host CredentialStore onto the qoder
// sender: the store supplies the static PAT per pool account (qoder's
// CredentialSource — there is no refresh sink, a PAT has no refresh
// semantics), WireQoderSender injects it into the registered adapter, and
// SeedQoderCredits closes the credit loop from the authoritative OpenAPI
// quota endpoint (a PAT file carries no balance; without seeding the default
// credit_floor would filter every qoder account out of request-path
// selection — the M3b workbuddy pattern, mirrored).
//
// Compiled under the same build tags as the qoder platform; workbuddy-only
// and trae-only builds get the no-op stub in credentials_wire_qoder_stub.go.
package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"all2api/internal/adapter"
	"all2api/internal/observability"
	"all2api/internal/platform/qoder"
	"all2api/internal/pool"
)

// qoderCredSource adapts the store onto qoder.CredentialSource. It cannot be
// the store itself: the workbuddy bridge already occupies CredentialFor with
// the workbuddy.Credential return type.
type qoderCredSource struct {
	store *CredentialStore
}

// Compile-time proof the adapter satisfies the sender seam.
var _ qoder.CredentialSource = qoderCredSource{}

// CredentialFor implements qoder.CredentialSource: value-snapshot lookup by
// platform/accountID. An unknown account is an explicit error — never a
// fabricated credential.
func (q qoderCredSource) CredentialFor(_ context.Context, acct *pool.Account) (*qoder.PATCredential, error) {
	if acct == nil {
		return nil, errors.New("credential store: no account given")
	}
	cred, ok := q.store.lookup(credKey(acct.Platform, acct.ID))
	if !ok {
		return nil, fmt.Errorf("credential store: no credential for %s/%s", acct.Platform, acct.ID)
	}
	out := qoder.PATCredential{PAT: cred.AccessToken}
	if strings.HasPrefix(cred.AccessToken, "dt-") {
		// Device-login credential: the dt- token rides DeviceToken and the
		// PAT stays empty — the exchange branches on it (chat-capable path).
		out.DeviceToken = cred.AccessToken
		out.ClientID = cred.ClientID
		out.PAT = ""
	}
	return &out, nil
}

// WireQoderSender injects the store into the registered qoder adapter as the
// PAT credential source. Production base URLs stay region defaults
// (zero-value ClientOptions is production-shaped). Returns 1 when the adapter
// was found and configured, 0 otherwise.
func (s *CredentialStore) WireQoderSender() int {
	a := adapter.Default.Get(QoderPlatform)
	if a == nil {
		return 0
	}
	type senderConfigurator interface {
		Configure(qoder.ClientOptions)
	}
	cfg, ok := a.(senderConfigurator)
	if !ok {
		return 0
	}
	cfg.Configure(qoder.ClientOptions{
		Credentials: qoderCredSource{store: s},
	})
	return 1
}

// qoderQuotaProber is the slice of the qoder sender the credit seeding drives.
type qoderQuotaProber interface {
	QuotaRemaining(ctx context.Context, acct *pool.Account) (int64, error)
}

// seedConcurrency bounds the parallel quota probes (same discipline as the
// workbuddy credit seeding: four at a time keeps the fan-out polite).
const qoderSeedConcurrency = 4

// SeedQoderCredits seeds every stored qoder account's pool Credit from the
// OpenAPI quota usage endpoint, one probe per account (4 in parallel,
// best-effort — a failure is one log line and the account stays at 0). No-op
// when the qoder adapter or its sender is not part of this build.
func (s *CredentialStore) SeedQoderCredits() (int, int) {
	a := adapter.Default.Get(QoderPlatform)
	if a == nil {
		return 0, 0
	}
	probe, ok := a.(qoderQuotaProber)
	if !ok {
		return 0, 0
	}
	return s.seedQoderCreditsWith(probe)
}

// seedQoderCreditsWith is the probe-carrying form tests use. Mirrors the
// workbuddy seedConcurrency discipline.
func (s *CredentialStore) seedQoderCreditsWith(probe qoderQuotaProber) (int, int) {
	accts := s.identitiesByPlatform(QoderPlatform)
	if len(accts) == 0 {
		return 0, 0
	}

	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		sem    = make(chan struct{}, qoderSeedConcurrency)
		seeded int
		failed int
	)
	ctx := context.Background()

	for _, a := range accts {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()

			credits, err := probe.QuotaRemaining(ctx, &pool.Account{ID: id, Platform: QoderPlatform})
			if err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				fmt.Printf("credit seed: %s/%s: %v\n", QoderPlatform, id, err)
				observability.Error("credit seed failed", "platform", QoderPlatform, "account", id, "error", err.Error())
				return
			}
			if s.pool == nil || !s.pool.SetAccountCredit(QoderPlatform, id, credits) {
				mu.Lock()
				failed++
				mu.Unlock()
				fmt.Printf("credit seed: %s/%s not in pool (balance %d discarded)\n", QoderPlatform, id, credits)
				observability.Error("credit seed account not in pool", "platform", QoderPlatform, "account", id, "balance", credits)
				return
			}
			mu.Lock()
			seeded++
			mu.Unlock()
			fmt.Printf("credit seed: %s/%s = %d credits\n", QoderPlatform, id, credits)
			observability.Info("credit seed", "platform", QoderPlatform, "account", id, "credit", credits)
		}(a.ID)
	}
	wg.Wait()
	return seeded, failed
}
