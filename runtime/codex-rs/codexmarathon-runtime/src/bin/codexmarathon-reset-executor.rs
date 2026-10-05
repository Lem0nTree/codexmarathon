use codexmarathon_accountd_client::AccountdClient;
use codexmarathon_runtime::expiry_executor::ExpiryExecutor;
use std::path::PathBuf;

fn main() {
    if run().is_err() {
        eprintln!("codexmarathon-reset-executor: configuration or runtime unavailable");
        std::process::exit(1);
    }
}

fn run() -> Result<(), ()> {
    let mut args = std::env::args().skip(1);
    let mut home = None;
    let mut socket = None;
    while let Some(argument) = args.next() {
        match argument.as_str() {
            "--codex-home" => home = Some(PathBuf::from(args.next().ok_or(())?)),
            "--socket" => socket = Some(PathBuf::from(args.next().ok_or(())?)),
            "--help" | "-h" => {
                println!("codexmarathon-reset-executor [--codex-home PATH] [--socket PATH]");
                return Ok(());
            }
            _ => return Err(()),
        }
    }
    let resolved = match home {
        Some(home) => codexmarathon_home::resolve_explicit(home),
        None => codexmarathon_home::resolve(),
    }
    .map_err(|_| ())?;
    let daemon = match socket {
        Some(socket) => AccountdClient::new(socket),
        None => AccountdClient::from_default_socket().map_err(|_| ())?,
    };
    let executor =
        ExpiryExecutor::new(resolved.codex_home().to_path_buf(), daemon).map_err(|_| ())?;
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .map_err(|_| ())?
        .block_on(executor.run())
        .map_err(|_| ())
}
