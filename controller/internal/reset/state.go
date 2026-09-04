// Package reset owns pool-exhaustion waiting and reset-boundary
// revalidation.  It does not deploy credentials or reload the runtime.
package reset

import (
	"time"

	"codexmarathon/controller/internal/telemetry"
)

// ResetState is the scheduler's coarse state.  WAIT_FOR_RESET and
// WAIT_FOR_DATA are substates of POOL_EXHAUSTED; PoolExhausted is also set on
// decisions in either waiting state for callers that only need the coarse
// signal.
type ResetState uint8

const (
	ResetReady ResetState = iota
	ResetPoolExhausted
	ResetWaitForReset
	ResetWaitForData
)

const (
	StateReady         = ResetReady
	StatePoolExhausted = ResetPoolExhausted
	StateWaitForReset  = ResetWaitForReset
	StateWaitForData   = ResetWaitForData
)

func (s ResetState) String() string {
	switch s {
	case ResetReady:
		return "READY"
	case ResetPoolExhausted:
		return "POOL_EXHAUSTED"
	case ResetWaitForReset:
		return "WAIT_FOR_RESET"
	case ResetWaitForData:
		return "WAIT_FOR_DATA"
	default:
		return "UNKNOWN"
	}
}

// ResetWaitState is advisory metadata describing the last authoritative reset
// timestamp observed by the server.  Reaching ExpectedResetAt never proves
// capacity; callers must request fresh telemetry first.
type ResetWaitState struct {
	CandidateAccountID string
	LimitID            string
	WindowDurationMins int
	ExpectedResetAt    time.Time
	EnteredAt          time.Time
}

// ResetDecision is returned by Scheduler.Evaluate.
type ResetDecision struct {
	State              ResetState
	PoolExhausted      bool
	CandidateAccountID string
	LimitID            string
	DurationMins       int
	ResetAt            time.Time
	RefetchRequired    bool
	RefreshAccountIDs  []string
	Reason             string
}

// IsWaiting reports whether the decision asks the controller to pause normal
// account selection.
func (d ResetDecision) IsWaiting() bool {
	return d.State == ResetWaitForReset || d.State == ResetWaitForData || d.State == ResetPoolExhausted
}

// HasResetCandidate reports whether a future server-provided reset was found.
func (d ResetDecision) HasResetCandidate() bool {
	return !d.ResetAt.IsZero() && d.CandidateAccountID != ""
}

// RevalidationRequest identifies accounts that must be refreshed after a
// reset boundary.  It is intentionally data-only; the provider/router owns
// the actual network request.
type RevalidationRequest struct {
	AccountIDs []string
	DueAt      time.Time
}

// ResetScheduler is the narrow interface consumed by policy code.
type ResetScheduler interface {
	Evaluate(accounts []telemetry.AccountTelemetry, now time.Time) ResetDecision
	NextRecheck() (time.Time, bool)
	Clear()
}
