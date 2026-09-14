//! Native CodexMarathon command-line controls.
//!
//! This module talks to an embedded app-server client in the same `codex`
//! process.  It deliberately does not know about the legacy Go controller or
//! its Unix socket environment variable; all state changes go through the
//! typed app-server Marathon requests.

use age::secrecy::ExposeSecret;
use clap::{Parser, ValueEnum};
use codex_app_server_client::DEFAULT_IN_PROCESS_CHANNEL_CAPACITY;
use codex_app_server_client::InProcessAppServerClient;
use codex_app_server_client::InProcessClientStartArgs;
use codex_app_server_client::InProcessServerEvent;
use codex_app_server_protocol::ClientRequest;
use codex_app_server_protocol::LoginAccountResponse;
use codex_app_server_protocol::MarathonAccount;
use codex_app_server_protocol::MarathonAutoResetSetParams;
use codex_app_server_protocol::MarathonAutoResetSetResponse;
use codex_app_server_protocol::MarathonCheckpointParams;
use codex_app_server_protocol::MarathonCheckpointResponse;
use codex_app_server_protocol::MarathonEnabledSetParams;
use codex_app_server_protocol::MarathonEnabledSetResponse;
use codex_app_server_protocol::MarathonImportParams;
use codex_app_server_protocol::MarathonImportResponse;
use codex_app_server_protocol::MarathonStatusResponse;
use codex_app_server_protocol::MarathonSwitchOutcome;
use codex_app_server_protocol::MarathonSwitchParams;
use codex_app_server_protocol::MarathonSwitchResponse;
use codex_app_server_protocol::RequestId;
use codex_app_server_protocol::ServerNotification;
use codex_arg0::Arg0DispatchPaths;
use codex_cloud_config::cloud_config_bundle_loader_for_storage;
use codex_config::CloudConfigBundleLoader;
use codex_config::LoaderOverrides;
use codex_core::config::Config;
use codex_core::config::ConfigBuilder;
use codex_exec_server::EnvironmentManager;
use codex_feedback::CodexFeedback;
use codex_utils_cli::CliConfigOverrides;
use codexmarathon_accountd_client::{
    AccountTelemetry as DaemonTelemetry, AccountdClient, AccountsResult, Freshness, UsageWindow,
    WindowKind, default_socket_path,
};
use codexmarathon_runtime::MarathonConfig;
use codexmarathon_transfer as transfer;
use codexmarathon_transfer::ConflictPolicy;
use codexmarathon_transfer::SecretString;
use crossterm::cursor;
use crossterm::event::{self, Event, KeyCode, KeyEventKind, KeyModifiers};
use crossterm::execute;
use crossterm::terminal::{self, ClearType};
use serde::de::DeserializeOwned;
use std::io;
use std::io::IsTerminal;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;
use zeroize::Zeroizing;

#[derive(Debug, Parser)]
pub(crate) struct MarathonCommand {
    /// Control the native Marathon service. With no action, print its status.
    #[command(subcommand)]
    pub action: Option<MarathonAction>,
}

#[derive(Debug, clap::Subcommand)]
pub(crate) enum MarathonAction {
    /// Print enabled state and the registered account list.
    Status,

    /// Alias for `status` that emphasizes the account list.
    Accounts {
        /// Read durable quota metadata from codexmarathon-accountd.
        #[arg(long)]
        daemon: bool,
        /// Output format used with --daemon.
        #[arg(long, value_enum, default_value_t = DaemonAccountsFormat::Table)]
        format: DaemonAccountsFormat,
        /// Override the accountd Unix socket.
        #[arg(long, requires = "daemon")]
        socket: Option<std::path::PathBuf>,
    },

    /// Enable automatic native Marathon account management.
    On,

    /// Disable automatic native Marathon account management.
    Off,

    /// Configure automatic use of a provider-supported quota-reset action
    /// once every eligible managed account reaches zero weekly quota.
    AutoReset {
        #[command(subcommand)]
        action: AutoResetAction,
    },

    /// Store the currently authenticated native Codex identity under ALIAS.
    Import {
        /// Local account alias, for example `personal` or `work`.
        alias: String,
    },

    /// Start the normal native ChatGPT login flow, then store it under ALIAS.
    Login {
        /// Local account alias to assign after login succeeds.
        alias: String,
        /// Use the browser-link flow. This is the default choice in an interactive terminal.
        #[arg(long, conflicts_with = "device_code")]
        browser: bool,
        /// Use device authentication and enter the one-time code from another machine.
        #[arg(long, conflicts_with = "browser")]
        device_code: bool,
    },

    /// Switch the native Codex identity to an account id or alias.
    Switch {
        /// Registered account alias or account id.
        target: String,
    },

    /// Encrypt or restore native Marathon account backups.
    Backup {
        #[command(subcommand)]
        action: BackupAction,
    },
}

#[derive(Debug, clap::Subcommand)]
pub(crate) enum BackupAction {
    /// Encrypt selected native Marathon accounts into an age backup file.
    Export(BackupExportArgs),

    /// Restore native Marathon accounts from an encrypted age backup file.
    Import(BackupImportArgs),
}

#[derive(Debug, clap::Args)]
pub(crate) struct BackupExportArgs {
    /// Destination path for the encrypted backup.
    #[arg(long, value_name = "PATH")]
    output: PathBuf,

    /// Account id to include. Repeat for multiple accounts.
    #[arg(long = "account", value_name = "ID", conflicts_with = "all")]
    account: Vec<String>,

