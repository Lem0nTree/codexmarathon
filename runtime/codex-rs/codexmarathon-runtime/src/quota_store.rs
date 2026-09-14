//! Durable, metadata-only quota snapshots.
//!
//! The store contains only the provider-neutral fields already represented by
//! [`AccountTelemetry`].  It deliberately does not have a JSON column or an
//! opaque provider payload column: credentials, prompts, responses, and other
//! provider documents cannot be written through this API.  Each account is
//! replaced in one SQLite transaction, so readers observe either the previous
//! complete snapshot or the new complete snapshot.

#![expect(
    clippy::disallowed_methods,
    reason = "the quota store owns its private SQLite connection configuration"
)]

use crate::accounts::validate_account_id;
use crate::auto_reset::QuotaResetCapability;
use crate::errors::{DomainError, DomainResult};
use crate::persistence::{ensure_private_dir, reject_unsafe_file, set_private_permissions};
use crate::telemetry::{AccountTelemetry, Freshness, LimitTelemetry, UsageWindow, WindowKind};
use chrono::{DateTime, Duration, Utc};
use sqlx::sqlite::{SqliteConnectOptions, SqliteJournalMode, SqlitePoolOptions, SqliteSynchronous};
use sqlx::{ConnectOptions, Row, Sqlite, SqlitePool, Transaction};
use std::collections::{BTreeMap, BTreeSet};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration as StdDuration;
use tokio::sync::OnceCell;

/// Filename used below the Marathon state directory.
pub const QUOTA_DB_FILENAME: &str = "quota.sqlite";

/// Current schema version for the metadata-only quota database.
pub const QUOTA_SCHEMA_VERSION: i64 = 1;

const SQLITE_BUSY_TIMEOUT: StdDuration = StdDuration::from_secs(5);
const MAX_METADATA_TEXT_BYTES: usize = 4096;
const MAX_LIMIT_KEY_BYTES: usize = 512;

const CREATE_SCHEMA_TABLE: &str = r#"
CREATE TABLE IF NOT EXISTS quota_schema (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    version INTEGER NOT NULL CHECK (version > 0)
)
"#;

const CREATE_SNAPSHOTS_TABLE: &str = r#"
CREATE TABLE IF NOT EXISTS quota_snapshots (
    account_id TEXT PRIMARY KEY NOT NULL,
    observed_at_ms INTEGER NOT NULL,
    usable INTEGER NOT NULL CHECK (usable IN (0, 1)),
    reset_available INTEGER NOT NULL CHECK (reset_available IN (0, 1)),
    reset_credit_id TEXT,
    source TEXT NOT NULL
)
"#;

const CREATE_LIMITS_TABLE: &str = r#"
CREATE TABLE IF NOT EXISTS quota_limits (
    account_id TEXT NOT NULL,
    limit_key TEXT NOT NULL,
    limit_id TEXT NOT NULL,
    limit_name TEXT NOT NULL,
    plan_type TEXT NOT NULL,
    PRIMARY KEY (account_id, limit_key),
    FOREIGN KEY (account_id) REFERENCES quota_snapshots(account_id)
        ON DELETE CASCADE
)
"#;

const CREATE_WINDOWS_TABLE: &str = r#"
CREATE TABLE IF NOT EXISTS quota_windows (
    account_id TEXT NOT NULL,
    limit_key TEXT NOT NULL,
    window_kind TEXT NOT NULL CHECK (window_kind IN ('primary', 'secondary')),
    used_percent REAL NOT NULL,
    window_duration_mins INTEGER,
    resets_at_ms INTEGER,
    observed_at_ms INTEGER NOT NULL,
    freshness TEXT NOT NULL CHECK (freshness IN ('fresh', 'stale')),
    PRIMARY KEY (account_id, limit_key, window_kind),
    FOREIGN KEY (account_id, limit_key)
        REFERENCES quota_limits(account_id, limit_key)
        ON DELETE CASCADE
)
"#;

