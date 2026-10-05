//! Credential-bearing executor for accountd's credential-free expiry ledger.
//!
//! Every account operation holds the existing Marathon state lock. Inactive
//! credentials live only in a private temporary native home; active credentials
//! use the native home and native cross-process refresh exclusion.
use crate::auto_reset::QuotaResetCapability;
use crate::{
    AccountTelemetry, AuthSnapshot, FileAccountRegistry, FileSnapshotVault, Freshness,
    LimitTelemetry, MarathonConfig, QuotaSnapshotStore, UsageWindow, WindowKind,
};
use chrono::{DateTime, Utc};
use codex_backend_client::{Client, ConsumeRateLimitResetCreditCode, RateLimitResetCreditDetails};
use codex_core::config::{Config, ConfigBuilder};
use codex_login::{
    AuthConfig, AuthCredentialsStoreMode, AuthDotJson, AuthKeyringBackendKind, AuthManager,
    AuthManagerConfig, save_auth,
};
use codexmarathon_accountd_client::{
    AccountdClient, ExpiryCompletion, ExpiryCredit, ExpiryJob, ExpiryObservation, ExpiryOutcome,
};
use std::collections::BTreeMap;
use std::path::PathBuf;
use std::time::Duration;

const REQUEST_TIMEOUT: Duration = Duration::from_secs(10);
const JOB_TIMEOUT: Duration = Duration::from_secs(65);
const INVENTORY_INTERVAL: Duration = Duration::from_secs(30);
const DISCOVERY_TIMEOUT: Duration = Duration::from_secs(25);

type Result<T> = std::result::Result<T, &'static str>;

pub struct ExpiryExecutor {
    config: MarathonConfig,
    daemon: AccountdClient,
    registry: FileAccountRegistry,
    vault: FileSnapshotVault,
    quotas: QuotaSnapshotStore,
    native_config: tokio::sync::OnceCell<Config>,
}

impl ExpiryExecutor {
    pub fn new(home: PathBuf, daemon: AccountdClient) -> Result<Self> {
        let config = MarathonConfig::new(home).map_err(|_| "invalid_home")?;
        Ok(Self {
            registry: FileAccountRegistry::new(config.registry_path()),
            vault: FileSnapshotVault::new(config.vault_dir()),
            quotas: QuotaSnapshotStore::new(config.quota_db_path()),
            config,
            daemon,
            native_config: tokio::sync::OnceCell::new(),
        })
    }

    // Validate the metadata daemon before even opening the credential vault.
    async fn enabled(&self) -> Result<bool> {
        let status = self
            .daemon
            .expiry_status()
            .await
            .map_err(|_| "daemon_unavailable")?;
        let daemon_home = status
            .codex_home
            .as_deref()
            .and_then(|home| std::fs::canonicalize(home).ok());
        let expected_home = std::fs::canonicalize(self.config.codex_home()).ok();
        if daemon_home.is_none() || daemon_home != expected_home {
            return Err("daemon_home_mismatch");
        }
        Ok(status.effective_enabled)
    }

    pub async fn run(&self) -> Result<()> {
        let mut due = BTreeMap::<String, tokio::time::Instant>::new();
        loop {
            if self.enabled().await.is_ok() {
                let _ = self.daemon.expiry_executor_heartbeat().await;
            } else {
                tokio::time::sleep(Duration::from_secs(2)).await;
                continue;
            }
            if self.enabled().await.unwrap_or(false) {
                if let Ok(state) = self.registry.preview_state() {
                    for id in state.accounts.keys() {
                        if due
                            .get(id)
                            .is_none_or(|at| *at <= tokio::time::Instant::now())
                        {
                            // One account at a time; no concurrent redemption or refresh owner.
                            let started = tokio::time::Instant::now();
                            let _ = self.daemon.expiry_executor_heartbeat().await;
                            let _ = tokio::time::timeout(
                                DISCOVERY_TIMEOUT,
                                self.account_operation(id, None, false),
                            )
                            .await;
                            due.insert(id.clone(), started + INVENTORY_INTERVAL);
                            self.process_job().await;
                        }
                    }
                    due.retain(|id, _| state.accounts.contains_key(id));
                }
                self.process_job().await;
            }
            tokio::time::sleep(Duration::from_secs(2)).await;
        }
    }

