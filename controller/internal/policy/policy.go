// Package policy makes deterministic account-selection decisions from
// normalized telemetry. Observation, reset waiting, and runtime transitions
// remain separate: policy may request a transition or a revalidation wake-up,
// but it never deploys credentials or reloads the runtime.
package policy

import (
	"sort"
	"strings"
	"sync"
	"time"

	"codexmarathon/controller/internal/reset"
	"codexmarathon/controller/internal/telemetry"
)

const (
	defaultPrimaryThreshold   = 90.0
	defaultSecondaryThreshold = 95.0
	defaultSwitchCooldown     = time.Minute
	defaultFreshnessTTL       = 5 * time.Minute
	defaultRememberedEvents   = 256
)

// PolicyDecisionType is intentionally small and stable for controller and
// CLI consumers. PolicyNoTelemetry includes missing, stale, ambiguous, and
// failed observations; RefetchRequired/RefreshAccountIDs distinguish the
// actionable substate without treating an error as zero usage.
type PolicyDecisionType uint8

const (
	PolicyStay PolicyDecisionType = iota
	PolicyTransition
	PolicyWaitForReset
	PolicyNoTelemetry
	PolicyCooldown
)

// Compatibility aliases with the architecture names.
const (
	Stay              = PolicyStay
	Transition        = PolicyTransition
	WaitForReset      = PolicyWaitForReset
	NoTelemetry       = PolicyNoTelemetry
	NoEligibleAccount = PolicyNoTelemetry
	Cooldown          = PolicyCooldown
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
	case PolicyCooldown:
		return "cooldown"
	default:
		return "unknown"
	}
}

// Trigger identifies why policy is being evaluated. A proactive threshold
// event and a hard UsageLimitExceeded event share candidate ranking but have
// different behavior when the current account is still below its threshold.
type Trigger string

const (
	TriggerManual             Trigger = "manual"
	TriggerProactiveThreshold Trigger = "proactive_threshold"
	TriggerUsageLimitExceeded Trigger = "usage_limit_exceeded"
	TriggerResetRevalidation  Trigger = "reset_revalidation"
)

// TriggerType is a readable compatibility alias.
type TriggerType = Trigger

const (
	Manual             = TriggerManual
	ProactiveThreshold = TriggerProactiveThreshold
	UsageLimitExceeded = TriggerUsageLimitExceeded
	ResetRevalidation  = TriggerResetRevalidation
)

// Thresholds are the donor watcher's primary/secondary threshold policy,
// generalized to every normalized bucket. Values are percentages in [0,100].
type Thresholds struct {
	PrimaryPercent   float64
	SecondaryPercent float64
}

func DefaultThresholds() Thresholds {
	return Thresholds{PrimaryPercent: defaultPrimaryThreshold, SecondaryPercent: defaultSecondaryThreshold}
}

func (t Thresholds) normalized() Thresholds {
	defaults := DefaultThresholds()
	if t.PrimaryPercent <= 0 {
		t.PrimaryPercent = defaults.PrimaryPercent
	}
	if t.SecondaryPercent <= 0 {
		t.SecondaryPercent = defaults.SecondaryPercent
	}
	if t.PrimaryPercent > 100 {
		t.PrimaryPercent = 100
	}
	if t.SecondaryPercent > 100 {
		t.SecondaryPercent = 100
	}
	return t
}

// Reached reports whether any observed conventional window crossed its
// configured threshold. Callers should require a complete fresh snapshot
// before using this as authorization for a switch.
func (t Thresholds) Reached(account telemetry.AccountTelemetry) bool {
	t = t.normalized()
	for _, window := range account.WindowList() {
		switch window.Kind {
		case telemetry.PrimaryWindow:
			if window.UsedPercent >= t.PrimaryPercent {
				return true
			}
		case telemetry.SecondaryWindow:
			if window.UsedPercent >= t.SecondaryPercent {
				return true
			}
		}
	}
	return false
}

