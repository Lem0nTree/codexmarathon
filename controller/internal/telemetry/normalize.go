package telemetry

import "time"

// NormalizeWindow converts an upstream wire window into a fresh normalized
// window.  Freshness is evaluated again by StateStore/Policy at decision time
// so an elapsed reset can never be mistaken for capacity.
func NormalizeWindow(wire RateLimitWindowWire, kind WindowKind, observedAt time.Time) WindowTelemetry {
	window := WindowTelemetry{
		Kind:               kind,
		UsedPercent:        wire.UsedPercent,
		WindowDurationMins: cloneInt64(wire.WindowDurationMins),
		ObservedAt:         observedAt,
		Freshness:          WindowFresh,
	}
	if wire.ResetsAt != nil {
		reset := time.Unix(*wire.ResetsAt, 0).UTC()
		window.ResetsAt = &reset
	}
	window.RefreshStaleness(observedAt)
	return window
}

// NormalizeLimitSnapshot converts one wire bucket into a domain bucket.
func NormalizeLimitSnapshot(wire RateLimitSnapshotWire, observedAt time.Time) LimitTelemetry {
	limit := LimitTelemetry{
		LimitID:              derefString(wire.LimitID),
		LimitName:            derefString(wire.LimitName),
		PlanType:             derefString(wire.PlanType),
		RateLimitReachedType: derefString(wire.RateLimitReachedType),
	}
	if wire.Primary != nil {
		window := NormalizeWindow(*wire.Primary, PrimaryWindow, observedAt)
		limit.Primary = &window
		limit.Windows = append(limit.Windows, window.Clone())
	}
	if wire.Secondary != nil {
		window := NormalizeWindow(*wire.Secondary, SecondaryWindow, observedAt)
		limit.Secondary = &window
		limit.Windows = append(limit.Windows, window.Clone())
	}
	return limit
}

// NormalizeSnapshot converts the aggregate and optional multi-bucket views
// into a controller account snapshot.  The aggregate view is always retained
// separately so sparse updates can apply it coherently with a matching bucket.
// When the server provides no multi-bucket map, the aggregate bucket is also
// indexed by its ID (or AggregateLimitKey when it has none).
func NormalizeSnapshot(accountID string, aggregate RateLimitSnapshotWire, byID map[string]RateLimitSnapshotWire, observedAt time.Time) AccountTelemetry {
	telemetry := AccountTelemetry{
		AccountID:  accountID,
		Limits:     make(map[string]LimitTelemetry, len(byID)),
		ObservedAt: observedAt,
		IsUsable:   true,
		Source:     "runtime",
	}

	aggregateLimit := NormalizeLimitSnapshot(aggregate, observedAt)
	telemetry.Aggregate = &aggregateLimit

	if len(byID) == 0 {
		key := aggregateLimit.LimitID
		if key == "" {
			key = AggregateLimitKey
		}
		telemetry.Limits[key] = aggregateLimit.Clone()
		return telemetry
	}

	for key, snapshot := range byID {
		limit := NormalizeLimitSnapshot(snapshot, observedAt)
		if key == "" {
			key = limit.LimitID
			if key == "" {
				key = AggregateLimitKey
			}
		}
		if limit.LimitID == "" && key != AggregateLimitKey {
			// The map key is the authoritative metered ID when a backend
			// omits the duplicated limitId field in a bucket value.
			limit.LimitID = key
		}
		telemetry.Limits[key] = limit
	}
	return telemetry
}

// NormalizeRateLimits is the response-shaped convenience form used by
// runtime adapters.
func NormalizeRateLimits(accountID string, response GetAccountRateLimitsResponseWire, observedAt time.Time) AccountTelemetry {
	return NormalizeSnapshot(accountID, response.RateLimits, response.RateLimitsByID, observedAt)
}

// NormalizeResponse is an alias retained for callers that use the protocol
// response terminology.
func NormalizeResponse(accountID string, response GetAccountRateLimitsResponseWire, observedAt time.Time) AccountTelemetry {
	return NormalizeRateLimits(accountID, response, observedAt)
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