    /// Include every registered account.
    #[arg(long, conflicts_with = "account")]
    all: bool,

    /// Replace an existing destination file.
    #[arg(long)]
    overwrite: bool,

    /// Read the passphrase from an owner-only file instead of prompting.
    #[arg(long, value_name = "PATH")]
    passphrase_file: Option<PathBuf>,
}

#[derive(Clone, Copy, Debug, ValueEnum)]
pub(crate) enum BackupConflict {
    /// Leave a conflicting account unchanged.
    Skip,
    /// Replace an existing account with the imported snapshot.
    Replace,
    /// Rename an imported account when its alias conflicts.
    Rename,
}

impl BackupConflict {
    fn into_policy(self) -> ConflictPolicy {
        match self {
            Self::Skip => ConflictPolicy::Skip,
            Self::Replace => ConflictPolicy::Replace,
            Self::Rename => ConflictPolicy::Rename,
        }
    }
}

#[derive(Debug, clap::Args)]
pub(crate) struct BackupImportArgs {
    /// Source path for the encrypted backup.
    #[arg(value_name = "PATH")]
    input: PathBuf,

    /// Resolve account conflicts using this policy.
    #[arg(long, value_enum, default_value_t = BackupConflict::Skip)]
    conflict: BackupConflict,

    /// Validate and preview the backup without writing account state.
    #[arg(long)]
    dry_run: bool,

    /// Apply the import without an interactive confirmation.
    #[arg(long, conflicts_with = "dry_run")]
    yes: bool,

    /// Read the passphrase from an owner-only file instead of prompting.
    #[arg(long, value_name = "PATH")]
    passphrase_file: Option<PathBuf>,
}

#[derive(Clone, Copy, Debug, ValueEnum)]
pub(crate) enum DaemonAccountsFormat {
    Table,
    Motd,
    Json,
}

#[derive(Debug, clap::Subcommand)]
pub(crate) enum AutoResetAction {
    /// Show automatic reset-action state.
    Status,
    /// Enable automatic reset-action use. It remains disabled by default.
    On,
    /// Disable automatic reset-action use.
    Off,
}

pub(crate) async fn run(
    command: MarathonCommand,
    root_config_overrides: &CliConfigOverrides,
    strict_config: bool,
    arg0_paths: Arg0DispatchPaths,
    loader_overrides: LoaderOverrides,
) -> anyhow::Result<()> {
    let action = command.action.unwrap_or(MarathonAction::Status);
    if matches!(&action, MarathonAction::Backup { .. }) {
        return run_backup(
            action,
            root_config_overrides,
            strict_config,
            arg0_paths,
            loader_overrides,
        )
        .await;
    }
    if let MarathonAction::Accounts {
        daemon: true,
        format,
        socket,
    } = &action
    {
        return print_daemon_accounts(*format, socket.clone()).await;
    }
    let mut client = start_client(
        root_config_overrides,
        strict_config,
        arg0_paths,
        loader_overrides,
    )
    .await?;

    let result = run_action(&mut client, action).await;
    let shutdown_result = client.shutdown().await;
    result?;
    shutdown_result.map_err(anyhow::Error::from)
}

async fn start_client(
    root_config_overrides: &CliConfigOverrides,
    strict_config: bool,
    arg0_paths: Arg0DispatchPaths,
    loader_overrides: LoaderOverrides,
) -> anyhow::Result<InProcessAppServerClient> {
    let prepared = prepare_config(root_config_overrides, strict_config, &loader_overrides).await?;
    start_client_with_prepared(prepared, strict_config, arg0_paths, loader_overrides).await
}

async fn start_client_with_prepared(
    prepared: PreparedConfig,
    strict_config: bool,
    arg0_paths: Arg0DispatchPaths,
    loader_overrides: LoaderOverrides,
) -> anyhow::Result<InProcessAppServerClient> {
    let PreparedConfig {
        config,
        cli_overrides,
        cloud_config_bundle,
    } = prepared;

    let config_warnings = config
        .startup_warnings
        .iter()
        .map(
            |summary| codex_app_server_protocol::ConfigWarningNotification {
                summary: summary.clone(),
                details: None,
                path: None,
                range: None,
            },
        )
        .collect();
    let environment_manager = Arc::new(EnvironmentManager::without_environments(
        config.http_client_factory(),
    ));

    Ok(InProcessAppServerClient::start(InProcessClientStartArgs {
        arg0_paths,
        config: Arc::new(config),
        cli_overrides,
        loader_overrides,
        strict_config,
        cloud_config_bundle,
        feedback: CodexFeedback::new(),
        log_db: None,
        state_db: None,
        environment_manager,
        config_warnings,
        session_source: serde_json::from_value(serde_json::json!("cli"))?,
        enable_codex_api_key_env: false,
        client_name: "codex-cli".to_string(),
        client_version: env!("CARGO_PKG_VERSION").to_string(),
        experimental_api: true,
        mcp_server_openai_form_elicitation: false,
        opt_out_notification_methods: Vec::new(),
        channel_capacity: DEFAULT_IN_PROCESS_CHANNEL_CAPACITY,
    })
    .await?)
}

struct PreparedConfig {
    config: Config,
    cli_overrides: Vec<(String, toml::Value)>,
    cloud_config_bundle: CloudConfigBundleLoader,
}

