//! Durable metadata-only scheduling; credentials and provider calls stay native.
use super::{
    AccountdError, EventInput, EventKind, EventStore, JobState, insert_event, validate_identifier,
};
use chrono::{DateTime, Utc};
use codexmarathon_accountd_client::*;
use sqlx::Row;
use std::sync::Arc;
use tokio::sync::Mutex;
use uuid::Uuid;

#[derive(Clone)]
pub(crate) struct ExpiryStore {
    store: EventStore,
    gate: Arc<Mutex<()>>,
}

impl ExpiryStore {
    pub(crate) async fn open(store: EventStore) -> Result<Self, AccountdError> {
        let this = Self {
            store,
            gate: Arc::new(Mutex::new(())),
        };
        let mut tx = this.store.pool().await?.begin().await?;
        sqlx::query("CREATE TABLE IF NOT EXISTS accountd_expiry_schema(singleton INTEGER PRIMARY KEY CHECK(singleton=1), version INTEGER NOT NULL)").execute(&mut *tx).await?;
        sqlx::query("INSERT OR IGNORE INTO accountd_expiry_schema VALUES(1,1)")
            .execute(&mut *tx)
            .await?;
        let version: i64 =
            sqlx::query_scalar("SELECT version FROM accountd_expiry_schema WHERE singleton=1")
                .fetch_one(&mut *tx)
                .await?;
        if version != 1 {
            return Err(AccountdError::CorruptState);
        }
        sqlx::query("CREATE TABLE IF NOT EXISTS accountd_expiry_settings(singleton INTEGER PRIMARY KEY CHECK(singleton=1), enabled INTEGER NOT NULL DEFAULT 0 CHECK(enabled IN (0,1)), heartbeat_ms INTEGER)").execute(&mut *tx).await?;
        sqlx::query(
            "INSERT OR IGNORE INTO accountd_expiry_settings(singleton,enabled) VALUES(1,0)",
        )
        .execute(&mut *tx)
        .await?;
        sqlx::query("CREATE TABLE IF NOT EXISTS accountd_expiry_observations(account_id TEXT PRIMARY KEY, observed_ms INTEGER NOT NULL)").execute(&mut *tx).await?;
        sqlx::query("CREATE TABLE IF NOT EXISTS accountd_expiry_jobs(job_id TEXT PRIMARY KEY, account_id TEXT NOT NULL, credit_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, expires_ms INTEGER NOT NULL, observed_ms INTEGER NOT NULL, available INTEGER NOT NULL, state TEXT NOT NULL, attempt INTEGER NOT NULL DEFAULT 0, run_after_ms INTEGER NOT NULL, lease_token TEXT, lease_until_ms INTEGER, diagnostic_code TEXT, UNIQUE(account_id,credit_id))").execute(&mut *tx).await?;
        sqlx::query("CREATE INDEX IF NOT EXISTS accountd_expiry_due ON accountd_expiry_jobs(state,run_after_ms)").execute(&mut *tx).await?;
        tx.commit().await?;
        Ok(this)
    }
    pub(crate) async fn set(&self, enabled: bool) -> Result<(), AccountdError> {
        let _guard = self.gate.lock().await;
        let mut tx = self.store.pool().await?.begin().await?;
        let changed = sqlx::query(
            "UPDATE accountd_expiry_settings SET enabled=? WHERE singleton=1 AND enabled!=?",
        )
        .bind(enabled)
        .bind(enabled)
        .execute(&mut *tx)
        .await?
        .rows_affected();
        if changed != 0 {
            insert_event(
                &mut tx,
                &EventInput {
                    diagnostic_code: Some(
                        if enabled {
                            "expiry_policy_enabled"
                        } else {
                            "expiry_policy_disabled"
                        }
                        .into(),
                    ),
                    ..EventInput::now(if enabled {
                        EventKind::JobQueued
                    } else {
                        EventKind::JobCancelled
                    })
                },
            )
            .await?;
        }
        tx.commit().await?;
        Ok(())
    }
    pub(crate) async fn heartbeat(&self, now: DateTime<Utc>) -> Result<(), AccountdError> {
        sqlx::query("UPDATE accountd_expiry_settings SET heartbeat_ms=? WHERE singleton=1")
            .bind(now.timestamp_millis())
            .execute(self.store.pool().await?)
            .await?;
        Ok(())
    }
    pub(crate) async fn observe(
        &self,
        observation: ExpiryObservation,
        now: DateTime<Utc>,
    ) -> Result<(), AccountdError> {
        validate_identifier(&observation.account_id, 256)?;
        if observation.observed_at > now {
            return Err(AccountdError::InvalidRequest);
        }
        let Some(credits) = observation.credits else {
            return Ok(());
        };
        let mut seen = std::collections::BTreeSet::new();
        for credit in &credits {
            validate_identifier(&credit.credit_id, 256)?;
            if !seen.insert(&credit.credit_id) {
                return Err(AccountdError::InvalidRequest);
            }
        }
        let _guard = self.gate.lock().await;
        let mut tx = self.store.pool().await?.begin().await?;
        let previous: Option<i64> = sqlx::query_scalar(
            "SELECT observed_ms FROM accountd_expiry_observations WHERE account_id=?",
        )
        .bind(&observation.account_id)
        .fetch_optional(&mut *tx)
        .await?;
        let stamp = observation.observed_at.timestamp_millis();
        if previous.is_some_and(|previous| stamp < previous) {
            return Ok(());
        }
        sqlx::query("INSERT INTO accountd_expiry_observations VALUES(?,?) ON CONFLICT(account_id) DO UPDATE SET observed_ms=excluded.observed_ms").bind(&observation.account_id).bind(stamp).execute(&mut *tx).await?;
        // Absence in capped inventories is never proof of redemption. Only explicit
        // per-credit status changes invalidate availability; incomplete reads do nothing.
        for credit in credits {
            if matches!(credit.status.as_str(), "redeemed" | "consumed") {
                let completed = sqlx::query("SELECT job_id,attempt,lease_token FROM accountd_expiry_jobs WHERE account_id=? AND credit_id=? AND state!='succeeded'").bind(&observation.account_id).bind(&credit.credit_id).fetch_optional(&mut *tx).await?;
                if let Some(row) = completed {
                    let job_id: String = row.try_get("job_id")?;
                    sqlx::query("UPDATE accountd_expiry_jobs SET state='succeeded',available=0,observed_ms=?,lease_until_ms=NULL,diagnostic_code='inventory_redeemed' WHERE job_id=?").bind(stamp).bind(&job_id).execute(&mut *tx).await?;
                    insert_event(
                        &mut tx,
                        &EventInput {
                            account_id: Some(observation.account_id.clone()),
                            job_id: Some(job_id),
                            state: Some(JobState::Succeeded),
                            attempt: Some(row.try_get("attempt")?),
                            diagnostic_code: Some("inventory_redeemed".into()),
                            occurred_at: now,
                            ..EventInput::now(EventKind::JobSucceeded)
                        },
                    )
                    .await?;
                }
                continue;
            }
            let available = credit.status == "available" && credit.expires_at.is_some();
            if let Some(expires) = credit.expires_at {
                let job_id = Uuid::new_v4().to_string();
                sqlx::query("INSERT INTO accountd_expiry_jobs(job_id,account_id,credit_id,idempotency_key,expires_ms,observed_ms,available,state,run_after_ms) VALUES(?,?,?,?,?,?,?, 'queued',?) ON CONFLICT(account_id,credit_id) DO UPDATE SET available=excluded.available,observed_ms=excluded.observed_ms,expires_ms=excluded.expires_ms,run_after_ms=CASE WHEN accountd_expiry_jobs.attempt=0 THEN excluded.run_after_ms ELSE accountd_expiry_jobs.run_after_ms END")
                    .bind(&job_id).bind(&observation.account_id).bind(&credit.credit_id).bind(Uuid::new_v4().to_string()).bind(expires.timestamp_millis()).bind(stamp).bind(available).bind(expires.timestamp_millis().saturating_sub(900_000)).execute(&mut *tx).await?;
            } else {
                sqlx::query("UPDATE accountd_expiry_jobs SET available=0 WHERE account_id=? AND credit_id=?").bind(&observation.account_id).bind(&credit.credit_id).execute(&mut *tx).await?;
            }
        }
        tx.commit().await?;
        Ok(())
    }
    pub(crate) async fn claim(
        &self,
        eligible: &[String],
        master: bool,
        now: DateTime<Utc>,
    ) -> Result<Option<ExpiryJob>, AccountdError> {
        if !master {
            return Ok(None);
        }
        let _guard = self.gate.lock().await;
        let mut tx = self.store.pool().await?.begin().await?;
        // Write first: acquire SQLite's writer lock before selecting a candidate.
        sqlx::query("UPDATE accountd_expiry_settings SET enabled=enabled WHERE singleton=1")
            .execute(&mut *tx)
            .await?;
        let settings = sqlx::query("SELECT CASE WHEN typeof(enabled)='integer' AND enabled=1 THEN 1 ELSE 0 END AS enabled,heartbeat_ms FROM accountd_expiry_settings WHERE singleton=1").fetch_one(&mut *tx).await?;
        let heartbeat: Option<i64> = settings.try_get("heartbeat_ms")?;
        let ms = now.timestamp_millis();
        if settings.try_get::<i64, _>("enabled")? != 1
            || !heartbeat.is_some_and(|t| t <= ms && ms - t <= 60_000)
        {
            return Ok(None);
        }
        let rows = sqlx::query("SELECT j.* FROM accountd_expiry_jobs j JOIN accountd_expiry_observations o USING(account_id) WHERE j.available=1 AND j.state IN ('queued','retry_wait','running') AND j.expires_ms>? AND j.run_after_ms<=? AND ((j.attempt=0 AND j.observed_ms<=? AND j.observed_ms>=?) OR (j.attempt>0 AND o.observed_ms<=? AND o.observed_ms>=?)) AND (j.state!='running' OR j.lease_until_ms<=?) ORDER BY j.run_after_ms,j.job_id")
            .bind(ms).bind(ms).bind(ms).bind(ms.saturating_sub(60_000)).bind(ms).bind(ms.saturating_sub(60_000)).bind(ms).fetch_all(&mut *tx).await?;
        for row in rows {
            let account_id: String = row.try_get("account_id")?;
            if !eligible.contains(&account_id) {
                continue;
            }
            let token = Uuid::new_v4().to_string();
            let job_id: String = row.try_get("job_id")?;
            let attempt: i64 = row.try_get::<i64, _>("attempt")?.saturating_add(1);
            sqlx::query("UPDATE accountd_expiry_jobs SET state='running',attempt=?,lease_token=?,lease_until_ms=? WHERE job_id=?").bind(attempt).bind(&token).bind(ms.saturating_add(90_000)).bind(&job_id).execute(&mut *tx).await?;
            insert_event(
                &mut tx,
                &EventInput {
                    account_id: Some(account_id.clone()),
                    job_id: Some(job_id.clone()),
                    state: Some(JobState::Running),
                    attempt: Some(attempt),
                    occurred_at: now,
                    ..EventInput::now(EventKind::JobStarted)
                },
            )
            .await?;
            let job = ExpiryJob {
                job_id,
                account_id,
                credit_id: row.try_get("credit_id")?,
                idempotency_key: row.try_get("idempotency_key")?,
                lease_token: token,
                expires_at: timestamp(row.try_get("expires_ms")?)?,
                attempt,
            };
            tx.commit().await?;
            return Ok(Some(job));
        }
        tx.commit().await?;
        Ok(None)
    }
    pub(crate) async fn complete(
        &self,
        completion: ExpiryCompletion,
        now: DateTime<Utc>,
    ) -> Result<(), AccountdError> {
        validate_identifier(&completion.job_id, 256)?;
        validate_identifier(&completion.lease_token, 256)?;
        if let Some(code) = &completion.diagnostic_code {
            // Diagnostic codes are symbols, never provider messages or response bodies.
            if code.len() > 64
                || code.is_empty()
                || !code.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'_')
            {
                return Err(AccountdError::InvalidRequest);
            }
        }
        let _guard = self.gate.lock().await;
        let mut tx = self.store.pool().await?.begin().await?;
        sqlx::query("UPDATE accountd_expiry_settings SET enabled=enabled WHERE singleton=1")
            .execute(&mut *tx)
            .await?;
        let row = sqlx::query(
            "SELECT account_id,attempt,state,lease_token FROM accountd_expiry_jobs WHERE job_id=?",
        )
        .bind(&completion.job_id)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or(AccountdError::InvalidRequest)?;
        if row.try_get::<Option<String>, _>("lease_token")?.as_deref()
            != Some(&completion.lease_token)
        {
            return Err(AccountdError::InvalidRequest);
        }
        if matches!(
            row.try_get::<String, _>("state")?.as_str(),
            "succeeded" | "retry_wait"
        ) {
            return Ok(());
        }
        if row.try_get::<String, _>("state")? != "running" {
            return Err(AccountdError::InvalidRequest);
        }
        let success = matches!(
            completion.outcome,
            ExpiryOutcome::Reset | ExpiryOutcome::AlreadyRedeemed
        );
        let attempt: i64 = row.try_get("attempt")?;
        let delay = retry_seconds(attempt);
        let (state, kind) = if success {
            ("succeeded", EventKind::JobSucceeded)
        } else {
            ("retry_wait", EventKind::JobRetryWait)
        };
        sqlx::query("UPDATE accountd_expiry_jobs SET state=?,run_after_ms=?,lease_until_ms=NULL,diagnostic_code=? WHERE job_id=? AND lease_token=?")
            .bind(state).bind(now.timestamp_millis().saturating_add(delay*1000)).bind(&completion.diagnostic_code).bind(&completion.job_id).bind(&completion.lease_token).execute(&mut *tx).await?;
        insert_event(
            &mut tx,
            &EventInput {
                account_id: Some(row.try_get("account_id")?),
                job_id: Some(completion.job_id),
                state: Some(if success {
                    JobState::Succeeded
                } else {
                    JobState::RetryWait
                }),
                attempt: Some(attempt),
                diagnostic_code: completion.diagnostic_code,
                occurred_at: now,
                ..EventInput::now(kind)
            },
        )
        .await?;
        tx.commit().await?;
        Ok(())
    }
    pub(crate) async fn status(
        &self,
        master: bool,
        codex_home: Option<String>,
        now: DateTime<Utc>,
    ) -> Result<ExpiryStatus, AccountdError> {
        let pool = self.store.pool().await?;
        let settings = sqlx::query("SELECT CASE WHEN typeof(enabled)='integer' AND enabled=1 THEN 1 ELSE 0 END AS enabled,heartbeat_ms FROM accountd_expiry_settings WHERE singleton=1").fetch_one(pool).await?;
        let enabled = settings.try_get::<i64, _>("enabled")? == 1;
        let ms = now.timestamp_millis();
        let executor_available = settings
            .try_get::<Option<i64>, _>("heartbeat_ms")?
            .is_some_and(|t| t <= ms && ms - t <= 60_000);
        // Live work must remain visible when historical expired rows exceed the
        // bounded status page. Within each class, put the next attempt first.
        let rows = sqlx::query("SELECT * FROM accountd_expiry_jobs ORDER BY CASE WHEN state='succeeded' THEN 3 WHEN expires_ms<=? THEN 2 WHEN available=1 THEN 0 ELSE 1 END,CASE WHEN state='running' THEN COALESCE(lease_until_ms,?) ELSE run_after_ms END,expires_ms,job_id LIMIT 1001")
            .bind(ms)
            .bind(ms)
            .fetch_all(pool)
            .await?;
        let jobs_truncated = rows.len() > 1000;
        let mut jobs = Vec::new();
        for row in rows.into_iter().take(1000) {
            let stored: String = row.try_get("state")?;
            let expires: i64 = row.try_get("expires_ms")?;
            let pending = stored != "succeeded";
            let state = if !pending {
                stored
            } else if expires <= ms {
                "expired".into()
            } else if !enabled || !master {
                "paused".into()
            } else if !row.try_get::<bool, _>("available")? {
                "unavailable".into()
            } else {
                stored
            };
            let next_attempt_at = if matches!(state.as_str(), "queued" | "retry_wait" | "running") {
                Some(timestamp(if state == "running" {
                    row.try_get::<Option<i64>, _>("lease_until_ms")?
                        .unwrap_or(ms)
                } else {
                    row.try_get("run_after_ms")?
                })?)
            } else {
                None
            };
            jobs.push(ExpiryJobStatus {
                job_id: row.try_get("job_id")?,
                account_id: row.try_get("account_id")?,
                credit_id: row.try_get("credit_id")?,
                state,
                diagnostic_code: row.try_get("diagnostic_code")?,
                expires_at: timestamp(expires)?,
                next_attempt_at,
                attempt: row.try_get("attempt")?,
            });
        }
        // Compute across the full store, independently of status truncation.
        let next_attempt_at = if enabled && master {
            let next_ms: Option<i64> = sqlx::query_scalar("SELECT MIN(CASE WHEN state='running' THEN COALESCE(lease_until_ms,?) ELSE run_after_ms END) FROM accountd_expiry_jobs WHERE state IN ('queued','retry_wait','running') AND available=1 AND expires_ms>?")
                .bind(ms)
                .bind(ms)
                .fetch_one(pool)
                .await?;
            next_ms.map(timestamp).transpose()?
        } else {
            None
        };
        Ok(ExpiryStatus {
            codex_home,
            jobs_truncated,
            enabled,
            effective_enabled: enabled && master,
            executor_available,
            jobs,
            next_attempt_at,
        })
    }
}
fn timestamp(ms: i64) -> Result<DateTime<Utc>, AccountdError> {
    DateTime::from_timestamp_millis(ms).ok_or(AccountdError::CorruptState)
}
fn retry_seconds(attempt: i64) -> i64 {
    match attempt {
        i64::MIN..=1 => 5,
        2 => 10,
        3 => 20,
        _ => 30,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::Duration;
    use tempfile::tempdir;
    fn run<F: std::future::Future>(future: F) -> F::Output {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("runtime")
            .block_on(future)
    }
    async fn store(path: &std::path::Path) -> ExpiryStore {
        // TempDir inherits the process umask. Match accountd's owner-only
        // database directory contract explicitly, including on AWS runners.
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(
                path.parent().expect("test database directory"),
                std::fs::Permissions::from_mode(0o700),
            )
            .expect("private test database directory");
        }
        ExpiryStore::open(EventStore::open(path).await.expect("events"))
            .await
            .expect("expiry")
    }
    fn clock() -> DateTime<Utc> {
        DateTime::from_timestamp(1_700_000_000, 0).expect("clock")
    }
    fn observation(now: DateTime<Utc>, expires: DateTime<Utc>) -> ExpiryObservation {
        ExpiryObservation {
            account_id: "account".into(),
            observed_at: now,
            credits: Some(vec![ExpiryCredit {
                credit_id: "credit".into(),
                status: "available".into(),
                expires_at: Some(expires),
            }]),
        }
    }
    async fn ready(store: &ExpiryStore, now: DateTime<Utc>, expires: DateTime<Utc>) {
        store.set(true).await.expect("enabled");
        store.heartbeat(now).await.expect("heartbeat");
        store
            .observe(observation(now, expires), now)
            .await
            .expect("observation");
    }
    async fn claim(store: &ExpiryStore, now: DateTime<Utc>) -> Option<ExpiryJob> {
        store
            .claim(&["account".into()], true, now)
            .await
            .expect("claim")
    }
    fn completion(job: &ExpiryJob, outcome: ExpiryOutcome) -> ExpiryCompletion {
        ExpiryCompletion {
            job_id: job.job_id.clone(),
            lease_token: job.lease_token.clone(),
            outcome,
            diagnostic_code: None,
        }
    }
    #[test]
    fn expired_history_cannot_hide_live_work_or_global_next_attempt() {
        run(async {
            let dir = tempdir().expect("dir");
            let store = store(&dir.path().join("quota.sqlite")).await;
            let now = clock();
            // Populate more historical rows than the status response can expose.
            sqlx::query("WITH RECURSIVE history(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM history WHERE n<1001) INSERT INTO accountd_expiry_jobs(job_id,account_id,credit_id,idempotency_key,expires_ms,observed_ms,available,state,run_after_ms) SELECT 'old-job-'||n,'account','old-credit-'||n,'old-key-'||n,?,?,1,'retry_wait',? FROM history")
                .bind((now-Duration::seconds(1)).timestamp_millis())
                .bind(now.timestamp_millis())
                .bind((now-Duration::seconds(5)).timestamp_millis())
                .execute(store.store.pool().await.expect("pool"))
                .await
                .expect("history");
            let expires = now + Duration::seconds(1000);
            ready(&store, now, expires).await;
            let status = store.status(true, None, now).await.expect("status");
            assert!(status.jobs_truncated);
            assert_eq!(status.jobs.len(), 1000);
            assert_eq!(status.jobs[0].credit_id, "credit");
            assert_eq!(status.jobs[0].state, "queued");
            assert_eq!(
                status.next_attempt_at,
                Some(expires - Duration::seconds(900))
            );
            store.set(false).await.expect("off");
            assert_eq!(
                store
                    .status(true, None, now)
                    .await
                    .expect("paused")
                    .next_attempt_at,
                None
            );
        })
    }

    #[test]
    fn default_off_bounds_and_master_gate() {
        run(async {
            let dir = tempdir().expect("dir");
            let store = store(&dir.path().join("quota.sqlite")).await;
            let now = clock();
            assert!(!store.status(true, None, now).await.expect("status").enabled);
            ready(&store, now, now + Duration::seconds(901)).await;
            assert!(claim(&store, now).await.is_none());
            assert!(
                store
                    .claim(&["account".into()], false, now + Duration::seconds(1))
                    .await
                    .expect("master")
                    .is_none()
            );
            assert!(
                store
                    .claim(&[], true, now + Duration::seconds(1))
                    .await
                    .expect("removed")
                    .is_none()
            );
            assert!(claim(&store, now + Duration::seconds(1)).await.is_some());
        })
    }
    #[test]
    fn incomplete_unknown_duplicates_future_and_stale_inventory() {
        run(async {
            let dir = tempdir().expect("dir");
            let store = store(&dir.path().join("quota.sqlite")).await;
            let now = clock();
            let expires = now + Duration::seconds(500);
            ready(&store, now, expires).await;
            store
                .observe(
                    ExpiryObservation {
                        account_id: "account".into(),
                        observed_at: now,
                        credits: None,
                    },
                    now,
                )
                .await
                .expect("incomplete");
            assert_eq!(
                store
                    .status(true, None, now)
                    .await
                    .expect("status")
                    .jobs
                    .len(),
                1
            );
            let mut duplicate = observation(now, expires);
            let extra = duplicate.credits.as_ref().expect("credits")[0].clone();
            duplicate.credits.as_mut().expect("credits").push(extra);
            assert!(store.observe(duplicate, now).await.is_err());
            assert!(
                store
                    .observe(observation(now + Duration::seconds(1), expires), now)
                    .await
                    .is_err()
            );
            let mut unknown = observation(now, expires);
            unknown.credits.as_mut().expect("credits")[0].status = "future_provider_status".into();
            store.observe(unknown, now).await.expect("unknown");
            assert!(claim(&store, now).await.is_none());
            store
                .observe(observation(now, expires), now)
                .await
                .expect("available");
            store
                .heartbeat(now + Duration::seconds(61))
                .await
                .expect("heartbeat");
            assert!(claim(&store, now + Duration::seconds(61)).await.is_none());
            store
                .observe(
                    ExpiryObservation {
                        account_id: "account".into(),
                        observed_at: now + Duration::seconds(61),
                        credits: None,
                    },
                    now + Duration::seconds(61),
                )
                .await
                .expect("null");
            assert!(claim(&store, now + Duration::seconds(61)).await.is_none());
        })
    }
    #[test]
    fn retries_have_no_limit_and_stop_at_expiry() {
        run(async {
            let dir = tempdir().expect("dir");
            let store = store(&dir.path().join("quota.sqlite")).await;
            let mut now = clock();
            let expires = now + Duration::seconds(900);
            ready(&store, now, expires).await;
            let mut key = None;
            for index in 1..=20 {
                store.heartbeat(now).await.expect("heartbeat");
                store
                    .observe(observation(now, expires), now)
                    .await
                    .expect("fresh");
                let job = claim(&store, now).await.expect("job");
                assert_eq!(job.attempt, index);
                if let Some(key) = &key {
                    assert_eq!(&job.idempotency_key, key)
                } else {
                    key = Some(job.idempotency_key.clone())
                };
                store
                    .complete(
                        completion(
                            &job,
                            if index % 2 == 0 {
                                ExpiryOutcome::NoCredit
                            } else {
                                ExpiryOutcome::NothingToReset
                            },
                        ),
                        now,
                    )
                    .await
                    .expect("complete");
                let delay = retry_seconds(index);
                assert!(
                    claim(&store, now + Duration::seconds(delay - 1))
                        .await
                        .is_none()
                );
                now += Duration::seconds(delay);
            }
            store.heartbeat(expires).await.expect("heartbeat");
            store
                .observe(observation(expires, expires), expires)
                .await
                .expect("fresh");
            assert!(claim(&store, expires).await.is_none());
        })
    }
    #[test]
    fn omitted_credit_only_replays_existing_attempt_and_explicit_redeemed_is_terminal() {
        run(async {
            let dir = tempdir().expect("dir");
            let store = store(&dir.path().join("quota.sqlite")).await;
            let now = clock();
            let expires = now + Duration::seconds(500);
            ready(&store, now, expires).await;
            let later = now + Duration::seconds(61);
            store.heartbeat(later).await.expect("heartbeat");
            store
                .observe(
                    ExpiryObservation {
                        account_id: "account".into(),
                        observed_at: later,
                        credits: Some(vec![]),
                    },
                    later,
                )
                .await
                .expect("capped");
            assert!(claim(&store, later).await.is_none()); // omitted credit isn't fresh for initial consume
            store
                .observe(observation(later, expires), later)
                .await
                .expect("fresh credit");
            let first = claim(&store, later).await.expect("first");
            store
                .complete(completion(&first, ExpiryOutcome::Retry), later)
                .await
                .expect("ambiguous");
            let replay_at = later + Duration::seconds(61);
            store.heartbeat(replay_at).await.expect("heartbeat");
            store
                .observe(
                    ExpiryObservation {
                        account_id: "account".into(),
                        observed_at: replay_at,
                        credits: Some(vec![]),
                    },
                    replay_at,
                )
                .await
                .expect("capped after consume");
            let replay = claim(&store, replay_at).await.expect("same-key replay");
            assert_eq!(first.idempotency_key, replay.idempotency_key);
            let mut redeemed = observation(replay_at, expires);
            redeemed.credits.as_mut().expect("credits")[0].status = "redeemed".into();
            store.observe(redeemed, replay_at).await.expect("redeemed");
            assert!(
                claim(&store, replay_at + Duration::seconds(91))
                    .await
                    .is_none()
            );
            assert_eq!(
                store
                    .status(true, None, replay_at)
                    .await
                    .expect("status")
                    .jobs[0]
                    .state,
                "succeeded"
            );
        })
    }
    #[test]
    fn restart_replays_key_fences_completion_and_preserves_consumption_when_off() {
        run(async {
            let dir = tempdir().expect("dir");
            let path = dir.path().join("quota.sqlite");
            let initial = store(&path).await;
            let now = clock();
            let expires = now + Duration::seconds(500);
            ready(&initial, now, expires).await;
            let first = claim(&initial, now).await.expect("first");
            let restarted = store(&path).await;
            assert!(
                claim(&restarted, now + Duration::seconds(30))
                    .await
                    .is_none()
            ); // generic scheduler recovery never touches lease
            let later = now + Duration::seconds(90);
            ready(&restarted, later, expires).await;
            let second = claim(&restarted, later).await.expect("replay");
            assert_eq!(first.idempotency_key, second.idempotency_key);
            assert_ne!(first.lease_token, second.lease_token);
            assert!(
                restarted
                    .complete(completion(&first, ExpiryOutcome::Reset), later)
                    .await
                    .is_err()
            );
            restarted.set(false).await.expect("off");
            assert!(claim(&restarted, later).await.is_none());
            restarted
                .complete(completion(&second, ExpiryOutcome::Reset), later)
                .await
                .expect("durable consumption while off");
            restarted
                .complete(completion(&second, ExpiryOutcome::Retry), later)
                .await
                .expect("late verification cannot undo success");
            ready(&restarted, later, expires).await;
            assert!(claim(&restarted, later).await.is_none());
            assert_eq!(
                restarted
                    .status(true, None, later)
                    .await
                    .expect("status")
                    .jobs[0]
                    .state,
                "succeeded"
            );
        })
    }
}

