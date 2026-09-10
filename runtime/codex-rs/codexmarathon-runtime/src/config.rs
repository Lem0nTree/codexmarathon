//! Configuration for the in-process Marathon domain stores.
//!
//! This module only resolves paths and pure policy settings. It does not open
//! the native Codex AuthManager, mutate `auth.json`, or start any server.

use crate::errors::{DomainError, DomainResult};
use crate::policy::PolicyConfig;
use std::path::{Path, PathBuf};

/// Native Marathon storage configuration.
#[derive(Clone, Debug, PartialEq)]
pub struct MarathonConfig {
    codex_home: PathBuf,
    state_dir: PathBuf,
    registry_path: PathBuf,
    vault_dir: PathBuf,
    journal_path: PathBuf,
    auto_reset_state_path: PathBuf,
    legacy_auth_path: Option<PathBuf>,
    policy: PolicyConfig,
}

impl MarathonConfig {
    /// Build the default state layout below a Codex home.
    pub fn new(codex_home: impl Into<PathBuf>) -> DomainResult<Self> {
        let codex_home = codex_home.into();
        if codex_home.as_os_str().is_empty() {
            return Err(DomainError::InvalidConfig("codex_home is empty"));
        }
        let state_dir = codex_home.join("marathon");
        Ok(Self {
            codex_home,
            registry_path: state_dir.join("accounts.json"),
            vault_dir: state_dir.join("vault"),
            journal_path: state_dir.join("transitions.jsonl"),
            auto_reset_state_path: state_dir.join("auto-reset.json"),
            state_dir,
            legacy_auth_path: None,
            policy: PolicyConfig::default(),
        })
    }

    /// Override the Marathon state directory while preserving its file names.
    pub fn with_state_dir(mut self, state_dir: impl Into<PathBuf>) -> DomainResult<Self> {
        let state_dir = state_dir.into();
        if state_dir.as_os_str().is_empty() {
            return Err(DomainError::InvalidConfig("state_dir is empty"));
        }
        self.registry_path = state_dir.join("accounts.json");
        self.vault_dir = state_dir.join("vault");
        self.journal_path = state_dir.join("transitions.jsonl");
        self.auto_reset_state_path = state_dir.join("auto-reset.json");
        self.state_dir = state_dir;
        Ok(self)
    }

    /// Configure a legacy Codex auth document as an import source.
    pub fn with_legacy_auth_path(mut self, path: impl Into<PathBuf>) -> DomainResult<Self> {
        let path = path.into();
        if path.as_os_str().is_empty() {
            return Err(DomainError::InvalidConfig("legacy auth path is empty"));
        }
        self.legacy_auth_path = Some(path);
        Ok(self)
    }

    /// Replace pure policy settings.
    pub fn with_policy(mut self, policy: PolicyConfig) -> Self {
        self.policy = policy.normalized();
        self
    }

    /// Validate path relationships and policy values.
    pub fn validate(&self) -> DomainResult<()> {
        for path in [
            &self.codex_home,
            &self.state_dir,
            &self.registry_path,
            &self.vault_dir,
            &self.journal_path,
            &self.auto_reset_state_path,
        ] {
            if path.as_os_str().is_empty() {
                return Err(DomainError::InvalidConfig("configuration path is empty"));
            }
        }
        if self.registry_path == self.journal_path || self.registry_path == self.vault_dir {
            return Err(DomainError::InvalidConfig("state paths overlap"));
        }
        if self.policy.freshness_ttl <= chrono::Duration::zero()
            || self.policy.cooldown <= chrono::Duration::zero()
        {
            return Err(DomainError::InvalidConfig(
                "policy durations must be positive",
            ));
        }
        Ok(())
    }

    /// Codex home used to derive defaults.
    pub fn codex_home(&self) -> &Path {
        &self.codex_home
    }

    /// Marathon state directory.
    pub fn state_dir(&self) -> &Path {
        &self.state_dir
    }

    /// Non-secret account registry path.
    pub fn registry_path(&self) -> &Path {
        &self.registry_path
    }

    /// Protected opaque snapshot directory.
    pub fn vault_dir(&self) -> &Path {
        &self.vault_dir
    }

    /// Metadata-only transition journal path.
    pub fn journal_path(&self) -> &Path {
        &self.journal_path
    }

    /// Durable automatic reset state path. The file contains only enablement,
    /// idempotency, cooldown, and outcome metadata.
    pub fn auto_reset_state_path(&self) -> &Path {
        &self.auto_reset_state_path
    }

    /// Optional stock Codex auth file used only by explicit import scaffolding.
    pub fn legacy_auth_path(&self) -> Option<&Path> {
        self.legacy_auth_path.as_deref()
    }

    /// Pure policy settings.
    pub fn policy(&self) -> PolicyConfig {
        self.policy
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn config_derives_private_store_paths() {
        let config = MarathonConfig::new("/tmp/codex").expect("config");
        config.validate().expect("valid");
        assert_eq!(config.state_dir(), Path::new("/tmp/codex/marathon"));
        assert_eq!(config.vault_dir(), Path::new("/tmp/codex/marathon/vault"));
    }
}
