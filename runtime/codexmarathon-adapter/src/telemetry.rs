//! Adapters for the two account telemetry shapes emitted by Codext.
//!
//! Codext's `account/rateLimits/updated` notification is sparse.  This module
//! deliberately does not merge it or select a bucket: that attribution is a
//! controller concern.  It only attaches the account observed on the runtime
//! connection and preserves the upstream camelCase payload members.

use crate::protocol::{
    EventBase, RateLimitSnapshot, RateLimitsSnapshotPayload, RateLimitsUpdatedPayload,
    RuntimeEvent, EVENT_RATE_LIMITS_SNAPSHOT, EVENT_RATE_LIMITS_UPDATED,
};

#[derive(Clone, Copy, Debug, Default)]
pub struct TelemetryForwarder;

impl TelemetryForwarder {
    pub fn snapshot(
        &self,
        base: EventBase,
        account_id: Option<String>,
        rate_limits: RateLimitSnapshot,
        rate_limits_by_limit_id: Option<
            std::collections::BTreeMap<String, RateLimitSnapshot>,
        >,
    ) -> Result<RuntimeEvent, serde_json::Error> {
        RuntimeEvent::new(
            EventBase {
                event_type: EVENT_RATE_LIMITS_SNAPSHOT.to_string(),
                ..base
            },
            &RateLimitsSnapshotPayload {
                account_id,
                rate_limits,
                rate_limits_by_limit_id,
            },
        )
    }

    pub fn sparse_update(
        &self,
        base: EventBase,
        account_id: Option<String>,
        rate_limits: RateLimitSnapshot,
    ) -> Result<RuntimeEvent, serde_json::Error> {
        RuntimeEvent::new(
            EventBase {
                event_type: EVENT_RATE_LIMITS_UPDATED.to_string(),
                ..base
            },
            &RateLimitsUpdatedPayload {
                account_id,
                rate_limits,
            },
        )
    }
}