// EvaluationInput is the complete policy event context. Accounts may come
// from both an active runtime snapshot and inactive-profile providers. An
// absent or failed snapshot must be represented as unusable telemetry.
type EvaluationInput struct {
	Accounts        []telemetry.AccountTelemetry
	ActiveAccountID string
	Trigger         Trigger
	Thresholds      Thresholds
	FreshnessTTL    time.Duration
	CooldownUntil   time.Time
	PendingTargetID string
	EventID         string
	Now             time.Time
}

// PolicyInput and EvaluationRequest preserve descriptive call-site names.
type PolicyInput = EvaluationInput
type EvaluationRequest = EvaluationInput

// CandidateRank records the multi-bucket score used to select an account.
// The ordering intentionally preserves codex-switch's secondary-first,
// primary-second behavior while adding aggregate pressure and a stable ID
// tie-breaker for arbitrary metered bucket sets.
type CandidateRank struct {
	AccountID            string
	SecondaryUsedPercent float64
	PrimaryUsedPercent   float64
	MaxUsedPercent       float64
	TotalUsedPercent     float64
}

// PolicyDecision is the result of evaluating all eligible configured
// accounts. Reset fields are copied out of reset.ResetDecision so callers do
// not need to retain mutable scheduler state.
type PolicyDecision struct {
	Type               PolicyDecisionType
	Trigger            Trigger
	ActiveAccountID    string
	AccountID          string
	Reason             string
	CandidateAccountID string
	LimitID            string
	DurationMins       int
	ResetAt            time.Time
	RetryAt            time.Time
	CooldownUntil      time.Time
	RefetchRequired    bool
	RefreshAccountIDs  []string
	Rank               CandidateRank
}

func (d PolicyDecision) IsTransition() bool { return d.Type == PolicyTransition }

func (d PolicyDecision) IsWaiting() bool {
	return d.Type == PolicyWaitForReset || d.Type == PolicyNoTelemetry || d.Type == PolicyCooldown
}

// EngineConfig controls freshness, thresholds, and loop prevention. A zero
// config uses the same 90/95 threshold and one-minute cooldown defaults as
// codex-switch's watcher.
type EngineConfig struct {
	Thresholds       Thresholds
	FreshnessTTL     time.Duration
	Cooldown         time.Duration
	RememberedEvents int
}

// Engine evaluates telemetry and delegates pool-exhaustion scheduling to one
// reset scheduler. It is safe for a policy loop and timer loop to call its
// methods concurrently.
type Engine struct {
	scheduler reset.ResetScheduler
	now       func() time.Time
	config    EngineConfig

	mu             sync.Mutex
	cooldownUntil  time.Time
	pending        bool
	pendingFrom    string
	pendingTarget  string
	lastEventID    string
	seenEvents     map[string]time.Time
	lastTransition time.Time
}

// NewEngine constructs an engine. If scheduler is nil, a private scheduler
// is created. The optional clock is used when Evaluate receives a zero time.
func NewEngine(scheduler reset.ResetScheduler, clock ...func() time.Time) *Engine {
	if scheduler == nil {
		scheduler = reset.NewScheduler(clock...)
	}
	return &Engine{
		scheduler:  scheduler,
		now:        selectClock(clock...),
		config:     normalizeConfig(EngineConfig{}),
		seenEvents: make(map[string]time.Time),
	}
}

// NewPolicyEngine is a descriptive compatibility alias for NewEngine.
func NewPolicyEngine(scheduler reset.ResetScheduler, clock ...func() time.Time) *Engine {
	return NewEngine(scheduler, clock...)
}

// Evaluate is a stateless convenience for one policy tick. It retains the
// original package-level API while using the stricter normalized eligibility
// and reset handling implemented by Engine.
func Evaluate(accounts []telemetry.AccountTelemetry, now time.Time) PolicyDecision {
	return NewEngine(nil).Evaluate(accounts, now)
}

