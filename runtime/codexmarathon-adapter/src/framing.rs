//! Newline-delimited JSON framing used by the local Marathon IPC seam.

use serde::de::DeserializeOwned;
use serde::Serialize;
use std::io::{BufRead, Write};

pub const DEFAULT_MAX_FRAME_BYTES: usize = 4 * 1024 * 1024;

#[derive(Debug)]
pub enum FramingError {
    Io(std::io::Error),
    Json(serde_json::Error),
    Oversized { max_bytes: usize },
}

impl std::fmt::Display for FramingError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Io(error) => write!(formatter, "I/O error: {error}"),
            Self::Json(error) => write!(formatter, "JSON error: {error}"),
            Self::Oversized { max_bytes } => {
                write!(formatter, "JSON frame exceeds {max_bytes} bytes")
            }
        }
    }
}

impl std::error::Error for FramingError {}

impl From<std::io::Error> for FramingError {
    fn from(error: std::io::Error) -> Self {
        Self::Io(error)
    }
}

impl From<serde_json::Error> for FramingError {
    fn from(error: serde_json::Error) -> Self {
        Self::Json(error)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct JsonLineCodec {
    max_frame_bytes: usize,
}

impl Default for JsonLineCodec {
    fn default() -> Self {
        Self {
            max_frame_bytes: DEFAULT_MAX_FRAME_BYTES,
        }
    }
}

impl JsonLineCodec {
    pub fn new(max_frame_bytes: usize) -> Result<Self, FramingError> {
        if max_frame_bytes == 0 {
            return Err(FramingError::Oversized { max_bytes: 0 });
        }
        Ok(Self { max_frame_bytes })
    }

    pub fn max_frame_bytes(&self) -> usize {
        self.max_frame_bytes
    }

    /// Reads one non-empty line.  The delimiter is included in the size
    /// limit, and a final unterminated line is accepted for test pipes and
    /// graceful EOF handling.
    pub fn read_frame<R: BufRead>(
        &self,
        reader: &mut R,
    ) -> Result<Option<Vec<u8>>, FramingError> {
        loop {
            let mut frame = Vec::new();
            let mut found_delimiter = false;
            loop {
                let available = reader.fill_buf()?;
                if available.is_empty() {
                    break;
                }
                if let Some(delimiter) = available.iter().position(|byte| *byte == b'\n') {
                    let count = delimiter + 1;
                    if frame.len() + count > self.max_frame_bytes {
                        return Err(FramingError::Oversized {
                            max_bytes: self.max_frame_bytes,
                        });
                    }
                    frame.extend_from_slice(&available[..count]);
                    reader.consume(count);
                    found_delimiter = true;
                    break;
                }
                if frame.len() + available.len() > self.max_frame_bytes {
                    return Err(FramingError::Oversized {
                        max_bytes: self.max_frame_bytes,
                    });
                }
                frame.extend_from_slice(available);
                let count = available.len();
                reader.consume(count);
            }

            if frame.is_empty() && !found_delimiter {
                return Ok(None);
            }
            if frame.last() == Some(&b'\n') {
                frame.pop();
            }
            if frame.last() == Some(&b'\r') {
                frame.pop();
            }
            if frame.iter().all(|byte| byte.is_ascii_whitespace()) {
                continue;
            }
            return Ok(Some(frame));
        }
    }

    pub fn decode<T: DeserializeOwned>(&self, frame: &[u8]) -> Result<T, FramingError> {
        if frame.len() > self.max_frame_bytes {
            return Err(FramingError::Oversized {
                max_bytes: self.max_frame_bytes,
            });
        }
        Ok(serde_json::from_slice(frame)?)
    }

    pub fn encode<T: Serialize>(&self, value: &T) -> Result<Vec<u8>, FramingError> {
        let mut frame = serde_json::to_vec(value)?;
        frame.push(b'\n');
        if frame.len() > self.max_frame_bytes {
            return Err(FramingError::Oversized {
                max_bytes: self.max_frame_bytes,
            });
        }
        Ok(frame)
    }

    pub fn write<T: Serialize, W: Write>(
        &self,
        writer: &mut W,
        value: &T,
    ) -> Result<(), FramingError> {
        let frame = self.encode(value)?;
        writer.write_all(&frame)?;
        Ok(())
    }
}
