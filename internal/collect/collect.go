// Package collect wires the per-provider collectors together.
//
// Each collector gathers snapshots for one upstream family. A single failing
// credential never fails the batch: its failure lives in that snapshot's
// OK/Err fields. A non-nil error from Collect means the collector could not run
// at all (for example the credential list itself was unreachable, or the shared
// context was cancelled).
package collect

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/cpa"
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/ollama"
)

// ErrControlPlane marks a collector failure that is a problem with CPA itself
// (management API unreachable, bad management key) rather than with any one
// credential. The app inspects it with errors.Is to raise a provider-level
// fault, so that existing credentials can still be aged toward stale instead of
// silently vanishing from the report.
var ErrControlPlane = errors.New("collect: CPA control plane unavailable")

// CPA concurrency is capped because each quota fetch triggers upstream work on
// the CPA instance; Ollama is capped lower because it is a third-party website
// that reacts badly to bursts.
const (
	cpaConcurrency    = 4
	ollamaConcurrency = 2
)

// CPACollector collects addressable credentials from either management transport.
type CPACollector struct {
	client         *cpa.Client
	maxConcurrency int
}

// NewCPACollector builds a collector over an existing client.
func NewCPACollector(client *cpa.Client) *CPACollector {
	return &CPACollector{client: client, maxConcurrency: cpaConcurrency}
}

// Name implements domain.Collector.
func (c *CPACollector) Name() string { return "cpa" }

// Collect lists credentials and fetches quota for each addressable one.
//
// Credentials without an auth_index cannot be queried and are skipped; they
// still appear in other surfaces via ListCredentials, but produce no snapshot.
//
// When ListCredentials itself fails there is no credential list to iterate, so
// the returned error wraps ErrControlPlane. The app uses that to raise a
// provider-level fault and keep existing credentials ageing toward stale,
// instead of receiving "no snapshots" and treating the provider as healthy.
func (c *CPACollector) Collect(ctx context.Context) ([]domain.QuotaSnapshot, error) {
	creds, err := c.client.ListCredentials(ctx)
	if err != nil {
		// Every failure here is control-plane: we could not even read the
		// credential list. Preserve the original error (including a deadline)
		// for errors.Is while adding the sentinel.
		return []domain.QuotaSnapshot{}, fmt.Errorf("%w: list credentials: %w", ErrControlPlane, err)
	}
	targets := make([]domain.Credential, 0, len(creds))
	for _, cred := range creds {
		if cred.AuthIndex == "" || cred.Disabled {
			continue
		}
		targets = append(targets, cred)
	}
	return c.run(ctx, targets, func(ctx context.Context, cred domain.Credential) (domain.QuotaSnapshot, error) {
		return c.client.FetchQuota(ctx, cred)
	})
}

// run fans items out over a bounded worker pool and preserves input order.
func (c *CPACollector) run(
	ctx context.Context,
	creds []domain.Credential,
	fetch func(context.Context, domain.Credential) (domain.QuotaSnapshot, error),
) ([]domain.QuotaSnapshot, error) {
	limit := c.maxConcurrency
	if limit < 1 {
		limit = 1
	}
	snapshots := make([]domain.QuotaSnapshot, len(creds))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i, cred := range creds {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("collect cpa: %w", err)
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, cred domain.Credential) {
			defer wg.Done()
			defer func() { <-sem }()
			snap, err := fetch(ctx, cred)
			if err != nil {
				// An expired shared context aborts the whole batch so callers
				// can detect the timeout; other failures stay per-credential.
				if ctxErr := ctx.Err(); ctxErr != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("collect cpa: %w", ctxErr)
					}
					mu.Unlock()
					return
				}
				snap = failedSnapshot(cred, c.client.Source(), err)
			}
			snapshots[i] = snap
		}(i, cred)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return snapshots, nil
}

// ollamaFetcher is the slice of *ollama.Client the collector needs. Having it
// as an interface keeps the collector unit-testable without reaching the real
// website.
type ollamaFetcher interface {
	Fetch(ctx context.Context, account config.OllamaAccount) (domain.QuotaSnapshot, error)
}

