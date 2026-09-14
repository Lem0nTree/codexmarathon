//! Password-encrypted transfer of saved accounts, compiled into the native CLI.
//!
//! No credential crosses an RPC boundary. The age plaintext exists only in
//! zeroizing process buffers. Import stages normal owner-only vault snapshots
//! under fresh, identity-bound references and fsyncs them before a SINGLE full
//! registry rename commits the batch. A crash before that point leaves only
//! unreachable entries; a crash after it exposes the complete batch. Never
//! delete staged entries on a commit error: rename may already have succeeded.
//! GC runs at the beginning of a subsequent write transaction, against its
//! freshly loaded authoritative registry. Older uncooperative binaries must
//! not run concurrently with this version.

use age::secrecy::ExposeSecret;
pub use age::secrecy::SecretString;
use base64::Engine;
use base64::engine::general_purpose::STANDARD;
use codexmarathon_runtime::state_lock::StateLock;
use codexmarathon_runtime::{
    AccountRecord, AuthSnapshot, DomainError, FileAccountRegistry, FileSnapshotVault,
    MarathonConfig, RegistryState, validate_account_id, validate_alias,
};
use serde::{Deserialize, Serialize};
use std::collections::BTreeSet;
use std::fs::{self, File, OpenOptions};
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use zeroize::{Zeroize, Zeroizing};

pub const MAX_ACCOUNTS: usize = 256;
pub const MAX_SNAPSHOT_BYTES: usize = 1024 * 1024;
pub const MAX_PLAINTEXT_BYTES: usize = 16 * 1024 * 1024;
pub const MAX_CIPHERTEXT_BYTES: usize = MAX_PLAINTEXT_BYTES + 64 * 1024;
const MAX_HEADER_BYTES: usize = 8 * 1024;
pub const MAX_PASSPHRASE_BYTES: usize = 1024;
pub const MIN_PASSPHRASE_CHARS: usize = 12;
// 2^18 scrypt, r=8: about 256 MiB. A fixed budget makes backups portable to
// the supported ARM64 hosts and prevents a hostile header requesting more.
const SCRYPT_WORK_FACTOR: u8 = 18;

pub type Result<T> = std::result::Result<T, TransferError>;

#[derive(Debug, thiserror::Error)]
pub enum TransferError {
    #[error("invalid account selection")]
    Selection,
    #[error("invalid or unsupported account backup")]
    Format,
    #[error("backup exceeds a resource limit")]
    Limit,
    #[error("passphrase must contain at least 12 characters and at most 1024 bytes")]
    Passphrase,
    #[error("backup decryption or authentication failed")]
    Decryption,
    #[error("backup encryption failed")]
    Encryption,
    #[error("unsafe file or directory")]
    UnsafePath,
    #[error("output already exists; explicit overwrite is required")]
    OutputExists,
    #[error("account alias conflicts with another account")]
    AliasConflict,
    #[error("cannot replace the currently active account")]
    ActiveAccount,
    #[error("Marathon state is busy; retry after the current operation")]
    Busy,
    #[error("account state is unavailable")]
    State,
    #[error("file operation failed")]
    Io,
    #[error("registry commit could not be confirmed; inspect account state before retrying")]
    CommitUncertain,
}