async fn prepare_config(
    root_config_overrides: &CliConfigOverrides,
    strict_config: bool,
    loader_overrides: &LoaderOverrides,
) -> anyhow::Result<PreparedConfig> {
    let cli_overrides = root_config_overrides
        .parse_overrides()
        .map_err(anyhow::Error::msg)?;

    // Build once without managed cloud config so the native auth configuration
    // can choose the same cloud loader as the interactive TUI.
    let bootstrap_config = ConfigBuilder::default()
        .cli_overrides(cli_overrides.clone())
        .loader_overrides(loader_overrides.clone())
        .strict_config(strict_config)
        .build()
        .await?;
    let cloud_config_bundle = cloud_config_bundle_loader_for_storage(
        bootstrap_config.auth_config(),
        /*enable_codex_api_key_env*/ false,
    )
    .await?;
    let config = ConfigBuilder::default()
        .cli_overrides(cli_overrides.clone())
        .loader_overrides(loader_overrides.clone())
        .strict_config(strict_config)
        .cloud_config_bundle(cloud_config_bundle.clone())
        .build()
        .await?;

    Ok(PreparedConfig {
        config,
        cli_overrides,
        cloud_config_bundle,
    })
}

async fn run_backup(
    action: MarathonAction,
    root_config_overrides: &CliConfigOverrides,
    strict_config: bool,
    arg0_paths: Arg0DispatchPaths,
    loader_overrides: LoaderOverrides,
) -> anyhow::Result<()> {
    let MarathonAction::Backup { action } = action else {
        unreachable!("run_backup is only called for a backup action");
    };
    let prepared = prepare_config(root_config_overrides, strict_config, &loader_overrides).await?;
    let config = MarathonConfig::new(prepared.config.codex_home.to_path_buf())?;
    run_backup_action(
        &config,
        action,
        prepared,
        strict_config,
        arg0_paths,
        loader_overrides,
    )
    .await
}

async fn run_backup_action(
    config: &MarathonConfig,
    action: BackupAction,
    prepared: PreparedConfig,
    strict_config: bool,
    arg0_paths: Arg0DispatchPaths,
    loader_overrides: LoaderOverrides,
) -> anyhow::Result<()> {
    match action {
        BackupAction::Export(args) => {
            run_backup_export(
                config,
                args,
                prepared,
                strict_config,
                arg0_paths,
                loader_overrides,
            )
            .await
        }
        BackupAction::Import(args) => {
            run_backup_import(
                config,
                args,
                prepared,
                strict_config,
                arg0_paths,
                loader_overrides,
            )
            .await
        }
    }
}

async fn run_backup_export(
    config: &MarathonConfig,
    args: BackupExportArgs,
    prepared: PreparedConfig,
    strict_config: bool,
    arg0_paths: Arg0DispatchPaths,
    loader_overrides: LoaderOverrides,
) -> anyhow::Result<()> {
    let interactive = interactive_terminal();
    let accounts = transfer::list_accounts(config).map_err(transfer_error)?;
    let selected_ids = if args.all {
        account_ids(&accounts)
    } else if !args.account.is_empty() {
        args.account
    } else {
        if !interactive {
            anyhow::bail!("backup export requires --account or --all when no terminal is attached");
        }
        choose_accounts(&accounts)?
    };
    if selected_ids.is_empty() {
        anyhow::bail!("no accounts are available to export");
    }

    let passphrase = export_passphrase(args.passphrase_file.as_deref(), interactive)?;
    checkpoint_before_export(
        &selected_ids,
        prepared,
        strict_config,
        arg0_paths,
        loader_overrides,
    )
    .await?;
    let report = transfer::export_accounts(
        config,
        &selected_ids,
        &args.output,
        passphrase,
        args.overwrite,
    )
    .map_err(transfer_error)?;

    println!(
        "Encrypted {} account{} to {}.",
        report.accounts.len(),
        plural_suffix(report.accounts.len()),
        args.output.display()
    );
    print_account_summaries("  ", &report.accounts);
    Ok(())
}

async fn run_backup_import(
    config: &MarathonConfig,
    args: BackupImportArgs,
    prepared: PreparedConfig,
    strict_config: bool,
    arg0_paths: Arg0DispatchPaths,
    loader_overrides: LoaderOverrides,
) -> anyhow::Result<()> {
    let interactive = interactive_terminal();
    if !args.dry_run && !args.yes && !interactive {
        anyhow::bail!("backup import requires --yes when no terminal is attached");
    }
    let passphrase = import_passphrase(args.passphrase_file.as_deref(), interactive)?;
    let conflict = args.conflict.into_policy();

    // Always preview first so a replace operation can be checked against the
    // account currently held by the native AuthManager before confirmation or
    // any write transaction begins.
    let preview =
        transfer::import_accounts(config, &args.input, passphrase.clone(), conflict, true)
            .map_err(transfer_error)?;
    if !args.dry_run && matches!(conflict, ConflictPolicy::Replace) && !preview.replaced.is_empty()
    {
        reject_live_account_replacement(
            &preview.replaced,
            prepared,
            strict_config,
            arg0_paths,
            loader_overrides,
        )
        .await?;
    }

    if args.dry_run {
        print_import_report(&preview, true);
        return Ok(());
    }

    if args.yes {
        let report = transfer::import_accounts(config, &args.input, passphrase, conflict, false)
            .map_err(transfer_error)?;
        print_import_report(&report, false);
        return Ok(());
    }

    // Planning with dry_run gives the operator a secret-free preview before
    // the one write transaction. The passphrase is cloned inside SecretString
    // and is never converted to a normal command-line or environment value.
    print_import_report(&preview, true);
    if preview.imported.is_empty() && preview.replaced.is_empty() {
        return Ok(());
    }
    if !confirm_import()? {
        println!("Backup import canceled.");
        return Ok(());
    }

    let report = transfer::import_accounts(config, &args.input, passphrase, conflict, false)
        .map_err(transfer_error)?;
    print_import_report(&report, false);
    Ok(())
}