/// Owner-private, SQLite-backed quota snapshots.
///
/// Construction is synchronous and does not touch the filesystem.  Call
/// [`Self::initialize`] explicitly at startup, or use [`Self::open`] when an
/// asynchronously opened store is more convenient.  Lazy initialization is
/// guarded by a process-local [`OnceCell`] and the database itself uses WAL
/// plus a busy timeout for independent Marathon processes.
#[derive(Clone)]
pub struct QuotaSnapshotStore {
    path: PathBuf,
    pool: Arc<OnceCell<SqlitePool>>,
}

impl QuotaSnapshotStore {
    /// Construct a store without creating its directory or database.
    pub fn new(path: impl Into<PathBuf>) -> Self {
        Self {
            path: path.into(),
            pool: Arc::new(OnceCell::new()),
        }
    }

    /// Open and initialize a quota store in one asynchronous operation.
    pub async fn open(path: impl Into<PathBuf>) -> DomainResult<Self> {
        let store = Self::new(path);
        store.initialize().await?;
        Ok(store)
    }

    /// Return the database path without exposing any stored values.
    pub fn path(&self) -> &Path {
        &self.path
    }

    /// Initialize the private SQLite database and schema exactly once per
    /// store instance. Distinct processes safely race through SQLite's WAL and
    /// busy-timeout configuration.
    pub async fn initialize(&self) -> DomainResult<()> {
        let path = self.path.clone();
        self.pool
            .get_or_try_init(|| async move { open_pool(&path).await })
            .await
            .map(|_| ())
    }

    /// Atomically replace one account's complete typed snapshot.
    pub async fn upsert(&self, snapshot: &AccountTelemetry) -> DomainResult<()> {
        validate_snapshot(snapshot)?;
        let pool = self.pool().await?;
        let mut transaction = pool.begin().await?;

        let snapshot_write = sqlx::query(
            r#"
INSERT INTO quota_snapshots (
    account_id,
    observed_at_ms,
    usable,
    reset_available,
    reset_credit_id,
    source
)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id) DO UPDATE SET
    observed_at_ms = excluded.observed_at_ms,
    usable = excluded.usable,
    reset_available = excluded.reset_available,
    reset_credit_id = excluded.reset_credit_id,
    source = excluded.source
WHERE quota_snapshots.observed_at_ms <= excluded.observed_at_ms
            "#,
        )
        .bind(&snapshot.account_id)
        .bind(snapshot.observed_at.timestamp_millis())
        .bind(bool_as_sqlite(snapshot.usable))
        .bind(reset_available(&snapshot.reset_capability))
        .bind(reset_credit_id(&snapshot.reset_capability))
        .bind(&snapshot.source)
        .execute(&mut *transaction)
        .await?;

        // Fire-and-forget producers may finish out of order. A stale writer
        // must not delete the children of the newer snapshot it failed to win.
        if snapshot_write.rows_affected() == 0 {
            transaction.rollback().await?;
            return Ok(());
        }

        // A snapshot is a replacement, not a patch. Removing children first
        // also makes sparse snapshots faithfully replace older full ones.
        sqlx::query("DELETE FROM quota_windows WHERE account_id = ?")
            .bind(&snapshot.account_id)
            .execute(&mut *transaction)
            .await?;
        sqlx::query("DELETE FROM quota_limits WHERE account_id = ?")
            .bind(&snapshot.account_id)
            .execute(&mut *transaction)
            .await?;

