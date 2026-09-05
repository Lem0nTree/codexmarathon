package app

import (
	"context"
	"errors"

	"codexmarathon/controller/internal/automation"
	"codexmarathon/controller/internal/policy"
	"codexmarathon/controller/internal/quota"
	"codexmarathon/controller/internal/telemetry"
)

// RegisterUsageProvider attaches the quota source for one configured account.
// Runtime-backed accounts can use the snapshot provider installed by the
// runtime event path; inactive profiles should register a quota.Provider that
// reads its opaque credential snapshot from the controller vault.
func (c *Controller) RegisterUsageProvider(accountID string, provider telemetry.UsageProvider) error {
	if c == nil || c.router == nil {
		return errors.New("usage router is unavailable")
	}
	return c.router.Register(accountID, provider)
}

// NewAutomationLoop composes the event-driven quota policy with the account
// registry and transition coordinator. The returned loop is not started; the
// caller owns its event channel and goroutine lifecycle.
func (c *Controller) NewAutomationLoop() (*automation.Loop, error) {
	return c.NewAutomationLoopWithTransition(nil)
}

// NewAutomationLoopWithTransition composes the event-driven quota policy with
// the account registry and a caller-supplied transition boundary.  The
// installed-Codex companion uses this seam because its transition is owned by
// the installed app-server/process supervisor rather than the embedded
// Marathon runtime coordinator.  A nil callback preserves the normal
// embedded-runtime behavior.
func (c *Controller) NewAutomationLoopWithTransition(transition automation.TransitionFunc) (*automation.Loop, error) {
	if c == nil {
		return nil, errors.New("controller is nil")
	}
	if c.registry == nil || c.router == nil || c.policy == nil {
		return nil, errors.New("controller automation dependencies are unavailable")
	}
	if err := c.ensureInactiveQuotaProviders(); err != nil {
		return nil, err
	}
	if transition == nil {
		transition = func(ctx context.Context, accountID string) error {
			_, err := c.RequestTransition(ctx, accountID)
			return err
		}
	}
	return automation.New(automation.Config{
		Router:       c.router,
		Policy:       c.policy,
		FreshnessTTL: c.config.TelemetryTTL,
		Thresholds:   policy.DefaultThresholds(),
		Accounts: automation.AccountSource{
			IDs: func() ([]string, error) {
				registered, err := c.registry.List()
				if err != nil {
					return nil, err
				}
				ids := make([]string, 0, len(registered))
				for _, account := range registered {
					ids = append(ids, account.ID)
				}
				return automation.AccountIDs(ids), nil
			},
			Active: func() (string, error) {
				return c.registry.ActiveID()
			},
		},
		Transition: transition,
		OnError:    c.reportEventError,
	}), nil
}

// ensureInactiveQuotaProviders installs the embedded donor-compatible quota
// provider for accounts that do not already have an authoritative runtime
// provider. Existing runtime snapshot providers are deliberately retained.
func (c *Controller) ensureInactiveQuotaProviders() error {
	registered, err := c.registry.List()
	if err != nil {
		return err
	}
	known := make(map[string]struct{}, len(c.router.Accounts()))
	for _, accountID := range c.router.Accounts() {
		known[accountID] = struct{}{}
	}
	provider := quota.Provider{
		Source: quota.VaultCredentialSource{Vault: c.vault},
		Client: quota.Client{},
		Model:  quota.DefaultModel,
	}
	for _, account := range registered {
		if _, exists := known[account.ID]; exists {
			continue
		}
		if err := c.router.Register(account.ID, provider); err != nil {
			return err
		}
	}
	return nil
}