async fn checkpoint_before_export(
    selected_ids: &[String],
    prepared: PreparedConfig,
    strict_config: bool,
    arg0_paths: Arg0DispatchPaths,
    loader_overrides: LoaderOverrides,
) -> anyhow::Result<()> {
    // Always ask the embedded service for the live identity. The persisted
    // registry marker can lag native AuthManager after a login or switch.
    let mut client =
        start_client_with_prepared(prepared, strict_config, arg0_paths, loader_overrides).await?;
    let checkpoint_result = checkpoint_selected_active(&mut client, selected_ids).await;
    let shutdown_result = client.shutdown().await;
    checkpoint_result?;
    shutdown_result.map_err(anyhow::Error::from)
}

async fn checkpoint_selected_active(
    client: &mut InProcessAppServerClient,
    selected_ids: &[String],
) -> anyhow::Result<()> {
    let status: MarathonStatusResponse = request(
        client,
        ClientRequest::MarathonStatus {
            request_id: request_id("marathon-backup-status"),
            params: None,
        },
    )
    .await?;
    let Some(active_account_id) = status.current_account_id else {
        return Ok(());
    };
    if !selected_ids
        .iter()
        .any(|account_id| account_id == &active_account_id)
    {
        return Ok(());
    }

    let response: MarathonCheckpointResponse = request(
        client,
        ClientRequest::MarathonCheckpoint {
            request_id: request_id("marathon-backup-checkpoint"),
            params: MarathonCheckpointParams {
                expected_account_id: active_account_id.clone(),
                expected_auth_generation: status.auth_generation,
            },
        },
    )
    .await?;
    if response.account_id != active_account_id {
        anyhow::bail!("backup checkpoint returned an unexpected account");
    }
    Ok(())
}

async fn reject_live_account_replacement(
    replaced: &[transfer::AccountSummary],
    prepared: PreparedConfig,
    strict_config: bool,
    arg0_paths: Arg0DispatchPaths,
    loader_overrides: LoaderOverrides,
) -> anyhow::Result<()> {
    let client =
        start_client_with_prepared(prepared, strict_config, arg0_paths, loader_overrides).await?;
    let current_result: anyhow::Result<Option<String>> = request(
        &client,
        ClientRequest::MarathonStatus {
            request_id: request_id("marathon-backup-import-status"),
            params: None,
        },
    )
    .await
    .map(|status: MarathonStatusResponse| status.current_account_id);
    let shutdown_result = client.shutdown().await;
    let current_account_id = current_result?;
    shutdown_result.map_err(anyhow::Error::from)?;

    if let Some(current_account_id) = current_account_id {
        if replacement_includes_account(replaced, &current_account_id) {
            anyhow::bail!(
                "backup import cannot replace the currently authenticated account; switch accounts first"
            );
        }
    }
    Ok(())
}

fn replacement_includes_account(
    replaced: &[transfer::AccountSummary],
    current_account_id: &str,
) -> bool {
    replaced
        .iter()
        .any(|account| account.id == current_account_id)
}

fn account_ids(accounts: &[transfer::AccountSummary]) -> Vec<String> {
    accounts.iter().map(|account| account.id.clone()).collect()
}

fn export_passphrase(path: Option<&Path>, interactive: bool) -> anyhow::Result<SecretString> {
    if let Some(path) = path {
        return transfer::read_passphrase_file(path).map_err(transfer_error);
    }
    if !interactive {
        anyhow::bail!("backup export requires --passphrase-file when no terminal is attached");
    }

    let first = read_hidden_passphrase("Passphrase: ")?;
    let second = read_hidden_passphrase("Repeat passphrase: ")?;
    if first.expose_secret() != second.expose_secret() {
        anyhow::bail!("passphrases do not match");
    }
    Ok(first)
}

fn import_passphrase(path: Option<&Path>, interactive: bool) -> anyhow::Result<SecretString> {
    if let Some(path) = path {
        return transfer::read_passphrase_file(path).map_err(transfer_error);
    }
    if !interactive {
        anyhow::bail!("backup import requires --passphrase-file when no terminal is attached");
    }
    read_hidden_passphrase("Passphrase: ")
}

fn transfer_error(error: transfer::TransferError) -> anyhow::Error {
    anyhow::anyhow!(error.to_string())
}

fn plural_suffix(count: usize) -> &'static str {
    if count == 1 { "" } else { "s" }
}

fn print_account_summaries(indent: &str, accounts: &[transfer::AccountSummary]) {
    for account in accounts {
        let alias = if account.alias.is_empty() {
            "(no alias)"
        } else {
            account.alias.as_str()
        };
        println!("{indent}{alias} ({})", account.id);
    }
}

fn print_import_report(report: &transfer::ImportReport, preview: bool) {
    println!(
        "Backup import {}.",
        if preview {
            "preview (no changes written)"
        } else {
            "completed"
        }
    );
    print_import_group("  Imported", &report.imported);
    print_import_group("  Replaced", &report.replaced);
    print_import_group("  Skipped", &report.skipped);
}

