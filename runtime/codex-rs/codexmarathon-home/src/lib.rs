//! Resolve the Codex home shared by CodexMarathon components.
//!
//! The installer records one selected home in a small owner-private config
//! file.  Components that need to locate Marathon state should use [`resolve`]
//! instead of independently applying `CODEX_HOME` defaults.  The resolver
//! never opens or parses credentials; `auth.json` is only mentioned by the
//! daemon's systemd policy.

use serde::{Deserialize, Serialize};
use std::fs::{self, File, OpenOptions};
use std::io::{self, Write};
use std::path::{Component, Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

/// Current version of the persisted home configuration format.
pub const CONFIG_VERSION: u32 = 1;

/// Directory below the XDG config directory used by CodexMarathon.
pub const CONFIG_DIRECTORY: &str = "codexmarathon";

/// File containing the persisted home selection.
pub const CONFIG_FILE: &str = "config.json";

const MAX_PATH_BYTES: usize = 4096;
const MAX_CONFIG_BYTES: u64 = 16 * 1024;

/// The source that supplied a resolved Codex home.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum HomeSource {
    /// `CODEXMARATHON_CODEX_HOME` was set.
    CodexMarathonEnvironment,
    /// `CODEX_HOME` was set.
    CodexEnvironment,
    /// The installer's persisted selection was used.
    Persisted,
    /// The built-in `$HOME/.codex` default was used.
    Default,
    /// A caller supplied an explicit path (for example, `--codex-home`).
    Programmatic,
}

impl HomeSource {
    /// Return a stable, human-readable source label.
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::CodexMarathonEnvironment => "CODEXMARATHON_CODEX_HOME",
            Self::CodexEnvironment => "CODEX_HOME",
            Self::Persisted => "persisted installer configuration",
            Self::Default => "$HOME/.codex default",
            Self::Programmatic => "programmatic explicit path",
        }
    }
}

impl std::fmt::Display for HomeSource {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(self.as_str())
    }
}

/// A resolved home and the provenance needed to diagnose split-brain state.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ResolvedHome {
    codex_home: PathBuf,
    source: HomeSource,
    persisted_home: Option<PathBuf>,
    config_path: Option<PathBuf>,
}

impl ResolvedHome {
    /// Return the normalized Codex home.
    ///
    /// Components can pass this path directly to their configuration builder.
    /// The modified CLI resolves it as its first process operation and, when
    /// upstream `CODEX_HOME` is absent, installs it into that existing
    /// process-wide contract before any logging, runtime, or worker starts.
    pub fn codex_home(&self) -> &Path {
        &self.codex_home
    }

    /// Return the source used to select [`Self::codex_home`].
    pub const fn source(&self) -> HomeSource {
        self.source
    }

    /// Return the normalized installer selection, when one was available.
    pub fn persisted_home(&self) -> Option<&Path> {
        self.persisted_home.as_deref()
    }

    /// Return the persisted config path considered during resolution.
    ///
    /// An explicit programmatic path intentionally does not inspect ambient
    /// configuration, so its value is `None`.
    pub fn config_path(&self) -> Option<&Path> {
        self.config_path.as_deref()
    }

    /// Whether the selected home differs from the installer's persisted home.
    pub fn is_persisted_mismatch(&self) -> bool {
        self.persisted_home
            .as_ref()
            .is_some_and(|persisted| persisted != &self.codex_home)
    }
}