    async fn process_job(&self) {
        if self.enabled().await.unwrap_or(false)
            && let Ok(Some(job)) = self.daemon.expiry_claim().await
        {
            let result = tokio::time::timeout(
                JOB_TIMEOUT,
                self.account_operation(&job.account_id, Some(&job), false),
            )
            .await;
            let (outcome, diagnostic_code) = match result {
                Ok(Ok(outcome)) => (outcome, None),
                Ok(Err(code)) => (ExpiryOutcome::Retry, Some(code.to_owned())),
                Err(_) => (ExpiryOutcome::Retry, Some("operation_timeout".to_owned())),
            };
            // Failed completion leaves the leased job to retry using its
            // persisted key. Quota refresh failure cannot change success.
            if self.enabled().await.is_ok() {
                let _ = self
                    .daemon
                    .expiry_complete(ExpiryCompletion {
                        job_id: job.job_id,
                        lease_token: job.lease_token,
                        outcome,
                        diagnostic_code,
                    })
                    .await;
            }
            // Publish the terminal result before any quota refresh. A
            // refresh timeout can never reopen successful consumption.
            if matches!(
                outcome,
                ExpiryOutcome::Reset | ExpiryOutcome::AlreadyRedeemed | ExpiryOutcome::NoCredit
            ) {
                let _ = tokio::time::timeout(
                    JOB_TIMEOUT,
                    self.account_operation(&job.account_id, None, true),
                )
                .await;
            }
        }
    }