// Eligible reports whether one account has a complete fresh non-exhausted
// snapshot at now.
func Eligible(account telemetry.AccountTelemetry, now time.Time) bool {
	return account.HasFreshCapacity(now)
}

// NewConfiguredEngine constructs an engine with explicit policy settings.
func NewConfiguredEngine(config EngineConfig, scheduler reset.ResetScheduler, clock ...func() time.Time) *Engine {
	if scheduler == nil {
		scheduler = reset.NewScheduler(clock...)
	}
	return &Engine{
		scheduler:  scheduler,
		now:        selectClock(clock...),
		config:     normalizeConfig(config),
		seenEvents: make(map[string]time.Time),
	}
}

// Configure updates policy defaults. Existing cooldown/pending state is
// retained deliberately so a configuration reload cannot trigger a switch
// loop.
func (e *Engine) Configure(config EngineConfig) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.config = normalizeConfig(config)
	e.mu.Unlock()
}

// Evaluate retains the original API: it selects the first deterministic
// fresh account. New automatic callers should use EvaluateInput/Evaluate
// Trigger so active account and trigger semantics are explicit.
func (e *Engine) Evaluate(accounts []telemetry.AccountTelemetry, now time.Time) PolicyDecision {
	return e.evaluate(EvaluationInput{
		Accounts: accounts,
		Trigger:  TriggerManual,
		Now:      now,
		// Legacy callers historically did not configure an age TTL. Reset and
		// structural freshness remain enforced, but no wall-clock age is
		// imposed here; automatic callers use the configured TTL.
		FreshnessTTL: -1,
	})
}

// EvaluateInput evaluates one event and is the primary automatic-policy API.
func (e *Engine) EvaluateInput(input EvaluationInput) PolicyDecision {
	return e.evaluate(input)
}

// EvaluateRequest is an alias for integrations that use request terminology.
func (e *Engine) EvaluateRequest(input EvaluationInput) PolicyDecision {
	return e.evaluate(input)
}

// EvaluateTrigger evaluates a trigger without requiring callers to construct
// an EvaluationInput value.
func (e *Engine) EvaluateTrigger(accounts []telemetry.AccountTelemetry, activeAccountID string, trigger Trigger, thresholds Thresholds, now time.Time) PolicyDecision {
	return e.evaluate(EvaluationInput{
		Accounts:        accounts,
		ActiveAccountID: activeAccountID,
		Trigger:         trigger,
		Thresholds:      thresholds,
		Now:             now,
	})
}

// EvaluateUsageLimitExceeded is a convenience for the hard-limit runtime
// event. EventID may be empty when the runtime cannot provide one.
func (e *Engine) EvaluateUsageLimitExceeded(accounts []telemetry.AccountTelemetry, activeAccountID, eventID string, now time.Time) PolicyDecision {
	return e.evaluate(EvaluationInput{
		Accounts:        accounts,
		ActiveAccountID: activeAccountID,
		Trigger:         TriggerUsageLimitExceeded,
		EventID:         eventID,
		Now:             now,
	})
}

// EvaluateThreshold is the proactive threshold convenience method.
func (e *Engine) EvaluateThreshold(accounts []telemetry.AccountTelemetry, activeAccountID string, thresholds Thresholds, now time.Time) PolicyDecision {
	return e.evaluate(EvaluationInput{
		Accounts:        accounts,
		ActiveAccountID: activeAccountID,
		Trigger:         TriggerProactiveThreshold,
		Thresholds:      thresholds,
		Now:             now,
	})
}