fn print_import_group(label: &str, accounts: &[transfer::AccountSummary]) {
    println!("{label}: {}", accounts.len());
    print_account_summaries("    ", accounts);
}

fn confirm_import() -> anyhow::Result<bool> {
    let mut stderr = io::stderr();
    write!(stderr, "Apply this backup import? [y/N] ")?;
    stderr.flush()?;
    let mut answer = String::new();
    io::stdin().read_line(&mut answer)?;
    writeln!(stderr)?;
    Ok(matches!(
        answer.trim().to_ascii_lowercase().as_str(),
        "y" | "yes"
    ))
}

fn interactive_terminal() -> bool {
    io::stdin().is_terminal() && io::stderr().is_terminal()
}

struct RawModeGuard;

impl Drop for RawModeGuard {
    fn drop(&mut self) {
        let _ = terminal::disable_raw_mode();
    }
}

fn read_hidden_passphrase(prompt: &str) -> anyhow::Result<SecretString> {
    let mut line = read_hidden_line(prompt)?;
    Ok(SecretString::from(std::mem::take(&mut *line)))
}

fn read_hidden_line(prompt: &str) -> anyhow::Result<Zeroizing<String>> {
    let mut stderr = io::stderr();
    write!(stderr, "{prompt}")?;
    stderr.flush()?;
    terminal::enable_raw_mode()?;
    let guard = RawModeGuard;
    let mut line = Zeroizing::new(String::new());
    let result = loop {
        let Event::Key(key) = event::read()? else {
            continue;
        };
        if key.kind != KeyEventKind::Press {
            continue;
        }
        if key.modifiers.contains(KeyModifiers::CONTROL)
            && matches!(key.code, KeyCode::Char('c') | KeyCode::Char('C'))
        {
            break Err(anyhow::anyhow!("passphrase entry canceled"));
        }
        match key.code {
            KeyCode::Enter => break Ok(Zeroizing::new(std::mem::take(&mut *line))),
            KeyCode::Esc => break Err(anyhow::anyhow!("passphrase entry canceled")),
            KeyCode::Backspace => {
                line.pop();
            }
            KeyCode::Char(character) => line.push(character),
            _ => {}
        }
    };
    drop(guard);
    writeln!(stderr)?;
    result
}

fn choose_accounts(accounts: &[transfer::AccountSummary]) -> anyhow::Result<Vec<String>> {
    if accounts.is_empty() {
        anyhow::bail!("no accounts are available to export");
    }
    terminal::enable_raw_mode()?;
    let guard = RawModeGuard;
    let result = account_picker_loop(accounts);
    drop(guard);
    writeln!(io::stderr())?;
    result
}

fn account_picker_loop(accounts: &[transfer::AccountSummary]) -> anyhow::Result<Vec<String>> {
    let mut selected = vec![false; accounts.len()];
    let mut cursor = 0usize;
    let mut rendered_lines = 0usize;
    let mut message = None;
    let mut stderr = io::stderr();

    loop {
        rendered_lines = render_account_picker(
            &mut stderr,
            accounts,
            &selected,
            cursor,
            rendered_lines,
            message,
        )?;
        message = None;
        let Event::Key(key) = event::read()? else {
            continue;
        };
        if key.kind != KeyEventKind::Press {
            continue;
        }
        if key.modifiers.contains(KeyModifiers::CONTROL)
            && matches!(key.code, KeyCode::Char('c') | KeyCode::Char('C'))
        {
            anyhow::bail!("account selection canceled");
        }
        match key.code {
            KeyCode::Up => {
                cursor = if cursor == 0 {
                    accounts.len() - 1
                } else {
                    cursor - 1
                };
            }
            KeyCode::Down => {
                cursor = (cursor + 1) % accounts.len();
            }
            KeyCode::Char(' ') => selected[cursor] = !selected[cursor],
            KeyCode::Char('a') | KeyCode::Char('A') => selected.fill(true),
            KeyCode::Enter => {
                if selected.iter().any(|selected| *selected) {
                    return Ok(accounts
                        .iter()
                        .zip(selected)
                        .filter_map(|(account, selected)| selected.then_some(account.id.clone()))
                        .collect());
                }
                message = Some("select at least one account before confirming");
            }
            KeyCode::Esc => anyhow::bail!("account selection canceled"),
            _ => {}
        }
    }
}

fn render_account_picker(
    stderr: &mut io::Stderr,
    accounts: &[transfer::AccountSummary],
    selected: &[bool],
    cursor: usize,
    rendered_lines: usize,
    message: Option<&str>,
) -> anyhow::Result<usize> {
    if rendered_lines > 0 {
        execute!(
            stderr,
            cursor::MoveToColumn(0),
            cursor::MoveUp(rendered_lines as u16),
            terminal::Clear(ClearType::FromCursorDown)
        )?;
    }
    writeln!(
        stderr,
        "Select accounts to export (Space toggle, a all, Enter confirm, Esc cancel):"
    )?;
    let mut lines = 1usize;
    for (index, account) in accounts.iter().enumerate() {
        let pointer = if index == cursor { ">" } else { " " };
        let marker = if selected[index] { "x" } else { " " };
        let alias = if account.alias.is_empty() {
            "(no alias)"
        } else {
            account.alias.as_str()
        };
        writeln!(stderr, "{pointer} [{marker}] {alias} ({})", account.id)?;
        lines += 1;
    }
    if let Some(message) = message {
        writeln!(stderr, "{message}")?;
        lines += 1;
    }
    stderr.flush()?;
    Ok(lines)
}