    async fn account_operation(
        &self,
        id: &str,
        job: Option<&ExpiryJob>,
        refresh_quota: bool,
    ) -> Result<ExpiryOutcome> {
        if !self.enabled().await? {
            return Err("disabled");
        }
        let lock = self.registry.lock_exclusive().map_err(|_| "state_busy")?;
        let state = self
            .registry
            .state_locked(&lock)
            .map_err(|_| "registry_unavailable")?;
        if !state.enabled {
            return Err("disabled");
        }
        let account = state.accounts.get(id).ok_or("account_removed")?;
        let reference = if account.credential_ref.is_empty() {
            id
        } else {
            &account.credential_ref
        };
        let native = self
            .native_config
            .get_or_try_init(|| async {
                ConfigBuilder::default()
                    .codex_home(self.config.codex_home().to_path_buf())
                    .fallback_cwd(Some(self.config.codex_home().to_path_buf()))
                    .build()
                    .await
                    .map_err(|_| "native_config_unavailable")
            })
            .await?;
        let factory = native.http_client_factory();
        let inactive_home;
        let home = if state.active_account_id == id {
            self.config.codex_home()
        } else {
            // Registry transitions use this same lock, so this account cannot
            // become active until refreshed credentials are durably published.
            let snapshot = self
                .vault
                .load_locked(&lock, reference)
                .map_err(|_| "vault_unavailable")?;
            let auth: AuthDotJson =
                serde_json::from_slice(snapshot.bytes()).map_err(|_| "invalid_auth")?;
            require_identity(snapshot_identity(&auth).as_deref(), id)?;
            inactive_home = tempfile::tempdir().map_err(|_| "isolated_home_failed")?;
            save_auth(
                inactive_home.path(),
                &auth,
                AuthCredentialsStoreMode::File,
                AuthKeyringBackendKind::default(),
            )
            .map_err(|_| "auth_stage_failed")?;
            inactive_home.path()
        };
        let auth_config = AuthConfig {
            codex_home: home.to_path_buf(),
            auth_credentials_store_mode: if state.active_account_id == id {
                native.cli_auth_credentials_store_mode
            } else {
                AuthCredentialsStoreMode::File
            },
            keyring_backend_kind: if state.active_account_id == id {
                native.auth_keyring_backend_kind()
            } else {
                AuthKeyringBackendKind::default()
            },
            forced_login_method: native.forced_login_method(),
            chatgpt_base_url: Some(native.chatgpt_base_url.clone()),
            forced_chatgpt_workspace_id: native.forced_chatgpt_workspace_id.clone(),
            managed_auth_policy: native.managed_auth_policy(),
            auth_route_config: native.auth_route_config(),
        };
        let manager = tokio::time::timeout(
            REQUEST_TIMEOUT,
            AuthManager::shared_from_auth_config(auth_config, false),
        )
        .await
        .map_err(|_| "auth_timeout")?
        .map_err(|_| "native_auth_unavailable")?;
        // Verify before proactive native refresh and after it. Never install an
        // inactive snapshot into the real home or change the active marker.
        require_identity(
            manager
                .auth_cached()
                .and_then(|auth| auth.get_account_id())
                .as_deref(),
            id,
        )?;
        let auth = tokio::time::timeout(REQUEST_TIMEOUT, manager.auth())
            .await
            .map_err(|_| "auth_timeout")?
            .ok_or("authentication_required")?;
        require_identity(auth.get_account_id().as_deref(), id)?;
        if !auth.uses_codex_backend() {
            return Err("unsupported_auth");
        }
        self.persist_auth(id, &manager, &lock).await?;
        let mut client = Client::from_auth(native.chatgpt_base_url.clone(), &auth, factory);
        if refresh_quota {
            let _ = self.refresh_quota(id, &client).await;
        }
        let inventory = match request(client.list_rate_limit_reset_credits()).await {
            Err("provider_unauthorized") => {
                client = self
                    .recover_unauthorized(id, &manager, &lock, native)
                    .await?;
                request(client.list_rate_limit_reset_credits()).await?
            }
            result => result?,
        };
        let credits: Vec<_> = inventory.credits.into_iter().map(map_credit).collect();
        if !self.enabled().await? {
            return Err("disabled");
        }
        self.daemon
            .expiry_observe(ExpiryObservation {
                account_id: id.to_owned(),
                observed_at: Utc::now(),
                credits: Some(credits.clone()),
            })
            .await
            .map_err(|_| "observation_failed")?;
        let Some(job) = job else {
            return Ok(ExpiryOutcome::Retry);
        };
        // A missing detailed ID can be caused by a capped inventory. It never
        // authorizes choosing a different credit or inventing an expiry.
        match credit_action(&credits, job, Utc::now())? {
            CreditAction::AlreadyRedeemed => return Ok(ExpiryOutcome::AlreadyRedeemed),
            CreditAction::Submit => {}
        }
        if !self.enabled().await? {
            return Err("disabled");
        }
        let response =
            match request(
                client.consume_rate_limit_reset_credit_by_id(&job.idempotency_key, &job.credit_id),
            )
            .await
            {
                Err("provider_unauthorized") => {
                    client = self
                        .recover_unauthorized(id, &manager, &lock, native)
                        .await?;
                    if !self.enabled().await? {
                        return Err("disabled");
                    }
                    request(client.consume_rate_limit_reset_credit_by_id(
                        &job.idempotency_key,
                        &job.credit_id,
                    ))
                    .await?
                }
                result => result?,
            };
        let outcome = map_outcome(response.code);
        // Complete the ledger before refreshing telemetry in the outer loop.
        Ok(outcome)
    }

