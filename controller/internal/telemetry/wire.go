// Package telemetry contains the controller-side representation of the
// account rate-limit data exposed by the Codext app-server.
package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// RateLimitWindowWire is the deliberately small subset of the Codext wire
// schema consumed by the controller.  A duration and reset timestamp are
// nullable in the upstream protocol; a nil value means that the server did
// not provide that piece of metadata.
//
// UsedPercent is kept as a number (rather than an integer) because the core
// protocol can originate fractional values even though the current app-server
// response rounds them for some clients.
type RateLimitWindowWire struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins *int64  `json:"windowDurationMins"`
	ResetsAt           *int64  `json:"resetsAt"`

	// usedPercentSet lets the decoder distinguish an explicit zero from an
	// omitted field when a sparse rolling update is decoded.  The field is
	// intentionally private so callers can continue to construct the canonical
	// wire type with a normal struct literal.
	usedPercentSet bool
}

// UnmarshalJSON preserves presence information for UsedPercent.  The
// upstream sparse notification normally contains a complete window object,
// but clients must also tolerate a future response that omits individual
// fields.
func (w *RateLimitWindowWire) UnmarshalJSON(data []byte) error {
	var decoded struct {
		UsedPercent        *float64 `json:"usedPercent"`
		WindowDurationMins *int64   `json:"windowDurationMins"`
		ResetsAt           *int64   `json:"resetsAt"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	// Reset all fields first: this method may be called more than once on a
	// reused value.
	*w = RateLimitWindowWire{
		WindowDurationMins: decoded.WindowDurationMins,
		ResetsAt:           decoded.ResetsAt,
		usedPercentSet:     decoded.UsedPercent != nil,
	}
	if decoded.UsedPercent != nil {
		w.UsedPercent = *decoded.UsedPercent
	}
	return nil
}

// HasUsedPercent reports whether a decoded wire window explicitly carried a
// usage value.  A non-zero struct literal is also treated as present, which
// keeps hand-written update tests and adapters ergonomic.
func (w RateLimitWindowWire) HasUsedPercent() bool {
	return w.usedPercentSet || w.UsedPercent != 0
}

// RateLimitSnapshotWire is the controller's MVP view of one metered bucket.
// Unknown upstream fields are intentionally ignored by encoding/json.
type RateLimitSnapshotWire struct {
	LimitID              *string              `json:"limitId"`
	LimitName            *string              `json:"limitName"`
	PlanType             *string              `json:"planType"`
	RateLimitReachedType *string              `json:"rateLimitReachedType"`
	Primary              *RateLimitWindowWire `json:"primary"`
	Secondary            *RateLimitWindowWire `json:"secondary"`
}

// GetAccountRateLimitsResponseWire is the response from
// account/rateLimits/read.  RateLimits is the historical aggregate view;
// RateLimitsByID is the multi-bucket representation keyed by metered limit
// ID.  A nil map means the server did not send the multi-bucket view.
type GetAccountRateLimitsResponseWire struct {
	RateLimits     RateLimitSnapshotWire            `json:"rateLimits"`
	RateLimitsByID map[string]RateLimitSnapshotWire `json:"rateLimitsByLimitId"`
}

// AccountRateLimitsUpdatedWire is the sparse rolling notification emitted by
// the runtime.  The account identity is supplied by the runtime connection,
// not by this notification.
type AccountRateLimitsUpdatedWire struct {
	RateLimits RateLimitSnapshotWire `json:"rateLimits"`
}

// Short aliases make the protocol vocabulary convenient at package
// boundaries while retaining the explicit Wire names for adapters.
type RateLimitWindow = RateLimitWindowWire
type RateLimitSnapshot = RateLimitSnapshotWire
type GetAccountRateLimitsResponse = GetAccountRateLimitsResponseWire
type AccountRateLimitsUpdated = AccountRateLimitsUpdatedWire

// DecodeRateLimitsResponse is a small convenience used by IPC adapters.  It
// rejects trailing JSON values while leaving unknown object fields ignored,
// matching normal Go JSON decoding semantics.
func DecodeRateLimitsResponse(data []byte) (GetAccountRateLimitsResponseWire, error) {
	var response GetAccountRateLimitsResponseWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&response); err != nil {
		return GetAccountRateLimitsResponseWire{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return GetAccountRateLimitsResponseWire{}, &TrailingJSONError{}
	} else if !errors.Is(err, io.EOF) {
		return GetAccountRateLimitsResponseWire{}, err
	}
	return response, nil
}

// TrailingJSONError indicates that a wire payload contained more than one
// JSON value.  It is kept as a concrete type so callers can classify malformed
// IPC frames without matching an implementation-specific error string.
type TrailingJSONError struct{}

func (*TrailingJSONError) Error() string { return "trailing JSON data" }
