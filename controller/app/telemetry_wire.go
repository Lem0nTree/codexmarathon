package app

import (
	"time"

	runtimeapi "codexmarathon/controller/internal/runtime"
	"codexmarathon/controller/internal/telemetry"
)

// normalizeRuntimeSnapshot bridges the runtime transport's wire types to the
// telemetry domain's equivalent wire types. Keeping the transport and domain
// packages separate prevents the JSON-RPC client from depending on policy
// state, while this composition boundary owns the conversion.
func normalizeRuntimeSnapshot(accountID string, aggregate runtimeapi.RateLimitSnapshotWire, byID map[string]runtimeapi.RateLimitSnapshotWire, observedAt time.Time) telemetry.AccountTelemetry {
	converted := make(map[string]telemetry.RateLimitSnapshotWire, len(byID))
	for id, snapshot := range byID {
		converted[id] = convertRuntimeSnapshot(snapshot)
	}
	return telemetry.NormalizeSnapshot(accountID, convertRuntimeSnapshot(aggregate), converted, observedAt)
}

func convertRuntimeSnapshot(snapshot runtimeapi.RateLimitSnapshotWire) telemetry.RateLimitSnapshotWire {
	return telemetry.RateLimitSnapshotWire{
		LimitID:              cloneStringPointer(snapshot.LimitID),
		LimitName:            cloneStringPointer(snapshot.LimitName),
		PlanType:             cloneStringPointer(snapshot.PlanType),
		RateLimitReachedType: cloneStringPointer(snapshot.RateLimitReachedType),
		Primary:              convertRuntimeWindow(snapshot.Primary),
		Secondary:            convertRuntimeWindow(snapshot.Secondary),
	}
}

func convertRuntimeWindow(window *runtimeapi.RateLimitWindowWire) *telemetry.RateLimitWindowWire {
	if window == nil {
		return nil
	}
	return &telemetry.RateLimitWindowWire{
		UsedPercent:        window.UsedPercent,
		WindowDurationMins: cloneInt64Pointer(window.WindowDurationMins),
		ResetsAt:           cloneInt64Pointer(window.ResetsAt),
	}
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
