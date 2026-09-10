//! Small filesystem helpers shared by the native domain stores.

use crate::errors::{DomainError, DomainResult};
use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

/// Ensure that a persistence directory is private to its owner.
pub(crate) fn ensure_private_dir(path: &Path) -> DomainResult<()> {
    fs::create_dir_all(path)?;
    set_private_permissions(path, true)?;
    Ok(())
}

/// Reject an existing symlink or directory at a file path.
pub(crate) fn reject_unsafe_file(path: &Path) -> DomainResult<()> {
    let metadata = match fs::symlink_metadata(path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(error.into()),
    };
    if metadata.file_type().is_symlink() || metadata.is_dir() {
        return Err(DomainError::UnsafePath);
    }
    Ok(())
}

/// Write a file through a same-directory temporary and rename it into place.
///
/// The temporary filename is intentionally uninteresting and contains no
/// credential data. A process-local timestamp and PID are sufficient because
/// callers serialize writes for each store; a collision is retried by the
/// exclusive create flag.
pub(crate) fn atomic_write(path: &Path, contents: &[u8]) -> DomainResult<()> {
    let parent = path.parent().ok_or(DomainError::UnsafePath)?;
    ensure_private_dir(parent)?;
    reject_unsafe_file(path)?;

    let stamp = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |duration| duration.as_nanos());
    let pid = std::process::id();
    let mut temporary = PathBuf::from(path);
    let extension = format!("tmp-{pid}-{stamp}");
    temporary.set_extension(extension);

    let mut file = OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(&temporary)?;
    set_private_permissions_file(&file)?;
    if let Err(error) = file.write_all(contents).and_then(|_| file.sync_all()) {
        drop(file);
        let _ = fs::remove_file(&temporary);
        return Err(error.into());
    }
    drop(file);

    if let Err(error) = fs::rename(&temporary, path) {
        let _ = fs::remove_file(&temporary);
        return Err(error.into());
    }
    let final_file = File::options().write(true).open(path)?;
    set_private_permissions_file(&final_file)?;
    final_file.sync_all()?;
    Ok(())
}

/// Tighten a file or directory's Unix mode where mode bits are meaningful.
/// Windows ACLs are managed by the platform and are left untouched.
pub(crate) fn set_private_permissions(value: &Path, directory: bool) -> DomainResult<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mode = if directory { 0o700 } else { 0o600 };
        let metadata = fs::metadata(value)?;
        let mut permissions = metadata.permissions();
        permissions.set_mode(mode);
        fs::set_permissions(value, permissions)?;
    }
    #[cfg(not(unix))]
    {
        let _ = (value, directory);
    }
    Ok(())
}

/// Apply private permissions to an already-open file descriptor.
pub(crate) fn set_private_permissions_file(file: &File) -> DomainResult<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mut permissions = file.metadata()?.permissions();
        permissions.set_mode(0o600);
        file.set_permissions(permissions)?;
    }
    Ok(())
}
