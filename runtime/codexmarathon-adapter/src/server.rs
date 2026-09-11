//! Authenticated-local IPC server for the Marathon adapter.
//!
//! The server is deliberately transport-small: the embedded Codex runtime
//! remains the owner of authentication, turns, transports, and recovery.  A
//! controller connects over a user-owned Unix socket (or a user-scoped
//! Windows named pipe), negotiates v1, and drives the existing adapter.  No
//! TCP listener is provided here.

use crate::adapter::{CodextBackend, RuntimeAdapter};
use crate::error::AdapterError;
use std::io;
#[cfg(unix)]
use std::os::unix::fs::FileTypeExt;
#[cfg(unix)]
use std::path::Path;
use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt};
#[cfg(unix)]
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::{Mutex, Notify};
use tokio::time::{self, Duration, MissedTickBehavior};

/// Poll cadence for native turn/rate-limit observations.  Native Codex
/// integrations may call the forwarding methods directly for lower latency;
/// this bounded poll keeps safe-boundary and quota events live when a native
/// event hook is unavailable.
pub const DEFAULT_POLL_INTERVAL: Duration = Duration::from_secs(5);

/// Configuration for a local Marathon listener.
#[derive(Clone, Debug)]
pub struct ServerConfig {
    pub poll_interval: Duration,
    pub max_line_bytes: usize,
}

impl Default for ServerConfig {
    fn default() -> Self {
        Self {
            poll_interval: DEFAULT_POLL_INTERVAL,
            max_line_bytes: crate::framing::DEFAULT_MAX_FRAME_BYTES,
        }
    }
}

/// A local runtime server around one embedded adapter.  The adapter state is
/// retained across reconnects; only protocol negotiation is reset between
/// connections.  This is what lets a controller restart without losing a
/// prepared transition.
pub struct RuntimeServer<B> {
    adapter: Arc<Mutex<RuntimeAdapter<B>>>,
    config: ServerConfig,
    shutdown: Arc<Notify>,
    shutdown_requested: Arc<AtomicBool>,
}