// RecordTransition marks a transition as accepted by the transition layer.
// It arms the cooldown and suppresses duplicate event-driven switch requests
// until the target is confirmed or the caller records a failure.
func (e *Engine) RecordTransition(fromAccountID, toAccountID string, at time.Time) {
	if e == nil {
		return
	}
	if at.IsZero() {
		at = e.currentTime()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pending = true
	e.pendingFrom = fromAccountID
	e.pendingTarget = toAccountID
	e.lastTransition = at.UTC()
	e.cooldownUntil = at.UTC().Add(e.config.Cooldown)
}

// TransitionAccepted is an alias for RecordTransition.
func (e *Engine) TransitionAccepted(fromAccountID, toAccountID string, at time.Time) {
	e.RecordTransition(fromAccountID, toAccountID, at)
}

// ConfirmTransition clears pending suppression once the runtime reports the
// new identity. The cooldown remains in force to prevent an immediate bounce.
func (e *Engine) ConfirmTransition(accountID string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.pending && (accountID == "" || accountID == e.pendingTarget) {
		e.pending = false
		e.pendingFrom = ""
		e.pendingTarget = ""
	}
	e.mu.Unlock()
}

// TransitionFailed clears the in-flight suppression. It does not erase the
// last cooldown boundary when the transition layer has already changed the
// runtime identity; callers can explicitly ConfirmTransition in that case.
func (e *Engine) TransitionFailed() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.pending = false
	e.pendingFrom = ""
	e.pendingTarget = ""
	e.mu.Unlock()
}

// CooldownUntil returns the current loop-prevention boundary.
func (e *Engine) CooldownUntil() time.Time {
	if e == nil {
		return time.Time{}
	}
	e.mu.Lock()
	until := e.cooldownUntil
	e.mu.Unlock()
	return until
}

// PendingTarget returns the target currently suppressed as in flight.
func (e *Engine) PendingTarget() (string, bool) {
	if e == nil {
		return "", false
	}
	e.mu.Lock()
	target, pending := e.pendingTarget, e.pending
	e.mu.Unlock()
	return target, pending
}