        for (limit_key, limit) in &snapshot.limits {
            sqlx::query(
                r#"
INSERT INTO quota_limits (
    account_id,
    limit_key,
    limit_id,
    limit_name,
    plan_type
)
VALUES (?, ?, ?, ?, ?)
                "#,
            )
            .bind(&snapshot.account_id)
            .bind(limit_key)
            .bind(&limit.limit_id)
            .bind(&limit.limit_name)
            .bind(&limit.plan_type)
            .execute(&mut *transaction)
            .await?;

            for window in &limit.windows {
                sqlx::query(
                    r#"
INSERT INTO quota_windows (
    account_id,
    limit_key,
    window_kind,
    used_percent,
    window_duration_mins,
    resets_at_ms,
    observed_at_ms,
    freshness
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                    "#,
                )
                .bind(&snapshot.account_id)
                .bind(limit_key)
                .bind(window_kind_name(window.kind))
                .bind(window.used_percent)
                .bind(window.window_duration_mins.map(u64_to_i64).transpose()?)
                .bind(window.resets_at.map(|value| value.timestamp_millis()))
                .bind(window.observed_at.timestamp_millis())
                .bind(freshness_name(window.freshness))
                .execute(&mut *transaction)
                .await?;
            }
        }

        transaction.commit().await?;
        Ok(())
    }

    /// Alias for [`Self::upsert`] using storage-oriented terminology.
    pub async fn save(&self, snapshot: &AccountTelemetry) -> DomainResult<()> {
        self.upsert(snapshot).await
    }

    /// Read one snapshot and refresh its in-memory freshness at the current
    /// wall clock. Reading never rewrites the persisted provider observation.
    pub async fn read(&self, account_id: &str) -> DomainResult<Option<AccountTelemetry>> {
        self.read_at(account_id, Utc::now(), None).await
    }

    /// Read one snapshot while applying an explicit freshness clock and age
    /// bound. A non-positive age bound has the same unbounded meaning as the
    /// policy layer's [`UsageWindow::is_fresh_at`] helper.
    pub async fn read_at(
        &self,
        account_id: &str,
        now: DateTime<Utc>,
        max_age: Option<Duration>,
    ) -> DomainResult<Option<AccountTelemetry>> {
        validate_account_id(account_id)?;
        let pool = self.pool().await?;
        let mut transaction = pool.begin().await?;
        let snapshot = read_snapshot(&mut transaction, account_id).await?;
        transaction.commit().await?;
        Ok(snapshot.map(|mut snapshot| {
            refresh_read_freshness(&mut snapshot, now, max_age);
            snapshot
        }))
    }

    /// Alias for [`Self::read`].
    pub async fn load(&self, account_id: &str) -> DomainResult<Option<AccountTelemetry>> {
        self.read(account_id).await
    }

    /// Alias for [`Self::read_at`].
    pub async fn load_at(
        &self,
        account_id: &str,
        now: DateTime<Utc>,
        max_age: Option<Duration>,
    ) -> DomainResult<Option<AccountTelemetry>> {
        self.read_at(account_id, now, max_age).await
    }

    /// Read all snapshots in deterministic account-id order.
    pub async fn list(&self) -> DomainResult<Vec<AccountTelemetry>> {
        self.list_at(Utc::now(), None).await
    }

    /// Read all snapshots while applying an explicit freshness clock and age
    /// bound to every returned window.
    pub async fn list_at(
        &self,
        now: DateTime<Utc>,
        max_age: Option<Duration>,
    ) -> DomainResult<Vec<AccountTelemetry>> {
        let pool = self.pool().await?;
        let mut transaction = pool.begin().await?;
        let rows = sqlx::query("SELECT account_id FROM quota_snapshots ORDER BY account_id")
            .fetch_all(&mut *transaction)
            .await?;
        let mut snapshots = Vec::with_capacity(rows.len());
        for row in rows {
            let account_id: String = row.try_get("account_id")?;
            let snapshot = read_snapshot(&mut transaction, &account_id)
                .await?
                .ok_or(DomainError::CorruptQuotaStore)?;
            snapshots.push(snapshot);
        }
        transaction.commit().await?;
        for snapshot in &mut snapshots {
            refresh_read_freshness(snapshot, now, max_age);
        }
        Ok(snapshots)
    }

    /// Remove one account snapshot and its child rows in one transaction.
    /// Returns whether a snapshot was present.
    pub async fn delete(&self, account_id: &str) -> DomainResult<bool> {
        validate_account_id(account_id)?;
        let pool = self.pool().await?;
        let mut transaction = pool.begin().await?;
        let result = sqlx::query("DELETE FROM quota_snapshots WHERE account_id = ?")
            .bind(account_id)
            .execute(&mut *transaction)
            .await?;
        transaction.commit().await?;
        Ok(result.rows_affected() != 0)
    }