impl<B: CodextBackend + Send + 'static> RuntimeServer<B> {
    pub fn new(adapter: RuntimeAdapter<B>, config: ServerConfig) -> Self {
        Self {
            adapter: Arc::new(Mutex::new(adapter)),
            config,
            shutdown: Arc::new(Notify::new()),
            shutdown_requested: Arc::new(AtomicBool::new(false)),
        }
    }

    /// Borrow the shared adapter for native event forwarding.
    pub fn adapter(&self) -> Arc<Mutex<RuntimeAdapter<B>>> {
        Arc::clone(&self.adapter)
    }

    /// Request a graceful listener shutdown.  The current client session is
    /// allowed to flush its response before the accept loop exits.
    pub fn shutdown(&self) {
        self.shutdown_requested.store(true, Ordering::Release);
        self.shutdown.notify_waiters();
    }

    fn is_shutdown_requested(&self) -> bool {
        self.shutdown_requested.load(Ordering::Acquire)
    }

    /// Serve a user-scoped Unix socket until [`Self::shutdown`] is called.
    ///
    /// Existing paths are removed only when they are Unix sockets.  A regular
    /// file, directory, or symlink at the requested path is rejected so a
    /// stale endpoint can never cause an unrelated file to be overwritten.
    #[cfg(unix)]
    pub async fn serve_unix(&self, socket_path: impl Into<PathBuf>) -> io::Result<()> {
        let socket_path = socket_path.into();
        prepare_unix_socket(&socket_path)?;
        let listener = UnixListener::bind(&socket_path)?;
        let result = self.accept_unix(listener).await;
        let _ = std::fs::remove_file(&socket_path);
        result
    }

    #[cfg(not(unix))]
    pub async fn serve_unix(&self, _socket_path: impl Into<PathBuf>) -> io::Result<()> {
        Err(io::Error::new(
            io::ErrorKind::Unsupported,
            "Unix sockets are unavailable on this platform",
        ))
    }

    /// Serve the canonical user-scoped Windows named pipe.  Each connection
    /// gets a fresh pipe instance so a controller can reconnect after a
    /// transport loss.  The embedding app must provision the pipe DACL for
    /// the current Windows user; the generated endpoint contains a stable
    /// user hash and is never published on a network interface.
    #[cfg(windows)]
    pub async fn serve_named_pipe(&self, pipe_path: impl Into<String>) -> io::Result<()> {
        use tokio::io::BufReader;
        use tokio::net::windows::named_pipe::ServerOptions;

        let pipe_path = pipe_path.into();
        let mut first = true;
        loop {
            if self.is_shutdown_requested() {
                return Ok(());
            }
            let mut options = ServerOptions::new();
            // Keep the named pipe local even if another process learns the
            // endpoint name. The stable user-scoped name is an additional
            // identity boundary; this flag rejects remote clients at the
            // Windows transport layer.
            options.reject_remote_clients(true);
            if first {
                options.first_pipe_instance(true);
                first = false;
            }
            let mut server = options.create(&pipe_path)?;
            tokio::select! {
                connected = server.connect() => {
                    connected?;
                    let (reader, writer) = tokio::io::split(server);
                    if self
                        .serve_stream(BufReader::new(reader), writer)
                        .await
                        .is_err()
                    {
                        // A malformed frame or a peer write failure belongs
                        // to this connection, not to the listener.  Reset
                        // only connection-scoped negotiation and accept a
                        // fresh controller on the next pipe instance.
                        self.reset_connection().await;
                    }
                }
                _ = self.shutdown.notified() => return Ok(()),
            }
        }
    }

    #[cfg(not(windows))]
    pub async fn serve_named_pipe(&self, _pipe_path: impl Into<String>) -> io::Result<()> {
        Err(io::Error::new(
            io::ErrorKind::Unsupported,
            "Windows named pipes are unavailable on this platform",
        ))
    }

    #[cfg(unix)]
    async fn accept_unix(&self, listener: UnixListener) -> io::Result<()> {
        loop {
            if self.is_shutdown_requested() {
                return Ok(());
            }
            tokio::select! {
                accepted = listener.accept() => {
                    let (stream, _) = accepted?;
                    // A single controller owns a runtime session.  Process
                    // one connection at a time so adapter mutation and
                    // transition intent cannot race across clients.
                    if self.serve_unix_stream(stream).await.is_err() {
                        self.reset_connection().await;
                    }
                }
                _ = self.shutdown.notified() => return Ok(()),
            }
        }
    }

    #[cfg(unix)]
    async fn serve_unix_stream(&self, stream: UnixStream) -> io::Result<()> {
        let (reader, writer) = stream.into_split();
        self.serve_stream(tokio::io::BufReader::new(reader), writer)
            .await
    }

    /// Serve one already-connected stream.  This is public for Windows
    /// named-pipe acceptors and in-process test transports.
    pub async fn serve_stream<R, W>(&self, reader: R, writer: W) -> io::Result<()>
    where
        R: tokio::io::AsyncBufRead + Unpin,
        W: tokio::io::AsyncWrite + Unpin,
    {
        let mut reader = reader;
        let mut writer = writer;
        let mut line = String::new();
        let mut poll = time::interval(self.config.poll_interval.max(Duration::from_millis(10)));
        poll.set_missed_tick_behavior(MissedTickBehavior::Delay);
        loop {
            if self.is_shutdown_requested() {
                break;
            }
            line.clear();
            tokio::select! {
                read = reader.read_line(&mut line) => {
                    let read = read?;
                    if read == 0 {
                        break;
                    }
                    if line.len() > self.config.max_line_bytes {
                        return Err(io::Error::new(io::ErrorKind::InvalidData, "runtime frame exceeds configured limit"));
                    }
                    let response_and_events = {
                        let mut adapter = self.adapter.lock().await;
                        // Native Codext emits recovery_parked at the moment
                        // it creates its existing pending synthetic turn.
                        // Drain first so a same-tick recovery/release request
                        // cannot race an empty adapter recovery map.
                        adapter
                            .drain_native_recovery_events()
                            .map_err(adapter_error_to_io)?;
                        let response = adapter
                            .handle_frame(line.as_bytes())
                            .map_err(adapter_error_to_io)?;
                        let events = adapter
                            .drain_event_lines()
                            .map_err(adapter_error_to_io)?;
                        (response, events)
                    };
                    writer.write_all(&response_and_events.0).await?;
                    for event in response_and_events.1 {
                        writer.write_all(&event).await?;
                    }
                    writer.flush().await?;
                }
                _ = poll.tick() => {
                    let events = {
                        let mut adapter = self.adapter.lock().await;
                        adapter
                            .drain_native_recovery_events()
                            .map_err(adapter_error_to_io)?;
                        if adapter.protocol_ready() {
                            // A rate-limit read may legitimately be
                            // unavailable for API-key/local providers.  The
                            // adapter records that as a diagnostic error, but
                            // it must not tear down the authenticated IPC
                            // session; native integrations can later forward
                            // a provider snapshot explicitly.
                            let _ = adapter.observe_turn_count();
                            let _ = adapter.read_and_forward_rate_limits();
                        }
                        adapter.drain_event_lines().map_err(adapter_error_to_io)?
                    };
                    for event in events {
                        writer.write_all(&event).await?;
                    }
                    writer.flush().await?;
                }
                _ = self.shutdown.notified() => break,
            }
        }
        let mut adapter = self.adapter.lock().await;
        adapter.reset_connection();
        Ok(())
    }

    async fn reset_connection(&self) {
        let mut adapter = self.adapter.lock().await;
        adapter.reset_connection();
    }
}

fn adapter_error_to_io(error: AdapterError) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, error.to_string())
}

#[cfg(unix)]
fn prepare_unix_socket(path: &Path) -> io::Result<()> {
    let parent = path.parent().ok_or_else(|| {
        io::Error::new(
            io::ErrorKind::InvalidInput,
            "Unix socket has no parent directory",
        )
    })?;
    std::fs::create_dir_all(parent)?;
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(parent, std::fs::Permissions::from_mode(0o700))?;
    match std::fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_socket() => {
            // Never unlink a live listener owned by another Marathon
            // process. A refused connection identifies a stale socket left
            // by a crashed process and is the only removable case.
            match std::os::unix::net::UnixStream::connect(path) {
                Ok(_) => Err(io::Error::new(
                    io::ErrorKind::AlreadyExists,
                    "runtime socket is already serving another process",
                )),
                Err(error)
                    if matches!(
                        error.kind(),
                        io::ErrorKind::ConnectionRefused | io::ErrorKind::NotFound
                    ) =>
                {
                    std::fs::remove_file(path)
                }
                Err(error) => Err(error),
            }
        }
        Ok(_) => Err(io::Error::new(
            io::ErrorKind::AlreadyExists,
            "runtime socket path exists and is not a Unix socket",
        )),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error),
    }
}

/// Canonicalize a user-scoped named-pipe endpoint for the Windows listener.
/// The caller must create the pipe with a DACL limited to the current user.
#[cfg(windows)]
pub fn canonical_named_pipe_path(name: &str) -> io::Result<String> {
    let name = name.trim().trim_matches(['/', '\\']);
    if name.is_empty() || name.contains("..") || name.contains(':') {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "invalid named-pipe name",
        ));
    }
    Ok(format!(r"\\.\pipe\{name}"))
}