fn state_error(error: DomainError) -> TransferError {
    match error {
        DomainError::Io(ref error) if error.kind() == std::io::ErrorKind::WouldBlock => {
            TransferError::Busy
        }
        DomainError::UnsafePath => TransferError::UnsafePath,
        _ => TransferError::State,
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ConflictPolicy {
    Skip,
    Replace,
    Rename,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct AccountSummary {
    pub id: String,
    pub alias: String,
}

#[derive(Debug, Serialize)]
pub struct ExportReport {
    pub accounts: Vec<AccountSummary>,
}

#[derive(Debug, Default, Serialize)]
pub struct ImportReport {
    pub imported: Vec<AccountSummary>,
    pub replaced: Vec<AccountSummary>,
    pub skipped: Vec<AccountSummary>,
    pub dry_run: bool,
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Manifest {
    format: String,
    version: u32,
    created_at: String,
    exporter_version: String,
    accounts: Vec<BackupAccount>,
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct BackupAccount {
    id: String,
    alias: String,
    snapshot: String,
}

impl Drop for BackupAccount {
    fn drop(&mut self) {
        self.snapshot.zeroize();
    }
}

fn summary(id: &str, alias: &str) -> AccountSummary {
    AccountSummary {
        id: id.to_owned(),
        alias: alias.to_owned(),
    }
}

pub fn validate_export_passphrase(passphrase: &SecretString) -> Result<()> {
    let text = passphrase.expose_secret();
    if text.chars().count() < MIN_PASSPHRASE_CHARS
        || text.len() > MAX_PASSPHRASE_BYTES
        || text.trim().is_empty()
        || text.contains(['\0', '\n', '\r'])
    {
        return Err(TransferError::Passphrase);
    }
    Ok(())
}

pub fn list_accounts(config: &MarathonConfig) -> Result<Vec<AccountSummary>> {
    let registry = FileAccountRegistry::new(config.registry_path());
    let state = registry.preview_state().map_err(state_error)?;
    Ok(state
        .accounts
        .values()
        .map(|account| summary(&account.id, &account.alias))
        .collect())
}

pub fn export_accounts(
    config: &MarathonConfig,
    selected_ids: &[String],
    output: &Path,
    passphrase: SecretString,
    overwrite: bool,
) -> Result<ExportReport> {
    validate_export_passphrase(&passphrase)?;
    let ids: BTreeSet<&str> = selected_ids.iter().map(String::as_str).collect();
    if ids.is_empty() || ids.len() != selected_ids.len() || ids.len() > MAX_ACCOUNTS {
        return Err(TransferError::Selection);
    }
    let output = checked_output(config, output, overwrite)?;
    let registry = FileAccountRegistry::new(config.registry_path());
    let vault = FileSnapshotVault::new(config.vault_dir());
    let accounts = {
        let lock = StateLock::shared(config.state_dir()).map_err(state_error)?;
        let state = registry.state_locked(&lock).map_err(state_error)?;
        let mut entries = Vec::with_capacity(ids.len());
        let mut encoded_size = 0usize;
        for id in ids {
            let account = state.accounts.get(id).ok_or(TransferError::Selection)?;
            let key = if account.credential_ref.is_empty() {
                &account.id
            } else {
                &account.credential_ref
            };
            let snapshot = vault.load_locked(&lock, key).map_err(state_error)?;
            if snapshot.embedded_account_id() != Some(id) {
                return Err(TransferError::Format);
            }
            let mut encoded = Zeroizing::new(STANDARD.encode(snapshot.bytes()));
            encoded_size = encoded_size
                .checked_add(encoded.len())
                .ok_or(TransferError::Limit)?;
            if encoded_size > MAX_PLAINTEXT_BYTES {
                return Err(TransferError::Limit);
            }
            entries.push(BackupAccount {
                id: id.to_owned(),
                alias: account.alias.clone(),
                snapshot: std::mem::take(&mut *encoded),
            });
        }
        entries
    };
    let report = ExportReport {
        accounts: accounts.iter().map(|a| summary(&a.id, &a.alias)).collect(),
    };
    let manifest = Manifest {
        format: "codexmarathon-accounts".into(),
        version: 1,
        created_at: chrono::Utc::now().to_rfc3339(),
        exporter_version: env!("CARGO_PKG_VERSION").into(),
        accounts,
    };
    let plaintext =
        Zeroizing::new(serde_json::to_vec(&manifest).map_err(|_| TransferError::Format)?);
    if plaintext.len() > MAX_PLAINTEXT_BYTES {
        return Err(TransferError::Limit);
    }
    let mut recipient = age::scrypt::Recipient::new(passphrase);
    recipient.set_work_factor(SCRYPT_WORK_FACTOR);
    let encryptor =
        age::Encryptor::with_recipients(std::iter::once(&recipient as &dyn age::Recipient))
            .map_err(|_| TransferError::Encryption)?;
    let parent = output.parent().ok_or(TransferError::UnsafePath)?;
    let mut temporary = tempfile::Builder::new()
        .prefix(".cmbackup-")
        .tempfile_in(parent)
        .map_err(|_| TransferError::Io)?;
    {
        let mut writer = encryptor
            .wrap_output(temporary.as_file_mut())
            .map_err(|_| TransferError::Encryption)?;
        writer
            .write_all(&plaintext)
            .map_err(|_| TransferError::Encryption)?;
        writer.finish().map_err(|_| TransferError::Encryption)?;
    }
    temporary
        .as_file()
        .sync_all()
        .map_err(|_| TransferError::Io)?;
    // Recheck immediately before publish; persist_noclobber is atomic when
    // overwrite was not authorized, including a competing process's create.
    checked_output(config, &output, overwrite)?;
    if overwrite {
        temporary.persist(&output).map_err(|_| TransferError::Io)?;
    } else {
        temporary.persist_noclobber(&output).map_err(|error| {
            if error.error.kind() == std::io::ErrorKind::AlreadyExists {
                TransferError::OutputExists
            } else {
                TransferError::Io
            }
        })?;
    }
    sync_dir(parent)?;
    Ok(report)
}

pub fn import_accounts(
    config: &MarathonConfig,
    input: &Path,
    passphrase: SecretString,
    conflict: ConflictPolicy,
    dry_run: bool,
) -> Result<ImportReport> {
    if passphrase.expose_secret().is_empty()
        || passphrase.expose_secret().len() > MAX_PASSPHRASE_BYTES
    {
        return Err(TransferError::Passphrase);
    }
    let file = open_input(input, false)?;
    let ciphertext = read_bounded(file, MAX_CIPHERTEXT_BYTES)?;
    let header_end = ciphertext[..ciphertext.len().min(MAX_HEADER_BYTES)]
        .windows(5)
        .position(|window| window == b"\n--- ")
        .ok_or(TransferError::Format)?;
    if header_end + 49 > MAX_HEADER_BYTES {
        return Err(TransferError::Limit);
    }
    let decryptor = age::Decryptor::new_buffered(ciphertext.as_slice())
        .map_err(|_| TransferError::Decryption)?;
    if !decryptor.is_scrypt() {
        return Err(TransferError::Format);
    }
    let mut identity = age::scrypt::Identity::new(passphrase);
    identity.set_max_work_factor(SCRYPT_WORK_FACTOR);
    let reader = decryptor
        .decrypt(std::iter::once(&identity as &dyn age::Identity))
        .map_err(|_| TransferError::Decryption)?;
    // Reading to EOF verifies the final authenticated chunk. Truncation, an
    // altered final tag, or trailing malformed ciphertext is never committed.
    let plaintext = read_bounded(reader, MAX_PLAINTEXT_BYTES).map_err(|error| {
        if matches!(error, TransferError::Limit) {
            error
        } else {
            TransferError::Decryption
        }
    })?;
    let manifest: Manifest =
        serde_json::from_slice(&plaintext).map_err(|_| TransferError::Format)?;
    let entries = validate_manifest(manifest)?;
    let registry = FileAccountRegistry::new(config.registry_path());
    let vault = FileSnapshotVault::new(config.vault_dir());
    if dry_run {
        let state = registry.preview_state().map_err(state_error)?;
        let (report, _) = plan_import(&state, entries, conflict, true)?;
        return Ok(report);
    }
    let lock = StateLock::exclusive(config.state_dir()).map_err(state_error)?;
    let mut state = registry.state_locked(&lock).map_err(state_error)?;
    let (report, changes) = plan_import(&state, entries, conflict, dry_run)?;
    if changes.is_empty() {
        return Ok(report);
    }
    // Only collect against a registry loaded under this exclusive lock. Never
    // GC after an uncertain commit, and never touch legacy vault filenames.
    vault
        .collect_orphans_locked(&lock, &state)
        .map_err(state_error)?;
    for (mut record, snapshot) in changes {
        record.credential_ref = vault
            .save_fresh_locked(&lock, &snapshot)
            .map_err(state_error)?;
        state.accounts.insert(record.id.clone(), record);
    }
    registry
        .replace_locked(&lock, state)
        .map_err(|_| TransferError::CommitUncertain)?;
    Ok(report)
}

fn validate_manifest(manifest: Manifest) -> Result<Vec<(AccountRecord, AuthSnapshot)>> {
    if manifest.version != 1 || manifest.format != "codexmarathon-accounts" {
        return Err(TransferError::Format);
    }
    if chrono::DateTime::parse_from_rfc3339(&manifest.created_at).is_err()
        || manifest.exporter_version.is_empty()
        || manifest.exporter_version.len() > 128
        || !manifest
            .exporter_version
            .bytes()
            .all(|byte| byte.is_ascii_graphic())
    {
        return Err(TransferError::Format);
    }
    if manifest.accounts.is_empty() || manifest.accounts.len() > MAX_ACCOUNTS {
        return Err(TransferError::Limit);
    }
    let mut ids = BTreeSet::new();
    let mut aliases = BTreeSet::new();
    let mut entries = Vec::with_capacity(manifest.accounts.len());
    for entry in &manifest.accounts {
        validate_account_id(&entry.id).map_err(|_| TransferError::Format)?;
        validate_alias(&entry.alias).map_err(|_| TransferError::Format)?;
        if !ids.insert(&entry.id) || (!entry.alias.is_empty() && !aliases.insert(&entry.alias)) {
            return Err(TransferError::Format);
        }
        if entry.snapshot.len() > MAX_SNAPSHOT_BYTES.div_ceil(3) * 4 {
            return Err(TransferError::Limit);
        }
        let bytes = Zeroizing::new(
            STANDARD
                .decode(&entry.snapshot)
                .map_err(|_| TransferError::Format)?,
        );
        if bytes.len() > MAX_SNAPSHOT_BYTES {
            return Err(TransferError::Limit);
        }
        let snapshot =
            AuthSnapshot::for_account(&entry.id, &bytes).map_err(|_| TransferError::Format)?;
        if snapshot.embedded_account_id() != Some(entry.id.as_str()) {
            return Err(TransferError::Format);
        }
        let record = AccountRecord::new(entry.id.clone(), entry.alias.clone())
            .map_err(|_| TransferError::Format)?;
        entries.push((record, snapshot));
    }
    Ok(entries)
}

fn plan_import(
    state: &RegistryState,
    entries: Vec<(AccountRecord, AuthSnapshot)>,
    conflict: ConflictPolicy,
    dry_run: bool,
) -> Result<(ImportReport, Vec<(AccountRecord, AuthSnapshot)>)> {
    let mut planned = state.accounts.clone();
    let mut changes = Vec::new();
    let mut report = ImportReport {
        dry_run,
        ..ImportReport::default()
    };
    for (mut account, snapshot) in entries {
        let exists = planned.contains_key(&account.id);
        if exists && conflict != ConflictPolicy::Replace {
            report.skipped.push(summary(&account.id, &account.alias));
            continue;
        }
        if exists && state.active_account_id == account.id {
            return Err(TransferError::ActiveAccount);
        }
        let collides = |alias: &str| {
            !alias.is_empty()
                && planned
                    .values()
                    .any(|other| other.id != account.id && other.alias == alias)
        };
        if collides(&account.alias) {
            match conflict {
                ConflictPolicy::Skip => {
                    report.skipped.push(summary(&account.id, &account.alias));
                    continue;
                }
                ConflictPolicy::Replace => return Err(TransferError::AliasConflict),
                ConflictPolicy::Rename => {
                    let base = account.alias.clone();
                    let mut renamed = None;
                    for n in 2..=(planned.len() + MAX_ACCOUNTS + 2) {
                        let suffix = format!(" ({n})");
                        let mut prefix = base.as_str();
                        while prefix.len() + suffix.len() > 256 {
                            prefix = &prefix
                                [..prefix.char_indices().last().ok_or(TransferError::Format)?.0];
                        }
                        let candidate = format!("{prefix}{suffix}");
                        if !collides(&candidate) {
                            renamed = Some(candidate);
                            break;
                        }
                    }
                    account.alias = renamed.ok_or(TransferError::AliasConflict)?;
                }
            }
        }
        let item = summary(&account.id, &account.alias);
        if exists {
            report.replaced.push(item);
        } else {
            report.imported.push(item);
        }
        planned.insert(account.id.clone(), account.clone());
        changes.push((account, snapshot));
    }
    let mut aliases = BTreeSet::new();
    if planned
        .values()
        .any(|account| !account.alias.is_empty() && !aliases.insert(&account.alias))
    {
        return Err(TransferError::AliasConflict);
    }
    Ok((report, changes))
}

pub fn read_passphrase_file(path: &Path) -> Result<SecretString> {
    let bytes = read_bounded(open_input(path, true)?, MAX_PASSPHRASE_BYTES + 2)?;
    let mut text = Zeroizing::new(
        std::str::from_utf8(&bytes)
            .map_err(|_| TransferError::Passphrase)?
            .to_owned(),
    );
    if text.ends_with('\n') {
        text.pop();
        if text.ends_with('\r') {
            text.pop();
        }
    }
    if text.len() > MAX_PASSPHRASE_BYTES || text.is_empty() || text.contains(['\0', '\n', '\r']) {
        return Err(TransferError::Passphrase);
    }
    Ok(SecretString::from(std::mem::take(&mut *text)))
}

fn read_bounded(mut reader: impl Read, limit: usize) -> Result<Zeroizing<Vec<u8>>> {
    let mut bytes = Zeroizing::new(Vec::new());
    reader
        .by_ref()
        .take(limit as u64 + 1)
        .read_to_end(&mut bytes)
        .map_err(|_| TransferError::Io)?;
    if bytes.len() > limit {
        return Err(TransferError::Limit);
    }
    Ok(bytes)
}

fn reject_symlinks(path: &Path) -> Result<()> {
    let mut current = PathBuf::new();
    for component in path.components() {
        if matches!(component, std::path::Component::ParentDir) {
            return Err(TransferError::UnsafePath);
        }
        current.push(component);
        match fs::symlink_metadata(&current) {
            Ok(metadata) if metadata.file_type().is_symlink() => {
                return Err(TransferError::UnsafePath);
            }
            Ok(_) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(_) => return Err(TransferError::Io),
        }
    }
    Ok(())
}

fn open_input(path: &Path, private: bool) -> Result<File> {
    reject_symlinks(path)?;
    let mut options = OpenOptions::new();
    options.read(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK);
    }
    let file = options.open(path).map_err(|_| TransferError::Io)?;
    let metadata = file.metadata().map_err(|_| TransferError::Io)?;
    if !metadata.is_file() {
        return Err(TransferError::UnsafePath);
    }
    #[cfg(unix)]
    if private {
        use std::os::unix::fs::MetadataExt;
        // SAFETY: geteuid has no preconditions.
        if metadata.uid() != unsafe { libc::geteuid() } || metadata.mode() & 0o077 != 0 {
            return Err(TransferError::UnsafePath);
        }
    }
    #[cfg(not(unix))]
    if private {
        return Err(TransferError::UnsafePath);
    }
    Ok(file)
}

fn checked_output(config: &MarathonConfig, output: &Path, overwrite: bool) -> Result<PathBuf> {
    let absolute = absolute_path(output)?;
    reject_symlinks(&absolute)?;
    let parent = absolute.parent().ok_or(TransferError::UnsafePath)?;
    if !parent.is_dir() {
        return Err(TransferError::UnsafePath);
    }
    if absolute.starts_with(absolute_path(config.state_dir())?)
        || absolute == absolute_path(&config.codex_home().join("auth.json"))?
    {
        return Err(TransferError::UnsafePath);
    }
    match fs::symlink_metadata(&absolute) {
        Ok(metadata) if !metadata.is_file() => Err(TransferError::UnsafePath),
        Ok(_) if !overwrite => Err(TransferError::OutputExists),
        Ok(_) => Ok(absolute),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(absolute),
        Err(_) => Err(TransferError::Io),
    }
}

fn absolute_path(path: &Path) -> Result<PathBuf> {
    let path = if path.is_absolute() {
        path.to_path_buf()
    } else {
        std::env::current_dir()
            .map_err(|_| TransferError::Io)?
            .join(path)
    };
    let mut normalized = PathBuf::new();
    for component in path.components() {
        match component {
            std::path::Component::ParentDir => return Err(TransferError::UnsafePath),
            std::path::Component::CurDir => {}
            component => normalized.push(component),
        }
    }
    Ok(normalized)
}

fn sync_dir(path: &Path) -> Result<()> {
    #[cfg(unix)]
    File::open(path)
        .and_then(|file| file.sync_all())
        .map_err(|_| TransferError::Io)?;
    Ok(())
}

#[cfg(test)]
mod tests;