    /// Return the initialized pool for internal operations.
    async fn pool(&self) -> DomainResult<&SqlitePool> {
        self.initialize().await?;
        self.pool.get().ok_or(DomainError::CorruptQuotaStore)
    }
}

async fn open_pool(path: &Path) -> DomainResult<SqlitePool> {
    if path.as_os_str().is_empty() {
        return Err(DomainError::InvalidConfig("quota database path is empty"));
    }
    let parent = path.parent().ok_or(DomainError::UnsafePath)?;
    ensure_private_dir(parent)?;
    reject_unsafe_file(path)?;

    let options = SqliteConnectOptions::new()
        .filename(path)
        .create_if_missing(true)
        .journal_mode(SqliteJournalMode::Wal)
        .synchronous(SqliteSynchronous::Normal)
        .foreign_keys(true)
        .busy_timeout(SQLITE_BUSY_TIMEOUT)
        .log_statements(log::LevelFilter::Off);
    let pool = SqlitePoolOptions::new()
        .max_connections(4)
        .connect_with(options)
        .await?;

    if let Err(error) = set_private_permissions(path, false) {
        pool.close().await;
        return Err(error);
    }
    if let Err(error) = initialize_schema(&pool).await {
        pool.close().await;
        return Err(error);
    }
    // Schema creation may have created or touched the file after the first
    // permission pass. Apply the owner-only mode once more before returning.
    set_private_permissions(path, false)?;
    Ok(pool)
}

async fn initialize_schema(pool: &SqlitePool) -> DomainResult<()> {
    let mut transaction = pool.begin().await?;
    sqlx::query(CREATE_SCHEMA_TABLE)
        .execute(&mut *transaction)
        .await?;
    sqlx::query(CREATE_SNAPSHOTS_TABLE)
        .execute(&mut *transaction)
        .await?;
    sqlx::query(CREATE_LIMITS_TABLE)
        .execute(&mut *transaction)
        .await?;
    sqlx::query(CREATE_WINDOWS_TABLE)
        .execute(&mut *transaction)
        .await?;
    sqlx::query(
        "INSERT INTO quota_schema (singleton, version) VALUES (1, ?) ON CONFLICT(singleton) DO NOTHING",
    )
    .bind(QUOTA_SCHEMA_VERSION)
    .execute(&mut *transaction)
    .await?;
    let version: i64 = sqlx::query_scalar("SELECT version FROM quota_schema WHERE singleton = 1")
        .fetch_one(&mut *transaction)
        .await?;
    if version != QUOTA_SCHEMA_VERSION {
        return Err(DomainError::IncompatibleQuotaSchema);
    }
    transaction.commit().await?;
    Ok(())
}

