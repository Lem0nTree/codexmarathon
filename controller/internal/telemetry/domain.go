package telemetry

import (
	"sort"
	"time"
)

// WindowFreshness is tracked independently for every rate-limit window.  A
// reset boundary invalidates only the window whose reset has elapsed.
type WindowFreshness uint8

const (
	WindowFresh WindowFreshness = iota
	WindowStale
)

const (
	Fresh = WindowFresh
	Stale = WindowStale
)

func (f WindowFreshness) String() string {
	if f == WindowStale {
		return "stale"
	}
	return "fresh"
}

// WindowKind identifies the two conventional windows in a Codex rate-limit
// bucket.  The kind is useful after normalization because the wire shape uses
// named primary/secondary fields while policy and reset scheduling iterate a
// flat list.
type WindowKind string

const (
	PrimaryWindow   WindowKind = "primary"
	SecondaryWindow WindowKind = "secondary"
)

// AggregateLimitKey is used only when the server supplies an aggregate view
// without a limit ID.  It is intentionally impossible to confuse with a
// normal backend metered-limit identifier.
const AggregateLimitKey = "__aggregate__"

// WindowTelemetry is the normalized form of one primary or secondary usage
// window.  ResetsAt is nil when the server omitted reset metadata.
type WindowTelemetry struct {
	Kind               WindowKind
	UsedPercent        float64
	WindowDurationMins *int64
	ResetsAt           *time.Time
	ObservedAt         time.Time
	Freshness          WindowFreshness
}

// Clone returns a fully independent copy, including the reset timestamp.
func (w WindowTelemetry) Clone() WindowTelemetry {
	clone := w
	if w.WindowDurationMins != nil {
		value := *w.WindowDurationMins
		clone.WindowDurationMins = &value
	}
	if w.ResetsAt != nil {
		value := *w.ResetsAt
		clone.ResetsAt = &value
	}
	return clone
}

// IsStaleAt evaluates reset-time staleness without mutating the value.  An
// elapsed reset is an instruction to re-observe state; it never implies that
// usage has reset to zero.
func (w WindowTelemetry) IsStaleAt(now time.Time) bool {
	return w.Freshness == WindowStale || (w.ResetsAt != nil && !w.ResetsAt.After(now))
}

// RefreshStaleness updates only this window's freshness marker.
func (w *WindowTelemetry) RefreshStaleness(now time.Time) {
	if w == nil {
		return
	}
	if w.IsStaleAt(now) {
		w.Freshness = WindowStale
	} else {
		w.Freshness = WindowFresh
	}
}

// MarkStale is a concise alias used by reset-boundary callers.
func (w *WindowTelemetry) MarkStale(now time.Time) { w.RefreshStaleness(now) }

// IsExhausted reports the server's conventional exhausted threshold.  A
// caller still needs to check freshness before relying on the result.
func (w WindowTelemetry) IsExhausted() bool { return w.UsedPercent >= 100 }

// HasFutureReset reports whether this window carries an authoritative reset
// timestamp strictly after now.
func (w WindowTelemetry) HasFutureReset(now time.Time) bool {
	return w.ResetsAt != nil && w.ResetsAt.After(now)
}

// LimitTelemetry is one normalized metered bucket.  Windows is the canonical
// representation used by policy and reset scheduling.  Primary and Secondary
// are retained as convenience views for adapters and callers that prefer the
// original wire shape.
type LimitTelemetry struct {
	LimitID              string
	LimitName            string
	PlanType             string
	RateLimitReachedType string
	Windows              []WindowTelemetry
	Primary              *WindowTelemetry
	Secondary            *WindowTelemetry
}

// Clone returns an independent bucket snapshot.
func (l LimitTelemetry) Clone() LimitTelemetry {
	clone := l
	if l.Windows != nil {
		clone.Windows = make([]WindowTelemetry, len(l.Windows))
		for i, window := range l.Windows {
			clone.Windows[i] = window.Clone()
		}
	}
	if l.Primary != nil {
		window := l.Primary.Clone()
		clone.Primary = &window
	}
	if l.Secondary != nil {
		window := l.Secondary.Clone()
		clone.Secondary = &window
	}
	return clone
}

