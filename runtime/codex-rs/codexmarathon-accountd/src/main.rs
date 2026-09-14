//! `codexmarathon-accountd` executable entry point.

use clap::Parser;
use codexmarathon_accountd::{
    AccountDaemon, AccountdError, DaemonListener, default_quota_db_path, sd_notify,
};
use std::path::PathBuf;
use std::time::Duration;

const DEFAULT_REQUEST_TIMEOUT_MS: u64 = 5_000;

#[derive(Debug, Parser)]
#[command(
    name = "codexmarathon-accountd",
    version,
    about = "Serve owner-only CodexMarathon account metadata over a Unix socket"
)]
struct Args {
    /// Codex home used to derive the default quota database path.
    #[arg(long, value_name = "PATH")]
    codex_home: Option<PathBuf>,

    /// Metadata-only quota database. Defaults to $CODEX_HOME/marathon/quota.sqlite.
    #[arg(long, value_name = "PATH")]
    quota_db: Option<PathBuf>,

    /// Foreground/development Unix socket path. Systemd socket activation is preferred.
    #[arg(long, value_name = "PATH")]
    socket: Option<PathBuf>,

    /// Maximum time for one request read, handler operation, or response write.
    #[arg(long, default_value_t = DEFAULT_REQUEST_TIMEOUT_MS, value_name = "MILLISECONDS")]
    request_timeout_ms: u64,
}

#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!("codexmarathon-accountd: {}", error.public_message());
        std::process::exit(1);
    }
}

async fn run() -> Result<(), AccountdError> {
    let args = Args::parse();
    let timeout = Duration::from_millis(args.request_timeout_ms);
    if timeout.is_zero() {
        return Err(AccountdError::InvalidConfig(
            "request timeout must be positive",
        ));
    }
    let quota_db_path = match args.quota_db {
        Some(path) => path,
        None => default_quota_db_path(args.codex_home)?,
    };
    let daemon = AccountDaemon::open(quota_db_path, timeout).await?;
    let listener = DaemonListener::from_systemd_or_path(args.socket).await?;
    match sd_notify("READY=1") {
        Ok(true) => {}
        Ok(false) => {}
        Err(error) => {
            eprintln!(
                "codexmarathon-accountd: sd_notify unavailable: {}",
                error.public_message()
            );
        }
    }
    daemon.serve(listener, shutdown_signal()).await?;
    let _ = sd_notify("STOPPING=1");
    Ok(())
}

async fn shutdown_signal() {
    #[cfg(unix)]
    {
        let mut terminate =
            match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
                Ok(signal) => signal,
                Err(_) => {
                    let _ = tokio::signal::ctrl_c().await;
                    return;
                }
            };
        let mut interrupt =
            match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::interrupt()) {
                Ok(signal) => signal,
                Err(_) => {
                    let _ = tokio::signal::ctrl_c().await;
                    return;
                }
            };
        tokio::select! {
            _ = terminate.recv() => {}
            _ = interrupt.recv() => {}
            _ = tokio::signal::ctrl_c() => {}
        }
    }
    #[cfg(not(unix))]
    {
        let _ = tokio::signal::ctrl_c().await;
    }
}
