// Package policy makes account-selection decisions from normalized telemetry.
// Observation, scheduling, and runtime transitions remain separate: policy
// may request a transition or ask reset.Scheduler to wait, but it never
// deploys credentials or reloads the runtime.
package policy

import (
	"sort"
	"strings"
	"time"

	"codexmarathon/controller/internal/reset"
	"codexmarathon/controller/internal/telemetry"
)

// PolicyDecisionType is intentionally small and stable for the controller's
// transition loop.
type PolicyDecisionType uint8

const (
	PolicyStay PolicyDecisionType = iota
	PolicyTransition
	PolicyWaitForReset
	PolicyNoTelemetry
)

// Compatibility aliases with the prose architecture names.
const (
	Stay             = PolicyStay
	Transition      = PolicyTransition
	WaitForReset    = PolicyWaitForReset
	NoTelemetry     = PolicyNoTelemetry
	NoEligibleAccount = PolicyNoTelemetry
)

func (t PolicyDecisionType) String() string {
	switch t {
	case PolicyStay:
		return "stay"
	case PolicyTransition:
		return "transition"
	case PolicyWaitForReset:
		return "wait_for_reset"
	case PolicyNoTelemetry:
		return "no_telemetry"
	default:
		return "unknown"
	}
}

// PolicyDecision is the result of evaluating all eligible configured
// accounts.  Reset fields are copied out of reset.ResetDecision so callers do
// not need to retain mutable scheduler state.
type PolicyDecision struct {
	Type               PolicyDecisionType
	AccountID          string
	Reason             string
	CandidateAccountID string
	LimitID            string
	DurationMins       int
	ResetAt            time.Time
	RefetchRequired    bool
	RefreshAccountIDs  []string
}

func (d PolicyDecision) IsTransition() bool { return d.Type == PolicyTransition }

func (d PolicyDecision) IsWaiting() bool {
	return d.Type == PolicyWaitForReset || d.Type == PolicyNoTelemetry
}

// Engine evaluates telemetry and delegates pool-exhaustion scheduling to one
// reset scheduler.  It is safe to reuse across policy ticks.
type Engine struct {
	scheduler reset.ResetScheduler
	now       func() time.Time
}

// NewEngine constructs an engine.  If scheduler is nil, a private scheduler
// is created.  The optional clock is used when Evaluate receives a zero time.
func NewEngine(scheduler reset.ResetScheduler, clock ...func() time.Time) *Engine {
	if scheduler == nil {
		scheduler = reset.NewScheduler(clock...)
	}
	return &Engine{scheduler: scheduler, now: selectClock(clock...)}
}

// NewPolicyEngine is an alias for NewEngine.
func NewPolicyEngine(scheduler reset.ResetScheduler, clock ...func() time.Time) *Engine {
	return NewEngine(scheduler, clock...)
}

// Evaluate chooses the first deterministic account with a complete fresh
// snapshot and no exhausted window.  A reset timestamp is a scheduling hint,
// never a reason to mark an account eligible without fresh data.
func (e *Engine) Evaluate(accounts []telemetry.AccountTelemetry, now time.Time) PolicyDecision {
	if e == nil {
		return PolicyDecision{Type: PolicyNoTelemetry, Reason: "policy engine is nil"}
	}
	if now.IsZero() {
		now = e.currentTime()
	}
	if e.scheduler == nil {
		e.scheduler = reset.NewScheduler(e.now)
	}
	ordered := make([]telemetry.AccountTelemetry, len(accounts))
	copy(ordered, accounts)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].AccountID < ordered[j].AccountID })

	for _, account := range ordered {
		if account.HasFreshCapacity(now) {
			e.scheduler.Clear()
			return PolicyDecision{
				Type:      PolicyTransition,
				AccountID: account.AccountID,
				Reason:    "account has fresh capacity",
			}
		}
	}

	decision := e.scheduler.Evaluate(ordered, now)
	if decision.State == reset.ResetWaitForReset {
		return PolicyDecision{
			Type:               PolicyWaitForReset,
			CandidateAccountID: decision.CandidateAccountID,
			LimitID:            decision.LimitID,
			DurationMins:       decision.DurationMins,
			ResetAt:            decision.ResetAt,
			RefetchRequired:    decision.RefetchRequired,
			RefreshAccountIDs:  append([]string(nil), decision.RefreshAccountIDs...),
			Reason:             decision.Reason,
		}
	}
	return PolicyDecision{
		Type:              PolicyNoTelemetry,
		RefetchRequired:   decision.RefetchRequired,
		RefreshAccountIDs: append([]string(nil), decision.RefreshAccountIDs...),
		Reason:            decision.Reason,
	}
}

// Evaluate is a stateless convenience for one policy tick.
func Evaluate(accounts []telemetry.AccountTelemetry, now time.Time) PolicyDecision {
	return NewEngine(nil).Evaluate(accounts, now)
}

// Eligible reports whether a single account can be used at now.
func Eligible(account telemetry.AccountTelemetry, now time.Time) bool {
	return account.HasFreshCapacity(now)
}

// NormalizeReason trims caller-provided policy reasons for stable logs and
// journal entries.  It is intentionally not used to make authorization
// decisions.
func NormalizeReason(reason string) string { return strings.TrimSpace(reason) }

func selectClock(clocks ...func() time.Time) func() time.Time {
	if len(clocks) > 0 && clocks[0] != nil {
		return clocks[0]
	}
	return func() time.Time { return time.Now().UTC() }
}

func (e *Engine) currentTime() time.Time {
	if e == nil || e.now == nil {
		return time.Now().UTC()
	}
	return e.now()
}