async fn read_snapshot(
    transaction: &mut Transaction<'_, Sqlite>,
    account_id: &str,
) -> DomainResult<Option<AccountTelemetry>> {
    let Some(row) = sqlx::query(
        r#"
SELECT account_id, observed_at_ms, usable, reset_available, reset_credit_id, source
FROM quota_snapshots
WHERE account_id = ?
        "#,
    )
    .bind(account_id)
    .fetch_optional(&mut **transaction)
    .await?
    else {
        return Ok(None);
    };

    let account_id: String = row.try_get("account_id")?;
    let observed_at_ms: i64 = row.try_get("observed_at_ms")?;
    let usable: i64 = row.try_get("usable")?;
    let reset_available: i64 = row.try_get("reset_available")?;
    let reset_credit_id: Option<String> = row.try_get("reset_credit_id")?;
    let source: String = row.try_get("source")?;
    let observed_at = datetime_from_millis(observed_at_ms)?;
    let usable = bool_from_sqlite(usable)?;
    let reset_capability = reset_capability_from_sqlite(reset_available, reset_credit_id)?;

    let limit_rows = sqlx::query(
        r#"
SELECT limit_key, limit_id, limit_name, plan_type
FROM quota_limits
WHERE account_id = ?
ORDER BY limit_key
        "#,
    )
    .bind(&account_id)
    .fetch_all(&mut **transaction)
    .await?;
    let mut limits = BTreeMap::new();
    for row in limit_rows {
        let limit_key: String = row.try_get("limit_key")?;
        let limit = LimitTelemetry {
            limit_id: row.try_get("limit_id")?,
            limit_name: row.try_get("limit_name")?,
            plan_type: row.try_get("plan_type")?,
            windows: Vec::new(),
        };
        if limits.insert(limit_key, limit).is_some() {
            return Err(DomainError::CorruptQuotaStore);
        }
    }

    let window_rows = sqlx::query(
        r#"
SELECT limit_key,
       window_kind,
       used_percent,
       window_duration_mins,
       resets_at_ms,
       observed_at_ms,
       freshness
FROM quota_windows
WHERE account_id = ?
ORDER BY limit_key, window_kind
        "#,
    )
    .bind(&account_id)
    .fetch_all(&mut **transaction)
    .await?;
    for row in window_rows {
        let limit_key: String = row.try_get("limit_key")?;
        let window = UsageWindow {
            kind: window_kind_from_name(row.try_get::<String, _>("window_kind")?.as_str())?,
            used_percent: row.try_get("used_percent")?,
            window_duration_mins: row
                .try_get::<Option<i64>, _>("window_duration_mins")?
                .map(i64_to_u64)
                .transpose()?,
            resets_at: row
                .try_get::<Option<i64>, _>("resets_at_ms")?
                .map(datetime_from_millis)
                .transpose()?,
            observed_at: datetime_from_millis(row.try_get("observed_at_ms")?)?,
            freshness: freshness_from_name(row.try_get::<String, _>("freshness")?.as_str())?,
        };
        let limit = limits
            .get_mut(&limit_key)
            .ok_or(DomainError::CorruptQuotaStore)?;
        if limit
            .windows
            .iter()
            .any(|existing| existing.kind == window.kind)
        {
            return Err(DomainError::CorruptQuotaStore);
        }
        limit.windows.push(window);
    }

    Ok(Some(AccountTelemetry {
        account_id,
        limits,
        observed_at,
        usable,
        reset_capability,
        source,
    }))
}

fn validate_snapshot(snapshot: &AccountTelemetry) -> DomainResult<()> {
    validate_account_id(&snapshot.account_id)?;
    validate_metadata_text(&snapshot.source, MAX_METADATA_TEXT_BYTES)?;
    for (limit_key, limit) in &snapshot.limits {
        validate_metadata_text(limit_key, MAX_LIMIT_KEY_BYTES)?;
        validate_metadata_text(&limit.limit_id, MAX_METADATA_TEXT_BYTES)?;
        validate_metadata_text(&limit.limit_name, MAX_METADATA_TEXT_BYTES)?;
        validate_metadata_text(&limit.plan_type, MAX_METADATA_TEXT_BYTES)?;
        let mut kinds = BTreeSet::new();
        for window in &limit.windows {
            if !kinds.insert(window.kind) {
                return Err(DomainError::InvalidQuotaSnapshot(
                    "duplicate quota window kind",
                ));
            }
            if !window.used_percent.is_finite() || !(0.0..=100.0).contains(&window.used_percent) {
                return Err(DomainError::InvalidQuotaSnapshot(
                    "quota usage must be between zero and one hundred",
                ));
            }
            if window
                .window_duration_mins
                .is_some_and(|duration| duration > i64::MAX as u64)
            {
                return Err(DomainError::InvalidQuotaSnapshot(
                    "quota window duration is too large",
                ));
            }
        }
    }
    if let QuotaResetCapability::Available { credit_id } = &snapshot.reset_capability {
        if let Some(credit_id) = credit_id {
            validate_metadata_text(credit_id, MAX_METADATA_TEXT_BYTES)?;
        }
    }
    Ok(())
}