func (e *Engine) evaluate(input EvaluationInput) PolicyDecision {
	if e == nil {
		return PolicyDecision{Type: PolicyNoTelemetry, Trigger: input.Trigger, Reason: "policy engine is nil"}
	}
	now := input.Now
	if now.IsZero() {
		now = e.currentTime()
	}
	now = now.UTC()
	trigger := input.Trigger
	if trigger == "" {
		trigger = TriggerManual
	}
	e.mu.Lock()
	config := e.config
	if config.RememberedEvents <= 0 {
		config.RememberedEvents = defaultRememberedEvents
	}
	cooldownUntil := e.cooldownUntil
	if input.CooldownUntil.After(cooldownUntil) {
		cooldownUntil = input.CooldownUntil
	}
	pending := e.pending
	pendingTarget := e.pendingTarget
	if input.PendingTargetID != "" {
		pending = true
		pendingTarget = input.PendingTargetID
	}
	duplicateEvent := input.EventID != "" && e.seenEventLocked(input.EventID, now, config.RememberedEvents)
	e.mu.Unlock()

	thresholds := input.Thresholds
	if thresholds.PrimaryPercent == 0 && thresholds.SecondaryPercent == 0 {
		thresholds = config.Thresholds
	}
	thresholds = thresholds.normalized()
	maxAge := input.FreshnessTTL
	if maxAge == 0 {
		maxAge = config.FreshnessTTL
	}
	if maxAge < 0 {
		maxAge = 0
	}
	ordered := cloneAndSort(input.Accounts)
	eligible := make([]telemetry.AccountTelemetry, 0, len(ordered))
	thresholded := make(map[string]bool, len(ordered))
	activeFound := false
	activeEligible := false
	activeThresholded := false
	for _, account := range ordered {
		ok, _ := account.IsCompleteFreshCapacity(now, maxAge)
		if input.ActiveAccountID != "" && account.AccountID == input.ActiveAccountID {
			activeFound = true
			activeEligible = ok
			activeThresholded = ok && thresholds.Reached(account)
		}
		if ok {
			eligible = append(eligible, account)
			thresholded[account.AccountID] = thresholds.Reached(account)
		}
	}

	decisionBase := PolicyDecision{
		Trigger:         trigger,
		ActiveAccountID: input.ActiveAccountID,
		CooldownUntil:   cooldownUntil,
	}
	if duplicateEvent {
		return e.stay(decisionBase, "duplicate policy event already handled")
	}
	if pending {
		return e.cooldown(decisionBase, now, pendingTarget, "a transition is already in flight")
	}
	if input.ActiveAccountID != "" && activeFound && trigger == TriggerProactiveThreshold && activeEligible && !activeThresholded {
		return e.stay(decisionBase, "active account is below configured thresholds")
	}

	legacySelection := trigger == TriggerManual && input.ActiveAccountID == "" && input.FreshnessTTL < 0
	if legacySelection {
		if len(eligible) > 0 {
			candidate := chooseBestCandidate(eligible, "")
			return e.transition(decisionBase, candidate, "account has fresh capacity", input.EventID)
		}
		return e.poolDecision(decisionBase, ordered, now, maxAge)
	}

	// A manual request without an active identity behaves like a first-start
	// selection. For automatic triggers the active account is excluded even if
	// it still has sub-100 capacity.
	if input.ActiveAccountID == "" && trigger == TriggerManual {
		if len(eligible) > 0 {
			candidate := chooseBestCandidate(eligible, "")
			return e.transition(decisionBase, candidate, "manual selection found fresh capacity", input.EventID)
		}
		return e.poolDecision(decisionBase, ordered, now, maxAge)
	}

	candidates := make([]telemetry.AccountTelemetry, 0, len(eligible))
	for _, account := range eligible {
		if account.AccountID == input.ActiveAccountID {
			continue
		}
		candidates = append(candidates, account)
	}
	if trigger == TriggerProactiveThreshold {
		belowThreshold := candidates[:0]
		for _, account := range candidates {
			if !thresholded[account.AccountID] {
				belowThreshold = append(belowThreshold, account)
			}
		}
		candidates = belowThreshold
	}
	if len(candidates) > 0 {
		if cooldownUntil.After(now) && trigger != TriggerManual {
			return e.cooldown(decisionBase, now, "", "switch cooldown is active")
		}
		candidate := chooseBestCandidate(candidates, input.ActiveAccountID)
		reason := "fresh account has lower quota pressure"
		if trigger == TriggerUsageLimitExceeded {
			reason = "UsageLimitExceeded requires a fresh replacement account"
		} else if trigger == TriggerProactiveThreshold {
			reason = "active account crossed the configured usage threshold"
		}
		return e.transition(decisionBase, candidate, reason, input.EventID)
	}

	// If there is a fresh account but all of them are at/above the proactive
	// threshold, waiting for a reset is not justified by stale data. Staying is
	// safer than thrashing between equally pressured accounts.
	if len(eligible) > 0 {
		if trigger == TriggerProactiveThreshold {
			return e.stay(decisionBase, "no replacement account is below the configured thresholds")
		}
		if input.ActiveAccountID != "" && activeEligible && trigger != TriggerUsageLimitExceeded {
			return e.stay(decisionBase, "no lower-pressure replacement account is available")
		}
	}
	return e.poolDecision(decisionBase, ordered, now, maxAge)
}