/// Errors raised while validating, reading, or writing home configuration.
#[derive(Debug, thiserror::Error)]
pub enum HomeError {
    /// A required environment variable was not available.
    #[error("{0} is required to resolve Codex home")]
    MissingEnvironment(&'static str),

    /// A configured path was empty.
    #[error("{setting} must not be empty")]
    EmptyPath { setting: &'static str },

    /// A configured path was not absolute.
    #[error("{setting} must be absolute: {path:?}")]
    RelativePath {
        setting: &'static str,
        path: PathBuf,
    },

    /// A configured path selected the filesystem root.
    #[error("{setting} must not be the filesystem root")]
    RootPath { setting: &'static str },

    /// A configured path contained `.` or `..` as a component.
    #[error("{setting} must not contain . or .. path components: {path:?}")]
    DotPath {
        setting: &'static str,
        path: PathBuf,
    },

    /// A path cannot be represented safely in the persisted JSON contract.
    #[error("{setting} must be valid UTF-8")]
    NonUtf8Path { setting: &'static str },

    /// A path included a control character or a systemd specifier marker.
    #[error("{setting} contains an unsafe character")]
    UnsafePathCharacter { setting: &'static str },

    /// A path exceeded the bounded configuration size.
    #[error("{setting} is too long")]
    PathTooLong { setting: &'static str },

    /// Both supported environment variables were set to different homes.
    #[error(
        "CODEXMARATHON_CODEX_HOME and CODEX_HOME resolve to different paths: {codexmarathon:?} versus {codex:?}"
    )]
    ConflictingEnvironment {
        codexmarathon: PathBuf,
        codex: PathBuf,
    },

    /// The persisted config directory could not be inspected.
    #[error("could not inspect persisted CodexMarathon config")]
    InspectConfigDirectory(#[source] io::Error),

    /// The persisted config file could not be read.
    #[error("could not read persisted CodexMarathon config")]
    ReadConfig(#[source] io::Error),

    /// The persisted config file was not a regular file.
    #[error("persisted CodexMarathon config is not a regular file")]
    ConfigNotRegular,

    /// The persisted config file or directory was a symbolic link.
    #[error("persisted CodexMarathon config must not be a symbolic link")]
    ConfigSymlink,

    /// The persisted config directory was not owner-private.
    #[error("persisted CodexMarathon config directory is not owner-private")]
    ConfigDirectoryNotPrivate,

    /// The persisted config file was not owner-private.
    #[error("persisted CodexMarathon config file is not owner-private")]
    ConfigFileNotPrivate,

    /// The persisted config file was too large.
    #[error("persisted CodexMarathon config is too large")]
    ConfigTooLarge,

    /// The persisted JSON did not match the versioned schema.
    #[error("persisted CodexMarathon config is invalid")]
    ParseConfig(#[source] serde_json::Error),

    /// The persisted JSON used a version this resolver does not understand.
    #[error("persisted CodexMarathon config version {0} is unsupported")]
    UnsupportedConfigVersion(u32),

    /// The config directory could not be created or made private.
    #[error("could not create persisted CodexMarathon config directory")]
    CreateConfigDirectory(#[source] io::Error),

    /// The new config could not be serialized.
    #[error("could not serialize persisted CodexMarathon config")]
    SerializeConfig(#[source] serde_json::Error),

    /// The temporary config file could not be opened or written.
    #[error("could not write persisted CodexMarathon config")]
    WriteConfig(#[source] io::Error),

    /// The temporary config could not atomically replace the old config.
    #[error("could not atomically install persisted CodexMarathon config")]
    ReplaceConfig(#[source] io::Error),
}

#[derive(Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct PersistedConfig {
    version: u32,
    codex_home: PathBuf,
}

/// Resolve the process's shared Codex home.
///
/// Selection precedence is:
///
/// 1. `CODEXMARATHON_CODEX_HOME`;
/// 2. `CODEX_HOME`;
/// 3. the installer's persisted selection;
/// 4. `$HOME/.codex`.
///
/// If both environment variables are present, their normalized paths must be
/// equal.  A malformed persisted file is reported rather than silently
/// falling back to a different store.
pub fn resolve() -> Result<ResolvedHome, HomeError> {
    let codexmarathon_environment = std::env::var_os("CODEXMARATHON_CODEX_HOME");
    let codex_environment = std::env::var_os("CODEX_HOME");
    let has_environment_home = codexmarathon_environment.is_some() || codex_environment.is_some();

    let config_context = match config_path() {
        Ok(path) => {
            let persisted = read_persisted_at(&path)?;
            (Some(path), persisted)
        }
        Err(HomeError::MissingEnvironment("HOME")) if has_environment_home => (None, None),
        Err(error) => return Err(error),
    };

    let default_home = if has_environment_home {
        None
    } else {
        Some(default_codex_home()?)
    };

    resolve_candidates(
        codexmarathon_environment.map(PathBuf::from),
        codex_environment.map(PathBuf::from),
        config_context.1,
        default_home,
        config_context.0,
    )
}

/// Resolve a caller-supplied home without consulting ambient configuration.
///
/// This is used for explicit daemon options such as `--codex-home`; the
/// option therefore remains authoritative even if environment variables or a
/// persisted installer selection disagree.
pub fn resolve_explicit(path: impl AsRef<Path>) -> Result<ResolvedHome, HomeError> {
    Ok(ResolvedHome {
        codex_home: normalize_codex_home(path.as_ref(), "codex_home")?,
        source: HomeSource::Programmatic,
        persisted_home: None,
        config_path: None,
    })
}

/// Return the path of the persisted configuration file.
pub fn config_path() -> Result<PathBuf, HomeError> {
    let config_root = match std::env::var_os("XDG_CONFIG_HOME") {
        Some(value) => normalize_path(Path::new(&value), "XDG_CONFIG_HOME", false)?,
        None => {
            let home = std::env::var_os("HOME").ok_or(HomeError::MissingEnvironment("HOME"))?;
            let home = normalize_path(Path::new(&home), "HOME", false)?;
            normalize_path(&home.join(".config"), "XDG_CONFIG_HOME", false)?
        }
    };
    Ok(config_root.join(CONFIG_DIRECTORY).join(CONFIG_FILE))
}

/// Persist a normalized home selection using the current XDG config location.
///
/// The config directory is made mode `0700`, the file mode is `0600`, and the
/// replacement is performed with a same-directory temporary file and rename.
/// This function only writes the home-selection JSON; it never reads or
/// migrates credentials.
pub fn persist_codex_home(path: impl AsRef<Path>) -> Result<PathBuf, HomeError> {
    let codex_home = normalize_codex_home(path.as_ref(), "codex_home")?;
    let config_path = config_path()?;
    write_persisted_at(&config_path, &codex_home)?;
    Ok(config_path)
}

fn resolve_candidates(
    codexmarathon_environment: Option<PathBuf>,
    codex_environment: Option<PathBuf>,
    persisted_home: Option<PathBuf>,
    default_home: Option<PathBuf>,
    config_path: Option<PathBuf>,
) -> Result<ResolvedHome, HomeError> {
    let codexmarathon_environment = codexmarathon_environment
        .map(|path| normalize_codex_home(&path, "CODEXMARATHON_CODEX_HOME"))
        .transpose()?;
    let codex_environment = codex_environment
        .map(|path| normalize_codex_home(&path, "CODEX_HOME"))
        .transpose()?;
    if let (Some(codexmarathon), Some(codex)) = (&codexmarathon_environment, &codex_environment) {
        if codexmarathon != codex {
            return Err(HomeError::ConflictingEnvironment {
                codexmarathon: codexmarathon.clone(),
                codex: codex.clone(),
            });
        }
    }

    let persisted_home = persisted_home
        .map(|path| normalize_codex_home(&path, "persisted codex_home"))
        .transpose()?;
    let selected = if let Some(path) = codexmarathon_environment {
        (path, HomeSource::CodexMarathonEnvironment)
    } else if let Some(path) = codex_environment {
        (path, HomeSource::CodexEnvironment)
    } else if let Some(path) = persisted_home.as_ref() {
        (path.clone(), HomeSource::Persisted)
    } else if let Some(path) = default_home {
        (
            normalize_codex_home(&path, "default CODEX_HOME")?,
            HomeSource::Default,
        )
    } else {
        return Err(HomeError::MissingEnvironment("HOME"));
    };

    Ok(ResolvedHome {
        codex_home: selected.0,
        source: selected.1,
        persisted_home,
        config_path,
    })
}

fn default_codex_home() -> Result<PathBuf, HomeError> {
    let home = std::env::var_os("HOME").ok_or(HomeError::MissingEnvironment("HOME"))?;
    let home = normalize_path(Path::new(&home), "HOME", false)?;
    normalize_codex_home(&home.join(".codex"), "default CODEX_HOME")
}

fn normalize_codex_home(path: &Path, setting: &'static str) -> Result<PathBuf, HomeError> {
    normalize_path(path, setting, true)
}

fn normalize_path(
    path: &Path,
    setting: &'static str,
    reject_percent: bool,
) -> Result<PathBuf, HomeError> {
    if path.as_os_str().is_empty() {
        return Err(HomeError::EmptyPath { setting });
    }
    let text = path.to_str().ok_or(HomeError::NonUtf8Path { setting })?;
    if text.len() > MAX_PATH_BYTES {
        return Err(HomeError::PathTooLong { setting });
    }
    if text
        .chars()
        .any(|character| character.is_control() || (reject_percent && character == '%'))
    {
        return Err(HomeError::UnsafePathCharacter { setting });
    }
    if !path.is_absolute() {
        return Err(HomeError::RelativePath {
            setting,
            path: path.to_path_buf(),
        });
    }
    if has_dot_component(text) {
        return Err(HomeError::DotPath {
            setting,
            path: path.to_path_buf(),
        });
    }

    let mut normalized = PathBuf::new();
    let mut has_normal_component = false;
    for component in path.components() {
        match component {
            Component::Prefix(_) | Component::RootDir => {
                normalized.push(component.as_os_str());
            }
            Component::Normal(value) => {
                has_normal_component = true;
                normalized.push(value);
            }
            Component::CurDir | Component::ParentDir => {
                return Err(HomeError::DotPath {
                    setting,
                    path: path.to_path_buf(),
                });
            }
        }
    }
    if !has_normal_component {
        return Err(HomeError::RootPath { setting });
    }
    Ok(normalized)
}

fn has_dot_component(path: &str) -> bool {
    let mut component = String::new();
    for character in path.chars() {
        let separator = character == '/' || (cfg!(windows) && character == '\\');
        if separator {
            if component == "." || component == ".." {
                return true;
            }
            component.clear();
        } else {
            component.push(character);
        }
    }
    component == "." || component == ".."
}

fn read_persisted_at(config_path: &Path) -> Result<Option<PathBuf>, HomeError> {
    let config_directory = config_path.parent().ok_or(HomeError::ConfigNotRegular)?;
    let directory_metadata = match fs::symlink_metadata(config_directory) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(HomeError::InspectConfigDirectory(error)),
    };
    if directory_metadata.file_type().is_symlink() {
        return Err(HomeError::ConfigSymlink);
    }
    if !directory_metadata.is_dir() {
        return Err(HomeError::ConfigNotRegular);
    }
    check_private_directory(&directory_metadata)?;

    let file_metadata = match fs::symlink_metadata(config_path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(HomeError::ReadConfig(error)),
    };
    if file_metadata.file_type().is_symlink() {
        return Err(HomeError::ConfigSymlink);
    }
    if !file_metadata.is_file() {
        return Err(HomeError::ConfigNotRegular);
    }
    check_private_file(&file_metadata)?;
    if file_metadata.len() > MAX_CONFIG_BYTES {
        return Err(HomeError::ConfigTooLarge);
    }
    let bytes = fs::read(config_path).map_err(HomeError::ReadConfig)?;
    let config: PersistedConfig = serde_json::from_slice(&bytes).map_err(HomeError::ParseConfig)?;
    if config.version != CONFIG_VERSION {
        return Err(HomeError::UnsupportedConfigVersion(config.version));
    }
    normalize_codex_home(&config.codex_home, "persisted codex_home").map(Some)
}

fn write_persisted_at(config_path: &Path, codex_home: &Path) -> Result<(), HomeError> {
    let config_directory = config_path
        .parent()
        .ok_or(HomeError::CreateConfigDirectory(io::Error::new(
            io::ErrorKind::InvalidInput,
            "config path has no parent",
        )))?;
    ensure_private_directory(config_directory)?;

    let file_metadata = match fs::symlink_metadata(config_path) {
        Ok(metadata) => Some(metadata),
        Err(error) if error.kind() == io::ErrorKind::NotFound => None,
        Err(error) => return Err(HomeError::ReadConfig(error)),
    };
    if let Some(metadata) = file_metadata.as_ref() {
        if metadata.file_type().is_symlink() {
            return Err(HomeError::ConfigSymlink);
        }
        if !metadata.is_file() {
            return Err(HomeError::ConfigNotRegular);
        }
        check_private_file(metadata)?;
    }

    let config = PersistedConfig {
        version: CONFIG_VERSION,
        codex_home: codex_home.to_path_buf(),
    };
    let bytes = serde_json::to_vec(&config).map_err(HomeError::SerializeConfig)?;
    let mut bytes = bytes;
    bytes.push(b'\n');

    let temporary_path = temporary_path(config_directory);
    let mut temporary = match open_private_new(&temporary_path) {
        Ok(file) => file,
        Err(error) => return Err(HomeError::WriteConfig(error)),
    };
    let write_result = (|| {
        temporary.write_all(&bytes)?;
        temporary.sync_all()?;
        Ok::<(), io::Error>(())
    })();
    drop(temporary);
    if let Err(error) = write_result {
        let _ = fs::remove_file(&temporary_path);
        return Err(HomeError::WriteConfig(error));
    }

    let replace_result = replace_file(&temporary_path, config_path);
    if let Err(error) = replace_result {
        let _ = fs::remove_file(&temporary_path);
        return Err(HomeError::ReplaceConfig(error));
    }
    sync_directory(config_directory).map_err(HomeError::ReplaceConfig)?;
    Ok(())
}

fn temporary_path(config_directory: &Path) -> PathBuf {
    let timestamp = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |duration| duration.as_nanos());
    config_directory.join(format!(
        ".{CONFIG_FILE}.tmp-{}-{timestamp}",
        std::process::id()
    ))
}

fn open_private_new(path: &Path) -> io::Result<File> {
    let mut options = OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;

        options.mode(0o600);
    }
    options.open(path)
}

fn replace_file(source: &Path, target: &Path) -> io::Result<()> {
    #[cfg(unix)]
    {
        fs::rename(source, target)
    }
    #[cfg(not(unix))]
    {
        match fs::rename(source, target) {
            Ok(()) => Ok(()),
            Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {
                fs::remove_file(target)?;
                fs::rename(source, target)
            }
            Err(error) => Err(error),
        }
    }
}

#[cfg(unix)]
fn sync_directory(path: &Path) -> io::Result<()> {
    File::open(path)?.sync_all()
}

#[cfg(not(unix))]
fn sync_directory(_path: &Path) -> io::Result<()> {
    Ok(())
}

fn ensure_private_directory(path: &Path) -> Result<(), HomeError> {
    let metadata = match fs::symlink_metadata(path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            fs::create_dir_all(path).map_err(HomeError::CreateConfigDirectory)?;
            fs::symlink_metadata(path).map_err(HomeError::CreateConfigDirectory)?
        }
        Err(error) => return Err(HomeError::CreateConfigDirectory(error)),
    };
    if metadata.file_type().is_symlink() {
        return Err(HomeError::ConfigSymlink);
    }
    if !metadata.is_dir() {
        return Err(HomeError::ConfigNotRegular);
    }
    check_owner_identity(&metadata, true)?;
    make_private_directory(path)?;
    let private_metadata = fs::symlink_metadata(path).map_err(HomeError::CreateConfigDirectory)?;
    if private_metadata.file_type().is_symlink() || !private_metadata.is_dir() {
        return Err(HomeError::ConfigNotRegular);
    }
    check_private_directory(&private_metadata)
}

fn make_private_directory(path: &Path) -> Result<(), HomeError> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;

        fs::set_permissions(path, fs::Permissions::from_mode(0o700))
            .map_err(HomeError::CreateConfigDirectory)?;
    }
    Ok(())
}

fn check_private_directory(metadata: &fs::Metadata) -> Result<(), HomeError> {
    check_owner(metadata, true)
}

fn check_private_file(metadata: &fs::Metadata) -> Result<(), HomeError> {
    check_owner(metadata, false)
}

fn check_owner_identity(metadata: &fs::Metadata, directory: bool) -> Result<(), HomeError> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt;

        if metadata.uid() != current_uid() {
            return Err(if directory {
                HomeError::ConfigDirectoryNotPrivate
            } else {
                HomeError::ConfigFileNotPrivate
            });
        }
    }
    let _ = metadata;
    let _ = directory;
    Ok(())
}

fn check_owner(metadata: &fs::Metadata, directory: bool) -> Result<(), HomeError> {
    check_owner_identity(metadata, directory)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt;

        if metadata.mode() & 0o077 != 0 {
            return Err(if directory {
                HomeError::ConfigDirectoryNotPrivate
            } else {
                HomeError::ConfigFileNotPrivate
            });
        }
        if !directory && metadata.mode() & 0o400 == 0 {
            return Err(HomeError::ConfigFileNotPrivate);
        }
    }
    let _ = metadata;
    let _ = directory;
    Ok(())
}

#[cfg(unix)]
fn current_uid() -> u32 {
    // SAFETY: geteuid has no preconditions and only reads process identity.
    unsafe { libc::geteuid() }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::tempdir;

    fn absolute(path: &Path) -> PathBuf {
        path.to_path_buf()
    }

    #[test]
    fn normalizes_trailing_and_repeated_separators() {
        let path =
            normalize_codex_home(Path::new("/tmp//codex///"), "CODEX_HOME").expect("valid home");
        assert_eq!(path, Path::new("/tmp/codex"));
    }

    #[test]
    fn rejects_relative_root_and_dot_paths() {
        for path in ["relative", "/", "/tmp/./codex", "/tmp/../codex"] {
            assert!(normalize_codex_home(Path::new(path), "CODEX_HOME").is_err());
        }
    }

    #[test]
    fn environment_conflict_is_rejected_after_normalization() {
        let error = resolve_candidates(
            Some(PathBuf::from("/tmp/codex/")),
            Some(PathBuf::from("/tmp/other")),
            None,
            None,
            None,
        )
        .expect_err("conflicting homes");
        assert!(matches!(error, HomeError::ConflictingEnvironment { .. }));
    }

    #[test]
    fn persisted_selection_is_used_and_mismatch_is_reported() {
        let resolved = resolve_candidates(
            Some(PathBuf::from("/tmp/codex")),
            None,
            Some(PathBuf::from("/tmp/installer")),
            None,
            Some(PathBuf::from("/tmp/config.json")),
        )
        .expect("home");
        assert_eq!(resolved.source(), HomeSource::CodexMarathonEnvironment);
        assert_eq!(resolved.codex_home(), Path::new("/tmp/codex"));
        assert_eq!(resolved.persisted_home(), Some(Path::new("/tmp/installer")));
        assert!(resolved.is_persisted_mismatch());
    }

    #[test]
    fn persisted_config_round_trips_private_and_atomic() {
        let root = tempdir().expect("temporary directory");
        let config_path = root.path().join(CONFIG_DIRECTORY).join(CONFIG_FILE);
        let home = absolute(&root.path().join("codex-home"));
        write_persisted_at(&config_path, &home).expect("write config");
        let persisted = read_persisted_at(&config_path)
            .expect("read config")
            .expect("config exists");
        assert_eq!(persisted, home);
        let text = fs::read_to_string(&config_path).expect("config text");
        assert!(text.contains("\"version\":1"));
        assert!(text.contains("\"codex_home\":"));
        #[cfg(unix)]
        {
            use std::os::unix::fs::MetadataExt;
            let directory_mode = fs::metadata(config_path.parent().expect("parent"))
                .expect("directory metadata")
                .mode()
                & 0o777;
            let file_mode = fs::metadata(&config_path).expect("file metadata").mode() & 0o777;
            assert_eq!(directory_mode, 0o700);
            assert_eq!(file_mode, 0o600);
        }
    }
}