fn validate_metadata_text(value: &str, max_bytes: usize) -> DomainResult<()> {
    if value.len() > max_bytes
        || value
            .chars()
            .any(|character| character == '\0' || character.is_control())
    {
        return Err(DomainError::InvalidQuotaSnapshot("invalid metadata text"));
    }
    Ok(())
}

fn refresh_read_freshness(
    snapshot: &mut AccountTelemetry,
    now: DateTime<Utc>,
    max_age: Option<Duration>,
) {
    let account_is_stale = snapshot.observed_at > now
        || max_age
            .is_some_and(|bound| bound > Duration::zero() && now - snapshot.observed_at >= bound);
    for window in snapshot
        .limits
        .values_mut()
        .flat_map(|limit| limit.windows.iter_mut())
    {
        if account_is_stale || !window.is_fresh_at(now, max_age) {
            window.freshness = Freshness::Stale;
        }
    }
}

fn bool_as_sqlite(value: bool) -> i64 {
    i64::from(value)
}

fn bool_from_sqlite(value: i64) -> DomainResult<bool> {
    match value {
        0 => Ok(false),
        1 => Ok(true),
        _ => Err(DomainError::CorruptQuotaStore),
    }
}

fn reset_available(capability: &QuotaResetCapability) -> i64 {
    i64::from(capability.is_available())
}

fn reset_credit_id(capability: &QuotaResetCapability) -> Option<&str> {
    capability.credit_id()
}

fn reset_capability_from_sqlite(
    available: i64,
    credit_id: Option<String>,
) -> DomainResult<QuotaResetCapability> {
    match available {
        0 if credit_id.is_none() => Ok(QuotaResetCapability::Unavailable),
        1 => Ok(QuotaResetCapability::Available { credit_id }),
        _ => Err(DomainError::CorruptQuotaStore),
    }
}

fn window_kind_name(kind: WindowKind) -> &'static str {
    match kind {
        WindowKind::Primary => "primary",
        WindowKind::Secondary => "secondary",
    }
}

fn window_kind_from_name(value: &str) -> DomainResult<WindowKind> {
    match value {
        "primary" => Ok(WindowKind::Primary),
        "secondary" => Ok(WindowKind::Secondary),
        _ => Err(DomainError::CorruptQuotaStore),
    }
}

fn freshness_name(freshness: Freshness) -> &'static str {
    match freshness {
        Freshness::Fresh => "fresh",
        Freshness::Stale => "stale",
    }
}

fn freshness_from_name(value: &str) -> DomainResult<Freshness> {
    match value {
        "fresh" => Ok(Freshness::Fresh),
        "stale" => Ok(Freshness::Stale),
        _ => Err(DomainError::CorruptQuotaStore),
    }
}

fn datetime_from_millis(value: i64) -> DomainResult<DateTime<Utc>> {
    DateTime::from_timestamp_millis(value).ok_or(DomainError::CorruptQuotaStore)
}

fn u64_to_i64(value: u64) -> DomainResult<i64> {
    i64::try_from(value).map_err(|_| DomainError::InvalidQuotaSnapshot("integer is too large"))
}