// OllamaCollector collects quota for the configured Ollama Cloud accounts.
type OllamaCollector struct {
	client         ollamaFetcher
	accounts       []config.OllamaAccount
	maxConcurrency int
}

// NewOllamaCollector builds a collector for the given accounts.
func NewOllamaCollector(client *ollama.Client, accounts []config.OllamaAccount) *OllamaCollector {
	return &OllamaCollector{client: client, accounts: accounts, maxConcurrency: ollamaConcurrency}
}

// Name implements domain.Collector.
func (c *OllamaCollector) Name() string { return "ollama" }

// Collect scrapes each configured account concurrently, bounded by
// ollamaConcurrency.
func (c *OllamaCollector) Collect(ctx context.Context) ([]domain.QuotaSnapshot, error) {
	limit := c.maxConcurrency
	if limit < 1 {
		limit = 1
	}
	snapshots := make([]domain.QuotaSnapshot, len(c.accounts))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i, account := range c.accounts {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("collect ollama: %w", err)
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, account config.OllamaAccount) {
			defer wg.Done()
			defer func() { <-sem }()
			snap, err := c.client.Fetch(ctx, account)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("collect ollama: %w", ctxErr)
					}
					mu.Unlock()
					return
				}
				snap = failedOllamaSnapshot(account, err)
			}
			snapshots[i] = snap
		}(i, account)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return snapshots, nil
}

// failedSnapshot builds a per-credential failure snapshot for an error that
// escaped FetchQuota. FetchQuota itself only returns an error for transport
// problems, so the classification is transport by construction.
func failedSnapshot(cred domain.Credential, source domain.Source, err error) domain.QuotaSnapshot {
	msg := "采集失败"
	if err != nil {
		msg = err.Error()
	}
	return domain.QuotaSnapshot{
		Credential: cred,
		Source:     source,
		Confidence: domain.ConfidenceUnknown,
		FetchedAt:  time.Now(),
		OK:         false,
		Failure:    domain.FailureTransport,
		Err:        msg,
	}
}

func failedOllamaSnapshot(account config.OllamaAccount, err error) domain.QuotaSnapshot {
	msg := "采集失败"
	if err != nil {
		msg = err.Error()
	}
	// Build the credential directly (and never from the cookie) so no secret
	// can leak through this path.
	return domain.QuotaSnapshot{
		Credential: domain.Credential{
			Key:      "ollama/" + account.Name,
			Provider: domain.ProviderOllama,
			Alias:    account.Name,
			ShortID:  "****",
		},
		Source:     domain.SourceOllamaWeb,
		Confidence: domain.ConfidenceUnknown,
		FetchedAt:  time.Now(),
		OK:         false,
		Failure:    domain.FailureTransport,
		Err:        msg,
	}
}

// All assembles the collectors enabled by configuration.
//
// The Ollama collector is omitted entirely when no accounts are configured, so
// nothing ever touches the Ollama website without an explicit opt-in. The CPA
// channel likewise switches to the OMC source (with a direct-CPA fallback) when
// QUOTA_SOURCE=omc; see Build.
func All(cfg config.Config) []domain.Collector {
	return AllWithCPA(cfg, NewCPAClient(cfg))
}

// NewCPAClient centralizes compatibility options for every composition root.
func NewCPAClient(cfg config.Config) *cpa.Client {
	return cpa.NewWithOptions(cfg.CPABaseURL, cfg.CPAManagementKey, cfg.CPATimeout, cpa.Options{
		APIVersion:         cfg.CPAAPIVersion,
		QuotaStrategy:      cfg.CPAQuotaStrategy,
		AntigravityProfile: cfg.AntigravityQuotaProfile,
		ContextOverrides:   cfg.CPAContextOverrides,
	})
}

// AllWithCPA lets the watcher and collector share negotiation and HTTP state.
func AllWithCPA(cfg config.Config, client *cpa.Client) []domain.Collector {
	collectors, _ := Build(cfg, client, nil)
	return collectors
}
