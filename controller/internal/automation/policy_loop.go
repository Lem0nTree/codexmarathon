// Package automation connects runtime quota events to the controller's
// observation and policy domains. It intentionally stops at the transition
// callback: the transition coordinator/runtime owns safe-boundary auth
// mechanics, while this loop owns trigger deduplication, provider refresh,
// reset waiting, and bounded wake-ups.
package automation

import (
	"context"
	"errors"
	"sort"
	"time"

	"codexmarathon/controller/internal/policy"
	"codexmarathon/controller/internal/telemetry"
)

var (
	ErrMissingAccountSource = errors.New("automation account source is unavailable")
	ErrMissingPolicy        = errors.New("automation policy engine is unavailable")
	ErrMissingRouter        = errors.New("automation usage router is unavailable")
)

// EventType identifies runtime or controller events that can wake policy.
type EventType string

const (
	EventThresholdReached   EventType = "threshold_reached"
	EventUsageLimitExceeded EventType = "usage_limit_exceeded"
	EventResetDue           EventType = "reset_due"
	EventTelemetryChanged   EventType = "telemetry_changed"
)

// Event is intentionally secret-free. EventID should be the runtime's stable
// notification ID when available; duplicate IDs are suppressed by policy.
type Event struct {
	ID         string
	Type       EventType
	AccountID  string
	OccurredAt time.Time
}

// AccountSource supplies stable configured IDs and the current runtime
// account. Registry implementations can adapt to these two callbacks without
// exposing credential material to the automation package.
type AccountSource struct {
	IDs    func() ([]string, error)
	Active func() (string, error)
}

// TransitionFunc is called only after policy returns a verified candidate.
// The callback should synchronously run the transition coordinator and return
// an error if safe-boundary/auth/identity verification did not commit.
type TransitionFunc func(context.Context, string) error

// Config controls one event-driven policy loop. A zero poll interval uses a
// conservative bounded wake-up; server reset timestamps remain the preferred
// wake-up and are never treated as proof of restored quota.
type Config struct {
	Router       *telemetry.MultiAccountUsageRouter
	Policy       *policy.Engine
	Accounts     AccountSource
	Transition   TransitionFunc
	Thresholds   policy.Thresholds
	FreshnessTTL time.Duration
	PollInterval time.Duration
	MaxWaitSlice time.Duration
	Now          func() time.Time
	// OnError receives a non-fatal observation/transition error. The loop
	// keeps running and schedules a bounded retry; a transient provider or
	// runtime failure must not silently disable automatic account switching.
	OnError func(error)
}

// ObservationResult is returned for operator diagnostics and tests. Each
// account retains its provider error; a failed account does not prevent other
// accounts from being evaluated.
type ObservationResult struct {
	Event         Event
	Decision      policy.PolicyDecision
	Observations  []telemetry.AccountObservation
	TransitionRan bool
	WaitUntil     time.Time
}

// Loop is safe for a single Run goroutine plus a status caller that invokes
// Evaluate directly. Policy itself owns event/cooldown synchronization.
type Loop struct {
	config Config
}

func New(config Config) *Loop {
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	if config.FreshnessTTL == 0 {
		config.FreshnessTTL = 5 * time.Minute
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 5 * time.Second
	}
	if config.MaxWaitSlice <= 0 {
		config.MaxWaitSlice = 30 * time.Second
	}
	return &Loop{config: config}
}