fn i64_to_u64(value: i64) -> DomainResult<u64> {
    u64::try_from(value).map_err(|_| DomainError::CorruptQuotaStore)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::thread;
    use tempfile::tempdir;

    fn block_on<F: std::future::Future>(future: F) -> F::Output {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("test runtime")
            .block_on(future)
    }

    fn at(value: &str) -> DateTime<Utc> {
        DateTime::parse_from_rfc3339(value)
            .expect("timestamp")
            .with_timezone(&Utc)
    }

    fn snapshot(account_id: &str) -> AccountTelemetry {
        AccountTelemetry {
            account_id: account_id.to_string(),
            limits: BTreeMap::from([(
                "plan-window".to_string(),
                LimitTelemetry {
                    limit_id: "provider-limit-123".to_string(),
                    limit_name: "Codex plan".to_string(),
                    plan_type: "pro".to_string(),
                    windows: vec![
                        UsageWindow {
                            kind: WindowKind::Primary,
                            used_percent: 42.5,
                            window_duration_mins: Some(300),
                            resets_at: Some(at("2026-09-13T03:00:00Z")),
                            observed_at: at("2026-09-13T00:00:00Z"),
                            freshness: Freshness::Fresh,
                        },
                        UsageWindow {
                            kind: WindowKind::Secondary,
                            used_percent: 88.0,
                            window_duration_mins: None,
                            resets_at: Some(at("2026-09-14T00:00:00Z")),
                            observed_at: at("2026-09-13T00:00:00Z"),
                            freshness: Freshness::Fresh,
                        },
                    ],
                },
            )]),
            observed_at: at("2026-09-13T00:00:00Z"),
            usable: true,
            reset_capability: QuotaResetCapability::Available {
                credit_id: Some("credit-7".to_string()),
            },
            source: "provider-rate-limit".to_string(),
        }
    }

    #[test]
    fn atomic_upsert_round_trip_replaces_sparse_children() {
        block_on(async {
            let directory = tempdir().expect("temporary directory");
            let store = QuotaSnapshotStore::open(directory.path().join(QUOTA_DB_FILENAME))
                .await
                .expect("open store");
            let original = snapshot("account-a");
            store.upsert(&original).await.expect("upsert");
            let loaded = store
                .read_at("account-a", at("2026-09-13T01:00:00Z"), None)
                .await
                .expect("read")
                .expect("snapshot");
            assert_eq!(loaded, original);
            assert_eq!(loaded.limits["plan-window"].limit_id, "provider-limit-123");

            let mut sparse = original.clone();
            sparse.observed_at = at("2026-09-13T02:00:00Z");
            let limit = sparse.limits.get_mut("plan-window").expect("plan limit");
            limit.windows.truncate(1);
            limit.windows[0].used_percent = 51.0;
            store.upsert(&sparse).await.expect("sparse upsert");
            let loaded = store
                .read_at("account-a", at("2026-09-13T02:00:00Z"), None)
                .await
                .expect("read sparse")
                .expect("sparse snapshot");
            assert_eq!(loaded, sparse);
            assert_eq!(loaded.limits["plan-window"].windows.len(), 1);
        });
    }

    #[test]
    fn both_windows_and_freshness_are_read_honestly() {
        block_on(async {
            let directory = tempdir().expect("temporary directory");
            let store = QuotaSnapshotStore::open(directory.path().join(QUOTA_DB_FILENAME))
                .await
                .expect("open store");
            let original = snapshot("account-a");
            store.upsert(&original).await.expect("upsert");

            let loaded = store
                .read_at("account-a", at("2026-09-13T03:00:00Z"), None)
                .await
                .expect("read reset")
                .expect("snapshot");
            assert_eq!(
                loaded.limits["plan-window"].windows[0].freshness,
                Freshness::Stale
            );
            assert_eq!(
                loaded.limits["plan-window"].windows[1].freshness,
                Freshness::Fresh
            );

            let loaded = store
                .read_at(
                    "account-a",
                    at("2026-09-13T02:00:00Z"),
                    Some(Duration::hours(1)),
                )
                .await
                .expect("read old")
                .expect("snapshot");
            assert!(
                loaded
                    .limits
                    .values()
                    .flat_map(|limit| limit.windows.iter())
                    .all(|window| window.freshness == Freshness::Stale)
            );

            let explicitly_stale = AccountTelemetry {
                limits: BTreeMap::from([(
                    "plan-window".to_string(),
                    LimitTelemetry {
                        windows: vec![UsageWindow {
                            kind: WindowKind::Primary,
                            used_percent: 1.0,
                            window_duration_mins: None,
                            resets_at: None,
                            observed_at: at("2026-09-13T00:00:00Z"),
                            freshness: Freshness::Stale,
                        }],
                        ..original.limits["plan-window"].clone()
                    },
                )]),
                observed_at: at("2026-09-13T00:00:00Z"),
                ..original
            };
            store.upsert(&explicitly_stale).await.expect("stale upsert");
            let loaded = store
                .read_at("account-a", at("2026-09-13T00:01:00Z"), None)
                .await
                .expect("read stale")
                .expect("snapshot");
            assert_eq!(
                loaded.limits["plan-window"].windows[0].freshness,
                Freshness::Stale
            );
        });
    }

    #[test]
    fn older_observation_cannot_replace_newer_snapshot() {
        block_on(async {
            let directory = tempdir().expect("temporary directory");
            let store = QuotaSnapshotStore::open(directory.path().join(QUOTA_DB_FILENAME))
                .await
                .expect("open store");
            let mut newer = snapshot("account-a");
            newer.observed_at = at("2026-09-13T02:00:00Z");
            newer
                .limits
                .get_mut("plan-window")
                .expect("plan limit")
                .windows[0]
                .used_percent = 75.0;
            store.upsert(&newer).await.expect("newer upsert");

            let older = snapshot("account-a");
            store.upsert(&older).await.expect("stale upsert is ignored");
            let loaded = store
                .read_at("account-a", at("2026-09-13T02:01:00Z"), None)
                .await
                .expect("read")
                .expect("snapshot");
            assert_eq!(loaded.observed_at, newer.observed_at);
            assert_eq!(loaded.limits["plan-window"].windows[0].used_percent, 75.0);
        });
    }

    #[test]
    fn invalid_values_are_rejected_before_any_write() {
        block_on(async {
            let directory = tempdir().expect("temporary directory");
            let store = QuotaSnapshotStore::open(directory.path().join(QUOTA_DB_FILENAME))
                .await
                .expect("open store");
            let original = snapshot("account-a");
            store.upsert(&original).await.expect("upsert");
            let mut invalid = original.clone();
            invalid
                .limits
                .get_mut("plan-window")
                .expect("plan limit")
                .windows[0]
                .used_percent = f64::NAN;
            assert!(matches!(
                store.upsert(&invalid).await,
                Err(DomainError::InvalidQuotaSnapshot(_))
            ));
            invalid
                .limits
                .get_mut("plan-window")
                .expect("plan limit")
                .windows[0]
                .used_percent = 100.1;
            assert!(matches!(
                store.upsert(&invalid).await,
                Err(DomainError::InvalidQuotaSnapshot(_))
            ));
            let loaded = store
                .read_at("account-a", at("2026-09-13T01:00:00Z"), None)
                .await
                .expect("read")
                .expect("snapshot");
            assert_eq!(loaded, original);
        });
    }

    #[test]
    fn initialization_is_safe_for_concurrent_store_instances() {
        let directory = tempdir().expect("temporary directory");
        let path = directory.path().join(QUOTA_DB_FILENAME);
        thread::scope(|scope| {
            for index in 0..4 {
                let path = path.clone();
                scope.spawn(move || {
                    block_on(async move {
                        let store = QuotaSnapshotStore::open(path).await.expect("open store");
                        let mut account = snapshot(&format!("account-{index}"));
                        account.observed_at = at("2026-09-13T00:00:00Z");
                        store.upsert(&account).await.expect("upsert");
                    });
                });
            }
        });

        let snapshots = block_on(async {
            QuotaSnapshotStore::open(path)
                .await
                .expect("reopen store")
                .list()
                .await
                .expect("list snapshots")
        });
        assert_eq!(snapshots.len(), 4);

        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mode = fs::metadata(directory.path().join(QUOTA_DB_FILENAME))
                .expect("database metadata")
                .permissions()
                .mode()
                & 0o777;
            assert_eq!(mode, 0o600);
            let parent_mode = fs::metadata(directory.path())
                .expect("directory metadata")
                .permissions()
                .mode()
                & 0o777;
            assert_eq!(parent_mode, 0o700);
        }
    }
}