    async fn persist_auth(
        &self,
        id: &str,
        manager: &AuthManager,
        lock: &crate::state_lock::StateLock,
    ) -> Result<()> {
        let updated = manager
            .snapshot_for_transition(id)
            .await
            .map_err(|_| "auth_snapshot_unavailable")?
            .into_auth_dot_json();
        require_identity(snapshot_identity(&updated).as_deref(), id)?;
        let snapshot = AuthSnapshot::from_owned_bytes(
            Some(id),
            serde_json::to_vec(&updated).map_err(|_| "auth_snapshot_failed")?,
        )
        .map_err(|_| "auth_identity_mismatch")?;
        let mut state = self
            .registry
            .state_locked(lock)
            .map_err(|_| "registry_unavailable")?;
        let account = state.accounts.get(id).ok_or("account_removed")?;
        let reference = if account.credential_ref.is_empty() {
            id
        } else {
            &account.credential_ref
        };
        if self.vault.load_locked(lock, reference).ok().as_ref() != Some(&snapshot) {
            let reference = self
                .vault
                .save_fresh_locked(lock, &snapshot)
                .map_err(|_| "vault_save_failed")?;
            state
                .accounts
                .get_mut(id)
                .ok_or("account_removed")?
                .credential_ref = reference;
            self.registry
                .replace_locked(lock, state)
                .map_err(|_| "registry_save_failed")?;
        }
        Ok(())
    }

    async fn recover_unauthorized(
        &self,
        id: &str,
        manager: &AuthManager,
        lock: &crate::state_lock::StateLock,
        native: &Config,
    ) -> Result<Client> {
        if !self.enabled().await? {
            return Err("disabled");
        }
        tokio::time::timeout(REQUEST_TIMEOUT, manager.refresh_token())
            .await
            .map_err(|_| "auth_refresh_timeout")?
            .map_err(|_| "auth_refresh_failed")?;
        self.persist_auth(id, manager, lock).await?;
        let auth = manager.auth_cached().ok_or("authentication_required")?;
        require_identity(auth.get_account_id().as_deref(), id)?;
        Ok(Client::from_auth(
            native.chatgpt_base_url.clone(),
            &auth,
            native.http_client_factory(),
        ))
    }

    async fn refresh_quota(&self, id: &str, client: &Client) -> Result<()> {
        let response = request(client.get_rate_limits_with_reset_credits()).await?;
        if let Some(observed) = response.account_id.as_deref() {
            require_identity(Some(observed), id)?;
        }
        let now = Utc::now();
        let mut limits = BTreeMap::new();
        for snapshot in response.rate_limits {
            let limit_id = snapshot.limit_id.unwrap_or_else(|| "codex".to_owned());
            let mut windows = Vec::new();
            for (kind, window) in [
                (WindowKind::Primary, snapshot.primary),
                (WindowKind::Secondary, snapshot.secondary),
            ] {
                if let Some(window) = window {
                    windows.push(UsageWindow {
                        kind,
                        used_percent: window.used_percent,
                        window_duration_mins: window
                            .window_minutes
                            .and_then(|n| u64::try_from(n).ok()),
                        resets_at: window
                            .resets_at
                            .and_then(|n| DateTime::from_timestamp(n, 0)),
                        observed_at: now,
                        freshness: Freshness::Fresh,
                    });
                }
            }
            limits.insert(
                limit_id.clone(),
                LimitTelemetry {
                    limit_id,
                    limit_name: String::new(),
                    plan_type: String::new(),
                    windows,
                },
            );
        }
        self.quotas
            .upsert(&AccountTelemetry {
                account_id: id.to_owned(),
                limits,
                observed_at: now,
                usable: true,
                reset_capability: QuotaResetCapability::Unavailable,
                source: "banked_reset_executor".to_owned(),
            })
            .await
            .map_err(|_| "quota_save_failed")
    }
}

#[derive(Debug, Eq, PartialEq)]
enum CreditAction {
    Submit,
    AlreadyRedeemed,
}