async fn run_action(
    client: &mut InProcessAppServerClient,
    action: MarathonAction,
) -> anyhow::Result<()> {
    match action {
        MarathonAction::Status | MarathonAction::Accounts { .. } => {
            let status = request(
                client,
                ClientRequest::MarathonStatus {
                    request_id: request_id("marathon-status"),
                    params: None,
                },
            )
            .await?;
            print_status(&status);
        }
        MarathonAction::On | MarathonAction::Off => {
            let enabled = matches!(action, MarathonAction::On);
            let response: MarathonEnabledSetResponse = request(
                client,
                ClientRequest::MarathonEnabledSet {
                    request_id: request_id("marathon-enabled"),
                    params: MarathonEnabledSetParams { enabled },
                },
            )
            .await?;
            println!(
                "Native Marathon is {}.",
                if response.enabled {
                    "enabled"
                } else {
                    "disabled"
                }
            );
        }
        MarathonAction::AutoReset { action } => match action {
            AutoResetAction::Status => {
                let status: MarathonStatusResponse = request(
                    client,
                    ClientRequest::MarathonStatus {
                        request_id: request_id("marathon-auto-reset-status"),
                        params: None,
                    },
                )
                .await?;
                println!(
                    "Automatic quota reset: {} (phase: {}).{}",
                    if status.auto_reset_enabled {
                        "enabled"
                    } else {
                        "disabled"
                    },
                    status.auto_reset_phase,
                    status
                        .auto_reset_last_error
                        .as_deref()
                        .map(|error| format!(" Last result: {error}."))
                        .unwrap_or_default()
                );
            }
            AutoResetAction::On | AutoResetAction::Off => {
                let enabled = matches!(action, AutoResetAction::On);
                let response: MarathonAutoResetSetResponse = request(
                    client,
                    ClientRequest::MarathonAutoResetSet {
                        request_id: request_id("marathon-auto-reset-set"),
                        params: MarathonAutoResetSetParams { enabled },
                    },
                )
                .await?;
                println!(
                    "Automatic provider quota reset is {} (phase: {}).",
                    if response.enabled {
                        "enabled"
                    } else {
                        "disabled"
                    },
                    response.phase
                );
            }
        },
        MarathonAction::Import { alias } => {
            let response: MarathonImportResponse = request(
                client,
                ClientRequest::MarathonImport {
                    request_id: request_id("marathon-import"),
                    params: MarathonImportParams { alias },
                },
            )
            .await?;
            println!(
                "Imported native account `{}` as `{}` ({}).",
                response.account_id,
                response.alias,
                if response.replaced {
                    "updated existing account"
                } else {
                    "created account"
                }
            );
        }
        MarathonAction::Switch { target } => {
            let response: MarathonSwitchResponse = request(
                client,
                ClientRequest::MarathonSwitch {
                    request_id: request_id("marathon-switch"),
                    params: MarathonSwitchParams { target },
                },
            )
            .await?;
            print_switch(&response)?;
        }
        MarathonAction::Login {
            alias,
            browser,
            device_code,
        } => {
            let mode = if browser {
                LoginMode::Browser
            } else if device_code {
                LoginMode::DeviceCode
            } else {
                choose_login_mode()?
            };
            login_and_import(client, alias, mode).await?
        }
        MarathonAction::Backup { .. } => {
            unreachable!("backup actions are handled before app-server startup")
        }
    }
    Ok(())
}

async fn print_daemon_accounts(
    format: DaemonAccountsFormat,
    socket: Option<std::path::PathBuf>,
) -> anyhow::Result<()> {
    let socket = socket.or_else(default_socket_path).ok_or_else(|| {
        anyhow::anyhow!("XDG_RUNTIME_DIR is unavailable; pass --socket explicitly")
    })?;
    let accounts: AccountsResult = AccountdClient::new(socket).accounts().await?;

    if matches!(format, DaemonAccountsFormat::Json) {
        println!("{}", serde_json::to_string(&accounts)?);
        return Ok(());
    }
    if matches!(format, DaemonAccountsFormat::Motd) {
        println!("ACCOUNT\tACTIVE\tHEALTH\t5H LEFT\tWEEKLY LEFT\t5H RESET");
    } else {
        println!(
            "{:<20} {:<7} {:<10} {:>9} {:>12} {:>12}",
            "ACCOUNT", "ACTIVE", "HEALTH", "5H LEFT", "WEEKLY LEFT", "5H RESET"
        );
    }
    for account in accounts.accounts {
        let (primary, secondary) = account
            .quota
            .as_ref()
            .map(quota_columns)
            .unwrap_or_else(|| (None, None));
        let short_left = quota_left(primary.as_ref());
        let weekly_left = quota_left(secondary.as_ref());
        let reset = quota_reset(primary.as_ref());
        if matches!(format, DaemonAccountsFormat::Motd) {
            println!(
                "{}\t{}\t{:?}\t{}\t{}\t{}",
                account.alias,
                if account.active { "yes" } else { "no" },
                account.credential_health,
                short_left,
                weekly_left,
                reset
            );
        } else {
            println!(
                "{:<20} {:<7} {:<10} {:>9} {:>12} {:>12}",
                account.alias,
                if account.active { "yes" } else { "no" },
                format!("{:?}", account.credential_health).to_lowercase(),
                short_left,
                weekly_left,
                reset
            );
        }
    }
    Ok(())
}

