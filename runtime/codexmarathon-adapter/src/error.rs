use std::error::Error;
use std::fmt;

/// A backend failure is intentionally code-plus-message only.  A Codext
/// integration can map its native error without exposing auth material on the
/// Marathon wire.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BackendError {
    pub code: String,
    pub message: String,
}

impl BackendError {
    pub fn new(code: impl Into<String>, message: impl Into<String>) -> Self {
        Self {
            code: code.into(),
            message: message.into(),
        }
    }
}

impl fmt::Display for BackendError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(formatter, "{}: {}", self.code, self.message)
    }
}

impl Error for BackendError {}

#[derive(Debug)]
pub enum AdapterError {
    InvalidParams(String),
    Protocol(String),
    Backend(BackendError),
    Serialization(serde_json::Error),
    Framing(String),
    Io(std::io::Error),
}

impl fmt::Display for AdapterError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidParams(message) => write!(formatter, "invalid params: {message}"),
            Self::Protocol(message) => write!(formatter, "protocol error: {message}"),
            Self::Backend(error) => write!(formatter, "backend error: {error}"),
            Self::Serialization(error) => write!(formatter, "serialization error: {error}"),
            Self::Framing(message) => write!(formatter, "framing error: {message}"),
            Self::Io(error) => write!(formatter, "I/O error: {error}"),
        }
    }
}

impl Error for AdapterError {}

impl From<BackendError> for AdapterError {
    fn from(error: BackendError) -> Self {
        Self::Backend(error)
    }
}

impl From<serde_json::Error> for AdapterError {
    fn from(error: serde_json::Error) -> Self {
        Self::Serialization(error)
    }
}

impl From<std::io::Error> for AdapterError {
    fn from(error: std::io::Error) -> Self {
        Self::Io(error)
    }
}