func (e *Engine) poolDecision(base PolicyDecision, accounts []telemetry.AccountTelemetry, now time.Time, maxAge time.Duration) PolicyDecision {
	if e.scheduler == nil {
		e.scheduler = reset.NewScheduler(e.now)
	}
	// Age and structural failures are represented as unusable/stale copies for
	// the scheduler. This prevents an old sub-100 snapshot from causing a
	// false ResetReady result.
	schedulerAccounts := make([]telemetry.AccountTelemetry, len(accounts))
	for i, account := range accounts {
		clone := account.Clone()
		// Preserve fresh exhausted windows so Scheduler can select their
		// authoritative future reset. Only age/future timestamp failures are
		// marked unusable/stale; otherwise an exhausted account would lose the
		// very reset metadata needed for pool-exhaustion waiting.
		if !account.IsUsable || account.AccountID == "" || (maxAge > 0 && (account.ObservedAt.IsZero() || account.ObservedAt.After(now) || now.Sub(account.ObservedAt) >= maxAge)) {
			clone.IsUsable = false
		}
		for key, limit := range clone.Limits {
			markWindow := func(window *telemetry.WindowTelemetry) {
				if window == nil {
					return
				}
				validAge := maxAge <= 0 || (!window.ObservedAt.IsZero() && !window.ObservedAt.After(now) && now.Sub(window.ObservedAt) < maxAge)
				preserveReset := window.IsExhausted() && window.HasFutureReset(now) && window.Freshness == telemetry.WindowFresh && validAge
				if !preserveReset && (!validAge || window.IsStaleAt(now)) {
					window.Freshness = telemetry.WindowStale
				}
			}
			for index := range limit.Windows {
				markWindow(&limit.Windows[index])
			}
			if limit.Primary != nil {
				markWindow(limit.Primary)
			}
			if limit.Secondary != nil {
				markWindow(limit.Secondary)
			}
			clone.Limits[key] = limit
		}
		schedulerAccounts[i] = clone
	}
	decision := e.scheduler.Evaluate(schedulerAccounts, now)
	base.RefetchRequired = decision.RefetchRequired
	base.RefreshAccountIDs = append([]string(nil), decision.RefreshAccountIDs...)
	base.CandidateAccountID = decision.CandidateAccountID
	base.LimitID = decision.LimitID
	base.DurationMins = decision.DurationMins
	base.ResetAt = decision.ResetAt
	base.Reason = decision.Reason
	if decision.State == reset.ResetWaitForReset {
		base.Type = PolicyWaitForReset
		return base
	}
	base.Type = PolicyNoTelemetry
	if next, ok := e.scheduler.(interface{ NextDataRecheck() (time.Time, bool) }); ok {
		if retry, exists := next.NextDataRecheck(); exists {
			base.RetryAt = retry
		}
	}
	if base.Reason == "" {
		base.Reason = "no fresh complete account telemetry is available"
	}
	return base
}

func (e *Engine) transition(base PolicyDecision, candidate telemetry.AccountTelemetry, reason, eventID string) PolicyDecision {
	base.Type = PolicyTransition
	base.AccountID = candidate.AccountID
	base.CandidateAccountID = candidate.AccountID
	base.Reason = reason
	base.Rank = Rank(candidate)
	if base.Trigger == TriggerManual {
		// Preserve the original one-shot/manual API: a caller that only asks for
		// a recommendation must explicitly RecordTransition after it accepts
		// the decision. Automatic event triggers reserve the target below before
		// returning so duplicate notifications cannot enqueue another request.
		return base
	}
	// Mark intent before returning so repeated runtime notifications cannot
	// enqueue the same switch before the transition coordinator acknowledges it.
	e.mu.Lock()
	e.pending = true
	e.pendingFrom = base.ActiveAccountID
	e.pendingTarget = candidate.AccountID
	e.lastEventID = eventID
	if eventID != "" {
		e.rememberEventLocked(eventID, e.currentTime())
	}
	e.mu.Unlock()
	return base
}

func (e *Engine) stay(base PolicyDecision, reason string) PolicyDecision {
	base.Type = PolicyStay
	base.Reason = reason
	return base
}

func (e *Engine) cooldown(base PolicyDecision, now time.Time, target, reason string) PolicyDecision {
	base.Type = PolicyCooldown
	base.AccountID = target
	base.CandidateAccountID = target
	base.Reason = reason
	if base.RetryAt.IsZero() {
		base.RetryAt = base.CooldownUntil
	}
	if base.RetryAt.Before(now) {
		base.RetryAt = now
	}
	return base
}

