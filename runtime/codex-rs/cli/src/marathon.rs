//! Native CodexMarathon command-line controls.
//!
//! This module talks to an embedded app-server client in the same `codex`
//! process.  It deliberately does not know about the legacy Go controller or
//! its Unix socket environment variable; all state changes go through the
//! typed app-server Marathon requests.

use clap::Parser;
use codex_app_server_client::DEFAULT_IN_PROCESS_CHANNEL_CAPACITY;
use codex_app_server_client::InProcessAppServerClient;
use codex_app_server_client::InProcessClientStartArgs;
use codex_app_server_client::InProcessServerEvent;
use codex_app_server_protocol::ClientRequest;
use codex_app_server_protocol::LoginAccountResponse;
use codex_app_server_protocol::MarathonAccount;
use codex_app_server_protocol::MarathonAutoResetSetParams;
use codex_app_server_protocol::MarathonAutoResetSetResponse;
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
use codex_config::LoaderOverrides;
use codex_core::config::ConfigBuilder;
use codex_exec_server::EnvironmentManager;
use codex_feedback::CodexFeedback;
use codex_utils_cli::CliConfigOverrides;
use serde::de::DeserializeOwned;
use std::io::IsTerminal;
use std::io::Write;
use std::sync::Arc;
use std::time::Duration;

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
    Accounts,

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

async fn run_action(
    client: &mut InProcessAppServerClient,
    action: MarathonAction,
) -> anyhow::Result<()> {
    match action {
        MarathonAction::Status | MarathonAction::Accounts => {
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
    }
    Ok(())
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
}
