//! Production wiring for the embedded CodexMarathon control boundary.
//!
//! The app-server remains the owner of authentication, threads, model
//! transports, and recovery.  This module only exposes the already-created
//! authorities through the small Marathon adapter when the launcher supplies
//! `CODEXMARATHON_LISTEN`.  The listener is local-only: Unix sockets on Unix
//! and a user-scoped named pipe on Windows.  No TCP fallback is provided.

use crate::message_processor::MessageProcessor;
use codex_core::config::Config;
use codexmarathon_runtime::{
    CodexNativeRuntime, EmbeddedRuntime, NativeAuthConfig, NativeBackend, RuntimeServer,
    ServerConfig,
};
use std::io::{self, ErrorKind};
#[cfg(unix)]
use std::path::PathBuf;
use std::sync::Arc;
use tokio::task::JoinHandle;
use url::Url;

const LISTEN_ENV: &str = "CODEXMARATHON_LISTEN";

pub(crate) struct CodexMarathonServerHandle {
    server: Arc<RuntimeServer<NativeBackend<CodexNativeRuntime>>>,
    task: JoinHandle<()>,
}

impl CodexMarathonServerHandle {
    pub(crate) async fn shutdown(self) {
        self.server.shutdown();
        let _ = self.task.await;
    }
}

/// Start the embedded Marathon listener if the launcher requested one.
///
/// Construction happens only after `MessageProcessor` has been assembled, so
/// the native bridge receives the exact AuthManager, ThreadManager, turn
/// watcher, and auth-transition lock used by Codex's normal request path.
pub(crate) fn spawn(
    processor: &Arc<MessageProcessor>,
    config: &Arc<Config>,
) -> Option<CodexMarathonServerHandle> {
    let endpoint = std::env::var(LISTEN_ENV)
        .ok()
        .map(|value| value.trim().to_string())
        .filter(|value| !value.is_empty())?;

    let mut auth_config = NativeAuthConfig::new(
        config.codex_home.to_path_buf(),
        config.auth_route_config(),
    )
    .with_http_client_factory(config.http_client_factory());
    auth_config.forced_chatgpt_workspace_id = config.forced_chatgpt_workspace_id.clone();
    auth_config.chatgpt_base_url = Some(config.chatgpt_base_url.clone());

    let native = match processor.codexmarathon_native_runtime(auth_config) {
        Ok(native) => native,
        Err(error) => {
            tracing::error!(error = %error, "failed to attach embedded CodexMarathon native runtime");
            return None;
        }
    };
    let embedded = match EmbeddedRuntime::new(
        native,
        format!("codexmarathon-runtime-{}", std::process::id()),
    ) {
        Ok(embedded) => embedded,
        Err(error) => {
            tracing::error!(error = %error, "failed to construct embedded CodexMarathon adapter");
            return None;
        }
    };
    let server = Arc::new(embedded.into_server(ServerConfig::default()));
    let server_for_task = Arc::clone(&server);
    let endpoint_for_task = endpoint.clone();
    let task = tokio::spawn(async move {
        if let Err(error) = serve_endpoint(&server_for_task, &endpoint_for_task).await {
            tracing::error!(endpoint = %redact_endpoint(&endpoint_for_task), error = %error, "embedded CodexMarathon listener stopped");
        }
    });
    Some(CodexMarathonServerHandle { server, task })
}

async fn serve_endpoint(
    server: &RuntimeServer<NativeBackend<CodexNativeRuntime>>,
    endpoint: &str,
) -> io::Result<()> {
    let parsed = Url::parse(endpoint).map_err(|error| {
        io::Error::new(
            ErrorKind::InvalidInput,
            format!("invalid CODEXMARATHON_LISTEN endpoint: {error}"),
        )
    })?;
    match parsed.scheme().to_ascii_lowercase().as_str() {
        "unix" => {
            #[cfg(unix)]
            {
                if parsed.host_str().is_some_and(|host| !host.is_empty()) {
                    return Err(io::Error::new(
                        ErrorKind::InvalidInput,
                        "Unix Marathon endpoint must not include a host",
                    ));
                }
                let path = PathBuf::from(parsed.path());
                if path.as_os_str().is_empty() {
                    return Err(io::Error::new(
                        ErrorKind::InvalidInput,
                        "Unix Marathon endpoint path is empty",
                    ));
                }
                return server.serve_unix(path).await;
            }
            #[cfg(not(unix))]
            {
                return Err(io::Error::new(
                    ErrorKind::Unsupported,
                    "Unix Marathon endpoints are unavailable on this platform",
                ));
            }
        }
        "pipe" | "npipe" | "namedpipe" => {
            #[cfg(windows)]
            {
                let name = parsed
                    .path()
                    .trim_matches('/')
                    .strip_prefix("pipe/")
                    .unwrap_or_else(|| parsed.path().trim_matches('/'));
                let name = if name.is_empty() {
                    parsed.host_str().unwrap_or_default()
                } else {
                    name
                };
                if name.is_empty() || name.contains("..") || name.contains(':') {
                    return Err(io::Error::new(
                        ErrorKind::InvalidInput,
                        "invalid Windows Marathon named-pipe name",
                    ));
                }
                return server
                    .serve_named_pipe(format!(r"\\.\pipe\{name}"))
                    .await;
            }
            #[cfg(not(windows))]
            {
                return Err(io::Error::new(
                    ErrorKind::Unsupported,
                    "Windows named-pipe endpoints are unavailable on this platform",
                ));
            }
        }
        "tcp" => Err(io::Error::new(
            ErrorKind::PermissionDenied,
            "TCP Marathon endpoints are disabled; use a user-scoped local endpoint",
        )),
        scheme => Err(io::Error::new(
            ErrorKind::InvalidInput,
            format!("unsupported Marathon endpoint scheme {scheme:?}"),
        )),
    }
}

fn redact_endpoint(endpoint: &str) -> String {
    Url::parse(endpoint)
        .map(|url| {
            let mut safe = url;
            safe.set_query(None);
            safe.set_fragment(None);
            safe.to_string()
        })
        .unwrap_or_else(|_| "<invalid endpoint>".to_string())
}
