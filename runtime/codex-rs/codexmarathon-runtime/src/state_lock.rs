//! Cross-process exclusion for one Marathon state directory.
//!
//! Lock order: native transition mutex, StateLock, store-local mutex. A lock
//! is never removed or renamed. Code already holding it must use `_locked`
//! store methods; acquiring a second descriptor can deadlock even in one process.
use crate::errors::{DomainError, DomainResult};
use crate::persistence::{ensure_private_dir, open_regular};
use std::fs::File;
use std::path::{Path, PathBuf};

pub struct StateLock {
    file: File,
    directory: PathBuf,
    exclusive: bool,
}

impl StateLock {
    pub fn shared(directory: &Path) -> DomainResult<Self> {
        Self::acquire(directory, false)
    }

    pub fn exclusive(directory: &Path) -> DomainResult<Self> {
        Self::acquire(directory, true)
    }

    fn acquire(directory: &Path, exclusive: bool) -> DomainResult<Self> {
        ensure_private_dir(directory)?;
        let file = open_regular(&directory.join("state.lock"), true, true)?;
        // Never block an async executor thread behind a transition that is
        // awaiting native authentication on that same executor. Busy callers
        // retry at their command boundary.
        if exclusive {
            file.try_lock().map_err(std::io::Error::from)?;
        } else {
            file.try_lock_shared().map_err(std::io::Error::from)?;
        }
        Ok(Self {
            file,
            directory: directory.to_path_buf(),
            exclusive,
        })
    }

    pub(crate) fn check(&self, directory: &Path, write: bool) -> DomainResult<()> {
        if self.directory != directory || (write && !self.exclusive) {
            return Err(DomainError::UnsafePath);
        }
        Ok(())
    }
}

impl Drop for StateLock {
    fn drop(&mut self) {
        let _ = self.file.unlock();
    }
}