fn quota_columns(telemetry: &DaemonTelemetry) -> (Option<UsageWindow>, Option<UsageWindow>) {
    let mut primary = None;
    let mut secondary = None;
    for window in telemetry.windows() {
        match window.kind {
            WindowKind::Primary => primary = Some(window.clone()),
            WindowKind::Secondary => secondary = Some(window.clone()),
        }
    }
    (primary, secondary)
}

fn quota_left(window: Option<&UsageWindow>) -> String {
    window.map_or_else(
        || "unknown".to_string(),
        |window| {
            if window.freshness == Freshness::Stale {
                "stale".to_string()
            } else {
                format!("{:.1}%", 100.0 - window.used_percent)
            }
        },
    )
}

fn quota_reset(window: Option<&UsageWindow>) -> String {
    let Some(reset) = window.and_then(|window| window.resets_at) else {
        return "—".to_string();
    };
    let seconds = (reset - chrono::Utc::now()).num_seconds();
    if seconds <= 0 {
        return "ready".to_string();
    }
    let hours = seconds / 3600;
    let minutes = (seconds % 3600) / 60;
    format!("{hours}h {minutes}m")
}

async fn login_and_import(
    client: &mut InProcessAppServerClient,
    alias: String,
    mode: LoginMode,
) -> anyhow::Result<()> {
    let params = match mode {
        LoginMode::Browser => codex_app_server_protocol::LoginAccountParams::Chatgpt {
            codex_streamlined_login: false,
            use_hosted_login_success_page: false,
            app_brand: None,
        },
        LoginMode::DeviceCode => codex_app_server_protocol::LoginAccountParams::ChatgptDeviceCode,
    };
    let response: LoginAccountResponse = request(
        client,
        ClientRequest::LoginAccount {
            request_id: request_id("marathon-login"),
            params,
        },
    )
    .await?;

    let login_id = match response {
        LoginAccountResponse::Chatgpt { login_id, auth_url } => {
            println!("Open this URL to sign in:\n{auth_url}");
            login_id
        }
        LoginAccountResponse::ChatgptDeviceCode {
            login_id,
            verification_url,
            user_code,
        } => {
            println!(
                "Open {verification_url} on another machine and enter this one-time code:\n{user_code}"
            );
            login_id
        }
        other => anyhow::bail!("native ChatGPT login returned unexpected response: {other:?}"),
    };

    let completion = tokio::time::timeout(
        Duration::from_secs(10 * 60),
        wait_for_login_completion(client, &login_id),
    )
    .await
    .map_err(|_| anyhow::anyhow!("native login timed out"))??;
    if !completion.success {
        anyhow::bail!(
            "native login failed{}",
            completion
                .error
                .as_deref()
                .map(|error| format!(": {error}"))
                .unwrap_or_default()
        );
    }

    let imported: MarathonImportResponse = request(
        client,
        ClientRequest::MarathonImport {
            request_id: request_id("marathon-login-import"),
            params: MarathonImportParams { alias },
        },
    )
    .await?;
    println!(
        "Login completed; imported native account `{}` as `{}`.",
        imported.account_id, imported.alias
    );
    Ok(())
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum LoginMode {
    Browser,
    DeviceCode,
}

fn choose_login_mode() -> anyhow::Result<LoginMode> {
    if !(std::io::stdin().is_terminal() && std::io::stderr().is_terminal()) {
        anyhow::bail!(
            "choose a login flow explicitly when no terminal is attached: use --browser or --device-code"
        );
    }

    eprintln!("Choose how to sign in the Marathon account:");
    eprintln!("  1) Browser link (this machine)");
    eprintln!("  2) Device/auth code (headless server or another machine)");
    eprint!("Choice [1]: ");
    std::io::stderr().flush()?;
    let mut choice = String::new();
    std::io::stdin().read_line(&mut choice)?;
    match choice.trim() {
        "" | "1" => Ok(LoginMode::Browser),
        "2" => Ok(LoginMode::DeviceCode),
        _ => anyhow::bail!("invalid choice; enter 1 for browser link or 2 for device/auth code"),
    }
}

async fn wait_for_login_completion(
    client: &mut InProcessAppServerClient,
    login_id: &str,
) -> anyhow::Result<codex_app_server_protocol::AccountLoginCompletedNotification> {
    let mut completion = None;
    loop {
        let event = client
            .next_event()
            .await
            .ok_or_else(|| anyhow::anyhow!("native app-server disconnected during login"))?;
        let InProcessServerEvent::ServerNotification(notification) = event else {
            continue;
        };
        match notification.as_ref() {
            ServerNotification::AccountLoginCompleted(login_completion)
                if login_completion.login_id.as_deref() == Some(login_id) =>
            {
                if !login_completion.success {
                    return Ok(login_completion.clone());
                }
                completion = Some(login_completion.clone());
            }
            // The app-server emits completion before it reloads AuthManager,
            // then emits AccountUpdated. Waiting for this second notification
            // ensures `marathon/import` snapshots the new native identity.
            ServerNotification::AccountUpdated(_) if completion.is_some() => {
                return Ok(completion.take().expect("completion was checked"));
            }
            _ => {}
        }
    }
}

async fn request<T: DeserializeOwned>(
    client: &InProcessAppServerClient,
    request: ClientRequest,
) -> anyhow::Result<T> {
    client
        .request_typed(request)
        .await
        .map_err(|error| anyhow::anyhow!(error.to_string()))
}

fn request_id(label: &str) -> RequestId {
    RequestId::String(label.to_string())
}

fn print_status(status: &MarathonStatusResponse) {
    println!(
        "Native Marathon: {}",
        if status.enabled {
            "enabled"
        } else {
            "disabled"
        }
    );
    println!(
        "Active account: {}",
        status.active_account_id.as_deref().unwrap_or("none")
    );
    println!(
        "Automatic quota reset: {} (phase: {})",
        if status.auto_reset_enabled {
            "enabled"
        } else {
            "disabled"
        },
        status.auto_reset_phase
    );
    if let Some(error) = &status.auto_reset_last_error {
        println!("Automatic quota reset last result: {error}");
    }
    println!("Accounts: {}", status.accounts.len());
    for account in &status.accounts {
        print_account(account);
    }
}

fn print_account(account: &MarathonAccount) {
    let marker = if account.active { "*" } else { " " };
    println!(
        "  {marker} {} ({}) — credential {}, {}, weekly quota {}, reset action {}",
        account.alias,
        account.account_id,
        if account.credential_present {
            "present"
        } else {
            "missing"
        },
        account.credential_health,
        account.weekly_quota_remaining_percent.map_or_else(
            || "unknown".to_string(),
            |value| format!("{value:.1}% remaining")
        ),
        if account.reset_action_available {
            "available"
        } else {
            "unavailable"
        }
    );
}

fn print_switch(response: &MarathonSwitchResponse) -> anyhow::Result<()> {
    match response.outcome {
        MarathonSwitchOutcome::Committed => {
            println!(
                "Switched native Codex account to {}.",
                response.account_id.as_deref().unwrap_or("unknown")
            );
            Ok(())
        }
        MarathonSwitchOutcome::Deferred => anyhow::bail!(
            "switch deferred until active turns finish{}",
            response
                .reason
                .as_deref()
                .map(|reason| format!(": {reason}"))
                .unwrap_or_default()
        ),
        MarathonSwitchOutcome::Rejected => anyhow::bail!(
            "switch rejected{}",
            response
                .reason
                .as_deref()
                .map(|reason| format!(": {reason}"))
                .unwrap_or_default()
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bare_command_defaults_to_status() {
        let command = MarathonCommand::try_parse_from(["marathon"]).expect("valid command");
        assert!(command.action.is_none());
    }

    #[test]
    fn parses_account_actions() {
        let command =
            MarathonCommand::try_parse_from(["marathon", "import", "work"]).expect("valid command");
        assert!(matches!(
            command.action,
            Some(MarathonAction::Import { alias }) if alias == "work"
        ));

        let command = MarathonCommand::try_parse_from(["marathon", "switch", "personal"])
            .expect("valid command");
        assert!(matches!(
            command.action,
            Some(MarathonAction::Switch { target }) if target == "personal"
        ));

        let command = MarathonCommand::try_parse_from(["marathon", "auto-reset", "on"])
            .expect("valid command");
        assert!(matches!(
            command.action,
            Some(MarathonAction::AutoReset {
                action: AutoResetAction::On
            })
        ));
    }

    #[test]
    fn parses_encrypted_backup_actions() {
        let command = MarathonCommand::try_parse_from([
            "marathon",
            "backup",
            "export",
            "--output",
            "accounts.age",
            "--account",
            "account-a",
            "--account",
            "account-b",
            "--overwrite",
        ])
        .expect("valid backup export");
        let Some(MarathonAction::Backup {
            action: BackupAction::Export(args),
        }) = command.action
        else {
            panic!("expected backup export");
        };
        assert_eq!(args.output, PathBuf::from("accounts.age"));
        assert_eq!(
            args.account,
            vec!["account-a".to_string(), "account-b".to_string()]
        );
        assert!(args.overwrite);

        let command = MarathonCommand::try_parse_from([
            "marathon",
            "backup",
            "import",
            "accounts.age",
            "--conflict",
            "rename",
            "--dry-run",
        ])
        .expect("valid backup import");
        assert!(matches!(
            command.action,
            Some(MarathonAction::Backup {
                action: BackupAction::Import(BackupImportArgs {
                    conflict: BackupConflict::Rename,
                    dry_run: true,
                    ..
                })
            })
        ));

        assert!(
            MarathonCommand::try_parse_from([
                "marathon",
                "backup",
                "import",
                "accounts.age",
                "--dry-run",
                "--yes",
            ])
            .is_err()
        );

        assert!(
            MarathonCommand::try_parse_from([
                "marathon",
                "backup",
                "export",
                "--output",
                "accounts.age",
                "--all",
                "--account",
                "account-a",
            ])
            .is_err()
        );
    }

    #[test]
    fn backup_selection_helpers_are_secret_free() {
        let accounts = vec![
            transfer::AccountSummary {
                id: "account-b".to_string(),
                alias: "work".to_string(),
            },
            transfer::AccountSummary {
                id: "account-a".to_string(),
                alias: "personal".to_string(),
            },
        ];
        assert_eq!(
            account_ids(&accounts),
            vec!["account-b".to_string(), "account-a".to_string()]
        );
        assert_eq!(plural_suffix(1), "");
        assert_eq!(plural_suffix(2), "s");

        assert!(replacement_includes_account(&accounts, "account-a"));
        assert!(!replacement_includes_account(&accounts, "account-c"));
    }
}