#[cfg(test)]
mod protocol_tests {
    use super::*;
    use crate::{AccountDaemon, PROTOCOL_VERSION, Request};
    use codexmarathon_runtime::{AccountRecord, FileAccountRegistry, QuotaSnapshotStore};
    use serde_json::{Value, json};
    use tempfile::tempdir;
    fn run<F: std::future::Future>(future: F) -> F::Output {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("runtime")
            .block_on(future)
    }
    fn request(method: &str, params: Value) -> Request {
        Request {
            version: PROTOCOL_VERSION,
            id: Some(json!(1)),
            method: method.into(),
            params,
        }
    }
    #[test]
    fn executor_can_report_while_native_registry_lock_is_held() {
        run(async {
            let directory = tempdir().expect("directory");
            let marathon = directory.path().join("marathon");
            std::fs::create_dir(&marathon).expect("marathon");
            let registry = Arc::new(FileAccountRegistry::new(marathon.join("accounts.json")));
            let lock = registry.lock_exclusive().expect("lock");
            let mut state = registry.state_locked(&lock).expect("state");
            state.accounts.insert(
                "account".into(),
                AccountRecord::new("account", "account").expect("account"),
            );
            registry.replace_locked(&lock, state).expect("publish");
            let path = marathon.join("quota.sqlite");
            let quota = Arc::new(QuotaSnapshotStore::open(&path).await.expect("quota"));
            let events = EventStore::open(&path).await.expect("events");
            let mut daemon =
                AccountDaemon::from_stores(quota, events, std::time::Duration::from_secs(2))
                    .await
                    .expect("daemon");
            daemon.registry = Some(registry.clone());
            let home = directory
                .path()
                .canonicalize()
                .expect("home")
                .to_string_lossy()
                .into_owned();
            assert!(
                daemon
                    .handle_request(request(
                        "auto_reset_expiry_set",
                        json!({"enabled":true,"expected_codex_home":home})
                    ))
                    .await
                    .ok
            );
            assert!(
                daemon
                    .handle_request(request("auto_reset_expiry_status", Value::Null))
                    .await
                    .ok
            );
            assert!(
                daemon
                    .handle_request(request("expiry_executor_heartbeat", Value::Null))
                    .await
                    .ok
            );
            let now = Utc::now();
            let observation = ExpiryObservation {
                account_id: "account".into(),
                observed_at: now,
                credits: Some(vec![ExpiryCredit {
                    credit_id: "credit".into(),
                    status: "available".into(),
                    expires_at: Some(now + chrono::Duration::seconds(500)),
                }]),
            };
            assert!(
                daemon
                    .handle_request(request(
                        "expiry_observe",
                        serde_json::to_value(observation).expect("observation")
                    ))
                    .await
                    .ok
            );
            let response = daemon
                .handle_request(request("expiry_claim", Value::Null))
                .await;
            assert!(response.ok);
            let job: ExpiryJob =
                serde_json::from_value(response.result.expect("result")).expect("job");
            assert!(
                daemon
                    .handle_request(request(
                        "expiry_complete",
                        serde_json::to_value(ExpiryCompletion {
                            job_id: job.job_id,
                            lease_token: job.lease_token,
                            outcome: ExpiryOutcome::Reset,
                            diagnostic_code: None
                        })
                        .expect("completion")
                    ))
                    .await
                    .ok
            );
            drop(lock);
        })
    }
    #[test]
    fn settings_identity_mismatch_cannot_enable_daemon() {
        run(async {
            let directory = tempdir().expect("directory");
            let marathon = directory.path().join("marathon");
            std::fs::create_dir(&marathon).expect("marathon");
            let daemon = AccountDaemon::open(
                marathon.join("quota.sqlite"),
                std::time::Duration::from_secs(2),
            )
            .await
            .expect("daemon");
            let other = tempdir().expect("other");
            let response = daemon
                .handle_request(request(
                    "auto_reset_expiry_set",
                    json!({"enabled":true,"expected_codex_home":other.path().to_string_lossy()}),
                ))
                .await;
            assert!(!response.ok);
            assert!(
                !daemon
                    .expiry
                    .status(true, None, Utc::now())
                    .await
                    .expect("status")
                    .enabled
            );
        })
    }
}