// Rank computes the deterministic donor-compatible score for one account.
func Rank(account telemetry.AccountTelemetry) CandidateRank {
	secondary, hasSecondary := account.UsedPercentByKind(telemetry.SecondaryWindow)
	primary, hasPrimary := account.UsedPercentByKind(telemetry.PrimaryWindow)
	if !hasSecondary {
		secondary = 0
	}
	if !hasPrimary {
		primary = 0
	}
	total := 0.0
	for _, window := range account.WindowList() {
		total += window.UsedPercent
	}
	return CandidateRank{
		AccountID:            account.AccountID,
		SecondaryUsedPercent: secondary,
		PrimaryUsedPercent:   primary,
		MaxUsedPercent:       account.MaxUsedPercent(),
		TotalUsedPercent:     total,
	}
}

func chooseBestCandidate(accounts []telemetry.AccountTelemetry, active string) telemetry.AccountTelemetry {
	ordered := append([]telemetry.AccountTelemetry(nil), accounts...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := Rank(ordered[i]), Rank(ordered[j])
		if left.SecondaryUsedPercent != right.SecondaryUsedPercent {
			return left.SecondaryUsedPercent < right.SecondaryUsedPercent
		}
		if left.PrimaryUsedPercent != right.PrimaryUsedPercent {
			return left.PrimaryUsedPercent < right.PrimaryUsedPercent
		}
		if left.MaxUsedPercent != right.MaxUsedPercent {
			return left.MaxUsedPercent < right.MaxUsedPercent
		}
		if left.TotalUsedPercent != right.TotalUsedPercent {
			return left.TotalUsedPercent < right.TotalUsedPercent
		}
		return left.AccountID < right.AccountID
	})
	if active != "" && len(ordered) > 1 && ordered[0].AccountID == active {
		return ordered[1]
	}
	return ordered[0]
}

func cloneAndSort(accounts []telemetry.AccountTelemetry) []telemetry.AccountTelemetry {
	ordered := make([]telemetry.AccountTelemetry, 0, len(accounts))
	seen := make(map[string]struct{}, len(accounts))
	for _, account := range accounts {
		if account.AccountID == "" {
			continue
		}
		if _, duplicate := seen[account.AccountID]; duplicate {
			continue
		}
		seen[account.AccountID] = struct{}{}
		ordered = append(ordered, account.Clone())
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].AccountID < ordered[j].AccountID })
	return ordered
}

func normalizeConfig(config EngineConfig) EngineConfig {
	config.Thresholds = config.Thresholds.normalized()
	if config.FreshnessTTL == 0 {
		config.FreshnessTTL = defaultFreshnessTTL
	}
	if config.FreshnessTTL < 0 {
		config.FreshnessTTL = 0
	}
	if config.Cooldown <= 0 {
		config.Cooldown = defaultSwitchCooldown
	}
	if config.RememberedEvents <= 0 {
		config.RememberedEvents = defaultRememberedEvents
	}
	return config
}

func (e *Engine) seenEventLocked(eventID string, now time.Time, max int) bool {
	if e.seenEvents == nil {
		e.seenEvents = make(map[string]time.Time)
	}
	if at, ok := e.seenEvents[eventID]; ok {
		if now.Sub(at) < 24*time.Hour {
			return true
		}
		delete(e.seenEvents, eventID)
	}
	if len(e.seenEvents) >= max {
		oldestID := ""
		var oldest time.Time
		for id, at := range e.seenEvents {
			if oldestID == "" || at.Before(oldest) {
				oldestID, oldest = id, at
			}
		}
		if oldestID != "" {
			delete(e.seenEvents, oldestID)
		}
	}
	return false
}

func (e *Engine) rememberEventLocked(eventID string, at time.Time) {
	if eventID == "" {
		return
	}
	if e.seenEvents == nil {
		e.seenEvents = make(map[string]time.Time)
	}
	e.seenEvents[eventID] = at.UTC()
}

// NormalizeReason trims caller-provided policy reasons for stable logs and
// journal entries. It is intentionally not used to make authorization
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
	if now := e.now(); !now.IsZero() {
		return now
	}
	return time.Now().UTC()
}