fn credit_action(
    credits: &[ExpiryCredit],
    job: &ExpiryJob,
    now: DateTime<Utc>,
) -> Result<CreditAction> {
    if job.expires_at <= now {
        return Err("credit_expired");
    }
    match credits
        .iter()
        .find(|credit| credit.credit_id == job.credit_id)
    {
        Some(credit) if credit.status == "redeemed" => Ok(CreditAction::AlreadyRedeemed),
        Some(credit)
            if credit.status == "available"
                && credit.expires_at.map(|at| at.timestamp_millis())
                    == Some(job.expires_at.timestamp_millis()) =>
        {
            Ok(CreditAction::Submit)
        }
        // Capped inventories may omit a submitted credit. Replaying the exact
        // daemon-owned credit/key reconciles ambiguous results without choosing
        // or consuming another credit. Prior eligibility remains ledger-owned.
        None if job.attempt > 1 => Ok(CreditAction::Submit),
        None => Err("credit_detail_unavailable"),
        _ => Err("credit_ineligible"),
    }
}

async fn request<T>(future: impl std::future::Future<Output = anyhow::Result<T>>) -> Result<T> {
    tokio::time::timeout(REQUEST_TIMEOUT, future)
        .await
        .map_err(|_| "provider_timeout")?
        .map_err(|error| {
            if let Some(error) = error.downcast_ref::<codex_backend_client::RequestError>() {
                if error.is_unauthorized() {
                    return "provider_unauthorized";
                }
                if error.status().is_some_and(|status| status.as_u16() == 429) {
                    return "provider_rate_limited";
                }
            }
            "provider_failed"
        })
}

fn require_identity(observed: Option<&str>, expected: &str) -> Result<()> {
    if observed == Some(expected) {
        Ok(())
    } else {
        Err("auth_identity_mismatch")
    }
}

fn snapshot_identity(auth: &AuthDotJson) -> Option<String> {
    auth.tokens.as_ref().and_then(|tokens| {
        tokens
            .account_id
            .clone()
            .or_else(|| tokens.id_token.chatgpt_account_id.clone())
    })
}

fn map_credit(credit: RateLimitResetCreditDetails) -> ExpiryCredit {
    let supported = credit.reset_type == "codex_rate_limits";
    ExpiryCredit {
        credit_id: credit.id,
        status: if supported {
            credit.status
        } else {
            "unknown".to_owned()
        },
        expires_at: credit
            .expires_at
            .and_then(|at| DateTime::parse_from_rfc3339(&at).ok())
            .map(|at| at.with_timezone(&Utc)),
    }
}