// Evaluate observes every configured account once, asks policy for an action,
// and invokes the transition callback only for a PolicyTransition decision.
// Reset/no-data decisions are returned with WaitUntil but are never converted
// into an optimistic account switch.
func (l *Loop) Evaluate(ctx context.Context, event Event) (ObservationResult, error) {
	if l == nil {
		return ObservationResult{}, errors.New("nil automation loop")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if l.config.Router == nil {
		return ObservationResult{}, ErrMissingRouter
	}
	if l.config.Policy == nil {
		return ObservationResult{}, ErrMissingPolicy
	}
	if l.config.Accounts.IDs == nil || l.config.Accounts.Active == nil {
		return ObservationResult{}, ErrMissingAccountSource
	}
	ids, err := l.config.Accounts.IDs()
	if err != nil {
		return ObservationResult{}, err
	}
	active, err := l.config.Accounts.Active()
	if err != nil {
		return ObservationResult{}, err
	}
	now := event.OccurredAt
	if now.IsZero() {
		now = l.config.Now()
	}
	now = now.UTC()
	observations, err := l.config.Router.Observe(ctx, ids, telemetry.ObserveOptions{MaxAge: l.config.FreshnessTTL, Now: now})
	if err != nil {
		return ObservationResult{}, err
	}
	accounts := make([]telemetry.AccountTelemetry, 0, len(observations))
	for _, observation := range observations {
		account := observation.Telemetry.Clone()
		if account.AccountID == "" {
			account.AccountID = observation.AccountID
		}
		if observation.Err != nil {
			account.IsUsable = false
		}
		accounts = append(accounts, account)
	}
	trigger := policy.TriggerManual
	switch event.Type {
	case EventThresholdReached:
		trigger = policy.TriggerProactiveThreshold
	case EventUsageLimitExceeded:
		trigger = policy.TriggerUsageLimitExceeded
	case EventResetDue:
		trigger = policy.TriggerResetRevalidation
	case EventTelemetryChanged:
		// Telemetry changes alone should be interpreted as a threshold check;
		// policy will stay when the active account remains below threshold.
		trigger = policy.TriggerProactiveThreshold
	}
	decision := l.config.Policy.EvaluateInput(policy.EvaluationInput{
		Accounts:        accounts,
		ActiveAccountID: active,
		Trigger:         trigger,
		Thresholds:      l.config.Thresholds,
		FreshnessTTL:    l.config.FreshnessTTL,
		EventID:         event.ID,
		Now:             now,
	})
	result := ObservationResult{Event: event, Decision: decision}
	if decision.Type == policy.PolicyTransition {
		if l.config.Transition == nil {
			l.config.Policy.TransitionFailed()
			return result, errors.New("automation transition callback is unavailable")
		}
		if err := ctx.Err(); err != nil {
			l.config.Policy.TransitionFailed()
			return result, err
		}
		if err := l.config.Transition(ctx, decision.AccountID); err != nil {
			l.config.Policy.TransitionFailed()
			return result, err
		}
		l.config.Policy.RecordTransition(active, decision.AccountID, now)
		l.config.Policy.ConfirmTransition(decision.AccountID)
		result.TransitionRan = true
	}
	result.WaitUntil = decision.RetryAt
	if result.WaitUntil.IsZero() {
		result.WaitUntil = decision.ResetAt
	}
	if result.WaitUntil.IsZero() && (decision.Type == policy.PolicyNoTelemetry || decision.Type == policy.PolicyCooldown) {
		result.WaitUntil = now.Add(l.config.PollInterval)
	}
	return result, nil
}

// Run consumes events until cancellation or the stream closes. Waiting is
// timer-driven and capped by MaxWaitSlice so a far-away reset or missing data
// cannot make shutdown unresponsive. Every wake re-observes telemetry before
// policy can authorize a transition.
func (l *Loop) Run(ctx context.Context, events <-chan Event) error {
	if l == nil {
		return errors.New("nil automation loop")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if events == nil {
		return errors.New("automation event stream is nil")
	}
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	arm := func(at time.Time) {
		if at.IsZero() {
			at = l.config.Now().Add(l.config.PollInterval)
		}
		delay := time.Until(at)
		if delay < 0 {
			delay = 0
		}
		if delay > l.config.MaxWaitSlice {
			delay = l.config.MaxWaitSlice
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(delay)
		}
		timerC = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-events:
			if !ok {
				return nil
			}
			result, err := l.Evaluate(ctx, event)
			if err != nil {
				if l.config.OnError != nil {
					l.config.OnError(err)
				}
				arm(l.config.Now().Add(l.config.PollInterval))
				continue
			}
			if !result.WaitUntil.IsZero() {
				arm(result.WaitUntil)
			}
		case <-timerC:
			wake := Event{Type: EventResetDue, OccurredAt: l.config.Now()}
			result, err := l.Evaluate(ctx, wake)
			if err != nil {
				if l.config.OnError != nil {
					l.config.OnError(err)
				}
				arm(l.config.Now().Add(l.config.PollInterval))
				continue
			}
			if !result.WaitUntil.IsZero() {
				arm(result.WaitUntil)
			} else {
				timerC = nil
			}
		}
	}
}

// AccountIDs normalizes a source list for adapters that want deterministic
// output before constructing a Loop.
func AccountIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			seen[id] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}