// WindowList returns a copy of the canonical flat window list.  It accepts
// hand-built values that populate only Primary/Secondary as well as values
// produced by NormalizeSnapshot.
func (l LimitTelemetry) WindowList() []WindowTelemetry {
	if len(l.Windows) > 0 {
		windows := make([]WindowTelemetry, len(l.Windows))
		for i, window := range l.Windows {
			windows[i] = window.Clone()
		}
		return windows
	}
	var windows []WindowTelemetry
	if l.Primary != nil {
		window := l.Primary.Clone()
		if window.Kind == "" {
			window.Kind = PrimaryWindow
		}
		windows = append(windows, window)
	}
	if l.Secondary != nil {
		window := l.Secondary.Clone()
		if window.Kind == "" {
			window.Kind = SecondaryWindow
		}
		windows = append(windows, window)
	}
	return windows
}

// RefreshStaleness applies the reset-boundary rule independently to every
// window, including convenience views.
func (l *LimitTelemetry) RefreshStaleness(now time.Time) {
	if l == nil {
		return
	}
	for i := range l.Windows {
		l.Windows[i].RefreshStaleness(now)
	}
	if l.Primary != nil {
		l.Primary.RefreshStaleness(now)
	}
	if l.Secondary != nil {
		l.Secondary.RefreshStaleness(now)
	}
}

// AccountTelemetry is an immutable-by-convention normalized account snapshot.
// StateStore and SnapshotCache enforce the convention by cloning on ingress
// and egress.
type AccountTelemetry struct {
	AccountID  string
	Limits     map[string]LimitTelemetry
	Aggregate  *LimitTelemetry
	ObservedAt time.Time
	IsUsable   bool
	Source     string
}

// Clone returns a fully independent account snapshot.
func (a AccountTelemetry) Clone() AccountTelemetry {
	clone := a
	if a.Limits != nil {
		clone.Limits = make(map[string]LimitTelemetry, len(a.Limits))
		for key, limit := range a.Limits {
			clone.Limits[key] = limit.Clone()
		}
	}
	if a.Aggregate != nil {
		aggregate := a.Aggregate.Clone()
		clone.Aggregate = &aggregate
	}
	return clone
}

// RefreshStaleness refreshes each window without changing any other account
// metadata.
func (a *AccountTelemetry) RefreshStaleness(now time.Time) {
	if a == nil {
		return
	}
	for key, limit := range a.Limits {
		limit.RefreshStaleness(now)
		a.Limits[key] = limit
	}
	if a.Aggregate != nil {
		a.Aggregate.RefreshStaleness(now)
	}
}

// MarkStale is the account-level alias for RefreshStaleness.
func (a *AccountTelemetry) MarkStale(now time.Time) { a.RefreshStaleness(now) }

// LimitList returns every distinct metered bucket in deterministic order.  A
// separately stored aggregate view is included only when it is not already
// represented by a map entry.
func (a AccountTelemetry) LimitList() []LimitTelemetry {
	if len(a.Limits) == 0 {
		if a.Aggregate == nil {
			return nil
		}
		return []LimitTelemetry{a.Aggregate.Clone()}
	}

	keys := make([]string, 0, len(a.Limits))
	for key := range a.Limits {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	limits := make([]LimitTelemetry, 0, len(keys)+1)
	seenIDs := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		limit := a.Limits[key].Clone()
		limits = append(limits, limit)
		if limit.LimitID != "" {
			seenIDs[limit.LimitID] = struct{}{}
		}
	}
	if a.Aggregate != nil {
		if a.Aggregate.LimitID == "" {
			// A nameless aggregate is useful when the map has no aggregate key;
			// avoid adding it twice when callers explicitly stored the sentinel
			// or when the multi-bucket view has exactly one bucket (the common
			// backward-compatible representation).
			if _, ok := a.Limits[AggregateLimitKey]; !ok && len(bucketKeys(a)) != 1 {
				limits = append(limits, a.Aggregate.Clone())
			}
		} else if _, ok := seenIDs[a.Aggregate.LimitID]; !ok {
			limits = append(limits, a.Aggregate.Clone())
		}
	}
	return limits
}

// WindowList returns a flattened copy of all distinct account windows.
func (a AccountTelemetry) WindowList() []WindowTelemetry {
	limits := a.LimitList()
	var windows []WindowTelemetry
	for _, limit := range limits {
		windows = append(windows, limit.WindowList()...)
	}
	return windows
}

// HasFreshCapacity reports whether every observed window is fresh and below
// the exhaustion threshold.  It deliberately returns false for an account
// with no windows or unusable telemetry.
func (a AccountTelemetry) HasFreshCapacity(now time.Time) bool {
	if !a.IsUsable {
		return false
	}
	windows := a.WindowList()
	if len(windows) == 0 {
		return false
	}
	for _, window := range windows {
		if window.IsStaleAt(now) || window.IsExhausted() {
			return false
		}
	}
	return true
}