fn map_outcome(code: ConsumeRateLimitResetCreditCode) -> ExpiryOutcome {
    match code {
        ConsumeRateLimitResetCreditCode::Reset => ExpiryOutcome::Reset,
        ConsumeRateLimitResetCreditCode::AlreadyRedeemed => ExpiryOutcome::AlreadyRedeemed,
        ConsumeRateLimitResetCreditCode::NoCredit => ExpiryOutcome::NoCredit,
        ConsumeRateLimitResetCreditCode::NothingToReset => ExpiryOutcome::NothingToReset,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn credit(reset_type: &str, expires_at: Option<&str>) -> RateLimitResetCreditDetails {
        RateLimitResetCreditDetails {
            id: "credit".into(),
            reset_type: reset_type.into(),
            status: "available".into(),
            granted_at: String::new(),
            expires_at: expires_at.map(str::to_owned),
            title: None,
            description: None,
        }
    }
    #[test]
    fn supported_credit_requires_a_parseable_expiry() {
        assert_eq!(
            map_credit(credit("Other", Some("2030-01-01T00:00:00Z"))).status,
            "unknown"
        );
        assert!(
            map_credit(credit("codex_rate_limits", None))
                .expires_at
                .is_none()
        );
        assert!(
            map_credit(credit("codex_rate_limits", Some("bad")))
                .expires_at
                .is_none()
        );
        assert!(
            map_credit(credit("codex_rate_limits", Some("2030-01-01T00:00:00Z")))
                .expires_at
                .is_some()
        );
    }
    #[test]
    fn mismatch_and_missing_identity_fail_closed() {
        assert!(require_identity(Some("a"), "a").is_ok());
        assert!(require_identity(Some("a"), "b").is_err());
        assert!(require_identity(None, "a").is_err());
    }
    #[test]
    fn non_success_codes_never_become_redemption_success() {
        assert_eq!(
            map_outcome(ConsumeRateLimitResetCreditCode::NoCredit),
            ExpiryOutcome::NoCredit
        );
        assert_eq!(
            map_outcome(ConsumeRateLimitResetCreditCode::NothingToReset),
            ExpiryOutcome::NothingToReset
        );
        assert_eq!(
            map_outcome(ConsumeRateLimitResetCreditCode::AlreadyRedeemed),
            ExpiryOutcome::AlreadyRedeemed
        );
    }
    fn job(attempt: i64) -> ExpiryJob {
        ExpiryJob {
            job_id: "job".into(),
            account_id: "a".into(),
            credit_id: "credit".into(),
            idempotency_key: "same-key".into(),
            lease_token: "lease".into(),
            expires_at: Utc::now() + chrono::Duration::minutes(5),
            attempt,
        }
    }
    #[test]
    fn ambiguous_retry_replays_the_same_credit_and_key_when_inventory_omits_it() {
        let mut job = job(1);
        assert_eq!(
            credit_action(&[], &job, Utc::now()),
            Err("credit_detail_unavailable")
        );
        let original = (job.credit_id.clone(), job.idempotency_key.clone());
        // The fake provider consumed this key but its response timed out. A
        // subsequent capped listing no longer includes the redeemed credit.
        job.attempt = 2;
        assert_eq!(
            credit_action(&[], &job, Utc::now()),
            Ok(CreditAction::Submit)
        );
        assert_eq!(
            (job.credit_id.clone(), job.idempotency_key.clone()),
            original
        );
        assert_eq!(
            map_outcome(ConsumeRateLimitResetCreditCode::AlreadyRedeemed),
            ExpiryOutcome::AlreadyRedeemed
        );
        job.expires_at = Utc::now() - chrono::Duration::seconds(1);
        assert_eq!(credit_action(&[], &job, Utc::now()), Err("credit_expired"));
    }
    #[test]
    fn retry_never_overrides_an_explicit_unsupported_credit() {
        let job = job(2);
        let unknown = ExpiryCredit {
            credit_id: job.credit_id.clone(),
            status: "unknown".into(),
            expires_at: Some(job.expires_at),
        };
        assert_eq!(
            credit_action(&[unknown], &job, Utc::now()),
            Err("credit_ineligible")
        );
    }
    #[test]
    fn provider_fractional_expiry_matches_ledger_millisecond_precision() {
        let mut job = job(1);
        let provider_expiry = DateTime::parse_from_rfc3339("2030-01-01T00:00:00.123456Z")
            .expect("provider expiry")
            .with_timezone(&Utc);
        job.expires_at = DateTime::from_timestamp_millis(provider_expiry.timestamp_millis())
            .expect("ledger expiry");
        let credit = ExpiryCredit {
            credit_id: job.credit_id.clone(),
            status: "available".into(),
            expires_at: Some(provider_expiry),
        };
        let now = job.expires_at - chrono::Duration::minutes(1);
        assert_eq!(credit_action(&[credit.clone()], &job, now), Ok(CreditAction::Submit));
        let changed = ExpiryCredit {
            expires_at: Some(provider_expiry + chrono::Duration::milliseconds(1)),
            ..credit
        };
        assert_eq!(credit_action(&[changed], &job, now), Err("credit_ineligible"));
    }
    fn block_on<F: std::future::Future>(future: F) -> F::Output {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("runtime")
            .block_on(future)
    }
    #[test]
    fn missing_daemon_does_not_open_credentials_or_create_state() {
        block_on(async {
            let home = tempfile::tempdir().expect("home");
            let executor = ExpiryExecutor::new(
                home.path().to_path_buf(),
                AccountdClient::new(home.path().join("absent.sock")),
            )
            .expect("executor");
            assert_eq!(
                executor.account_operation("a", None, false).await,
                Err("daemon_unavailable")
            );
            assert!(!home.path().join("marathon").exists());
        });
    }
    #[test]
    fn daemon_home_mismatch_fails_before_reading_credentials() {
        block_on(async {
            use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
            let home = tempfile::tempdir().expect("home");
            let socket = home.path().join("daemon.sock");
            let listener = tokio::net::UnixListener::bind(&socket).expect("listener");
            let server = tokio::spawn(async move {
                let (stream, _) = listener.accept().await.expect("accept");
                let mut stream = BufReader::new(stream);
                let mut line = String::new();
                stream.read_line(&mut line).await.expect("request");
                let request: serde_json::Value = serde_json::from_str(&line).expect("json");
                let response = serde_json::json!({"version":1,"id":request["id"],"ok":true,
                    "result":{"enabled":true,"effective_enabled":true,"executor_available":true,
                        "codex_home":"/another/home","jobs":[],"next_attempt_at":null}});
                stream
                    .get_mut()
                    .write_all(format!("{response}\n").as_bytes())
                    .await
                    .expect("response");
            });
            let executor =
                ExpiryExecutor::new(home.path().to_path_buf(), AccountdClient::new(socket))
                    .expect("executor");
            assert_eq!(
                executor.account_operation("a", None, false).await,
                Err("daemon_home_mismatch")
            );
            assert!(!home.path().join("marathon").exists());
            server.await.expect("server");
        });
    }

    #[test]
    fn provider_timeout_missing_inventory_retry_sends_identical_key_and_credit() {
        block_on(async {
            use codex_http_client::{HttpClientFactory, OutboundProxyPolicy};
            use wiremock::matchers::{body_partial_json, method, path};
            use wiremock::{Mock, MockServer, ResponseTemplate};
            let server = MockServer::start().await;
            let mut job = job(1);
            let body = serde_json::json!({"redeem_request_id":job.idempotency_key,"credit_id":job.credit_id});
            Mock::given(method("POST"))
                .and(path("/api/codex/rate-limit-reset-credits/consume"))
                .and(body_partial_json(body.clone()))
                .respond_with(
                    ResponseTemplate::new(200)
                        .set_body_json(serde_json::json!({"code":"reset","windows_reset":1}))
                        .set_delay(Duration::from_millis(200)),
                )
                .expect(1)
                .mount(&server)
                .await;
            let client = Client::new(
                server.uri(),
                HttpClientFactory::new(OutboundProxyPolicy::ReqwestDefault),
            );
            assert!(
                tokio::time::timeout(
                    Duration::from_millis(50),
                    client.consume_rate_limit_reset_credit_by_id(
                        &job.idempotency_key,
                        &job.credit_id
                    )
                )
                .await
                .is_err()
            );
            server.reset().await;
            Mock::given(method("GET"))
                .and(path("/api/codex/rate-limit-reset-credits"))
                .respond_with(
                    ResponseTemplate::new(200)
                        .set_body_json(serde_json::json!({"credits":[],"available_count":5})),
                )
                .expect(1)
                .mount(&server)
                .await;
            Mock::given(method("POST"))
                .and(path("/api/codex/rate-limit-reset-credits/consume"))
                .and(body_partial_json(body))
                .respond_with(ResponseTemplate::new(200).set_body_json(
                    serde_json::json!({"code":"already_redeemed","windows_reset":0}),
                ))
                .expect(1)
                .mount(&server)
                .await;
            job.attempt = 2;
            let inventory = request(client.list_rate_limit_reset_credits())
                .await
                .expect("inventory");
            let credits: Vec<_> = inventory.credits.into_iter().map(map_credit).collect();
            assert_eq!(
                credit_action(&credits, &job, Utc::now()),
                Ok(CreditAction::Submit)
            );
            let response = request(
                client.consume_rate_limit_reset_credit_by_id(&job.idempotency_key, &job.credit_id),
            )
            .await
            .expect("replay");
            assert_eq!(map_outcome(response.code), ExpiryOutcome::AlreadyRedeemed);
        });
    }
}
