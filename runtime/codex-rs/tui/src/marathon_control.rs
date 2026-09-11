//! Typed native Marathon values used by the TUI.
//!
//! Marathon requests are sent through the existing in-process app-server
//! request handle. This module intentionally contains no socket client and
//! does not read the legacy controller's socket environment setting; the
//! compatibility controller remains separate from the native TUI path.

pub(crate) type MarathonStatus = codex_app_server_protocol::MarathonStatusResponse;
pub(crate) type MarathonEnabledSetResult = codex_app_server_protocol::MarathonEnabledSetResponse;
pub(crate) type MarathonSwitchResult = codex_app_server_protocol::MarathonSwitchResponse;
pub(crate) type MarathonImportResult = codex_app_server_protocol::MarathonImportResponse;
