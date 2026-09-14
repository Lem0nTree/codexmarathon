use super::*;
use codexmarathon_runtime::{AccountStore, CredentialHealth, SnapshotVault};

const SECRET: &str = "test passphrase with several words";

fn snapshot(id: &str) -> AuthSnapshot {
    let bytes = format!(
        "{{ \"account_id\":\"{id}\", \"tokens\":{{\"access_token\":\"synthetic-{id}\"}} }}\n"
    );
    AuthSnapshot::for_account(id, bytes).expect("synthetic snapshot")
}

fn manifest(ids: &[(&str, &str)]) -> Manifest {
    Manifest {
        format: "codexmarathon-accounts".into(),
        version: 1,
        created_at: "2026-09-14T00:00:00Z".into(),
        exporter_version: "test".into(),
        accounts: ids
            .iter()
            .map(|(id, alias)| BackupAccount {
                id: (*id).into(),
                alias: (*alias).into(),
                snapshot: STANDARD.encode(snapshot(id).bytes()),
            })
            .collect(),
    }
}

// Reduced work factor is restricted to synthetic test fixtures. Production
// export uses the fixed public memory/work budget.
fn encrypted_fixture(path: &Path, manifest: &Manifest) {
    let mut recipient = age::scrypt::Recipient::new(SecretString::from(SECRET.to_owned()));
    recipient.set_work_factor(10);
    let encryptor =
        age::Encryptor::with_recipients(std::iter::once(&recipient as &dyn age::Recipient))
            .expect("encryptor");
    let mut bytes = Vec::new();
    let mut writer = encryptor.wrap_output(&mut bytes).expect("writer");
    writer
        .write_all(&serde_json::to_vec(manifest).expect("JSON"))
        .expect("write");
    writer.finish().expect("finish");
    fs::write(path, bytes).expect("fixture");
}

fn register(config: &MarathonConfig, id: &str, alias: &str) {
    FileSnapshotVault::new(config.vault_dir())
        .save(id, &snapshot(id))
        .expect("vault");
    FileAccountRegistry::new(config.registry_path())
        .register(AccountRecord::new(id, alias).expect("record"))
        .expect("register");
}

#[test]
fn export_round_trip_preserves_exact_bytes_and_never_switches_destination() {
    let temp = tempfile::tempdir().expect("tempdir");
    let source = MarathonConfig::new(temp.path().join("source")).expect("config");
    let destination = MarathonConfig::new(temp.path().join("destination")).expect("config");
    register(&source, "a", "Work");
    register(&source, "b", "Personal");
    register(&destination, "c", "Existing");
    let registry = FileAccountRegistry::new(destination.registry_path());
    registry.set_active("c").expect("activate");
    registry.set_enabled(false).expect("disable");
    let auth_path = destination.codex_home().join("auth.json");
    fs::write(&auth_path, b"untouched active auth fixture").expect("auth fixture");
    let output = temp.path().join("accounts.cmbackup");
    let report = export_accounts(
        &source,
        &["a".into(), "b".into()],
        &output,
        SecretString::from(SECRET.to_owned()),
        false,
    )
    .expect("export");
    assert_eq!(report.accounts.len(), 2);
    let ciphertext = fs::read(&output).expect("ciphertext");
    assert!(
        !ciphertext
            .windows(b"synthetic-a".len())
            .any(|part| part == b"synthetic-a")
    );
    let report = import_accounts(
        &destination,
        &output,
        SecretString::from(SECRET.to_owned()),
        ConflictPolicy::Skip,
        false,
    )
    .expect("import");
    assert_eq!(report.imported.len(), 2);
    let state = registry.state().expect("state");
    assert_eq!(state.active_account_id, "c");
    assert!(!state.enabled);
    assert_eq!(
        fs::read(&auth_path).expect("auth"),
        b"untouched active auth fixture"
    );
    let account = state.accounts.get("a").expect("imported");
    assert_eq!(account.credential_health, CredentialHealth::Unknown);
    assert_eq!(
        FileSnapshotVault::new(destination.vault_dir())
            .load(&account.credential_ref)
            .expect("snapshot")
            .bytes(),
        snapshot("a").bytes()
    );
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        assert_eq!(
            fs::metadata(&output).expect("mode").permissions().mode() & 0o777,
            0o600
        );
    }
}

#[test]
fn wrong_password_tampering_truncation_and_dry_run_make_no_destination_files() {
    let temp = tempfile::tempdir().expect("tempdir");
    let config = MarathonConfig::new(temp.path().join("destination")).expect("config");
    let input = temp.path().join("backup.cmbackup");
    encrypted_fixture(&input, &manifest(&[("a", "A")]));
    assert!(matches!(
        import_accounts(
            &config,
            &input,
            SecretString::from("incorrect password".to_owned()),
            ConflictPolicy::Skip,
            false
        ),
        Err(TransferError::Decryption)
    ));
    assert!(!config.codex_home().exists());
    let preview = import_accounts(
        &config,
        &input,
        SecretString::from(SECRET.to_owned()),
        ConflictPolicy::Skip,
        true,
    )
    .expect("preview");
    assert!(preview.dry_run);
    assert_eq!(preview.imported.len(), 1);
    assert!(!config.codex_home().exists());
    let original = fs::read(&input).expect("ciphertext");
    let mut tampered = original.clone();
    *tampered.last_mut().expect("last byte") ^= 1;
    fs::write(&input, tampered).expect("tamper");
    assert!(
        import_accounts(
            &config,
            &input,
            SecretString::from(SECRET.to_owned()),
            ConflictPolicy::Skip,
            false
        )
        .is_err()
    );
    fs::write(&input, &original[..original.len() - 1]).expect("truncate");
    assert!(
        import_accounts(
            &config,
            &input,
            SecretString::from(SECRET.to_owned()),
            ConflictPolicy::Skip,
            false
        )
        .is_err()
    );
    assert!(!config.codex_home().exists());
}

#[test]
fn excessive_scrypt_header_is_rejected_without_destination_writes() {
    let temp = tempfile::tempdir().expect("tempdir");
    let config = MarathonConfig::new(temp.path().join("destination")).expect("config");
    let input = temp.path().join("backup.cmbackup");
    encrypted_fixture(&input, &manifest(&[("a", "A")]));
    let mut bytes = fs::read(&input).expect("ciphertext");
    let position = bytes
        .windows(4)
        .position(|part| part == b" 10\n")
        .expect("scrypt factor");
    bytes[position + 2] = b'9';
    fs::write(&input, bytes).expect("hostile header");
    assert!(
        import_accounts(
            &config,
            &input,
            SecretString::from(SECRET.to_owned()),
            ConflictPolicy::Skip,
            false
        )
        .is_err()
    );
    assert!(!config.codex_home().exists());
}

#[test]
fn conflicts_are_planned_for_the_entire_batch_before_writing() {
    let mut state = RegistryState::default();
    state
        .accounts
        .insert("a".into(), AccountRecord::new("a", "Work").expect("record"));
    state.active_account_id = "a".into();
    let entries = validate_manifest(manifest(&[("b", "Work")])).expect("entries");
    let (report, changes) =
        plan_import(&state, entries, ConflictPolicy::Rename, false).expect("rename");
    assert_eq!(report.imported[0].alias, "Work (2)");
    assert_eq!(changes[0].0.id, "b");
    let entries = validate_manifest(manifest(&[("a", "Replacement")])).expect("entries");
    assert!(matches!(
        plan_import(&state, entries, ConflictPolicy::Replace, false),
        Err(TransferError::ActiveAccount)
    ));
    let entries = validate_manifest(manifest(&[("a", "Replacement")])).expect("entries");
    let (report, changes) =
        plan_import(&state, entries, ConflictPolicy::Rename, false).expect("same ID");
    assert_eq!(report.skipped.len(), 1);
    assert!(changes.is_empty());
}

#[test]
fn manifest_rejects_identity_mismatch_duplicates_unknown_version_and_oversize() {
    let mut value = manifest(&[("a", "A")]);
    value.accounts[0].snapshot = STANDARD.encode(snapshot("b").bytes());
    assert!(validate_manifest(value).is_err());
    assert!(validate_manifest(manifest(&[("a", "A"), ("a", "B")])).is_err());
    assert!(validate_manifest(manifest(&[("a", "A"), ("b", "A")])).is_err());
    let mut value = manifest(&[("a", "A")]);
    value.version = 2;
    assert!(validate_manifest(value).is_err());
    let mut value = manifest(&[("a", "A")]);
    value.accounts[0].snapshot = "A".repeat(MAX_SNAPSHOT_BYTES * 2);
    assert!(matches!(
        validate_manifest(value),
        Err(TransferError::Limit)
    ));
    let mut value = manifest(&[("a", "A")]);
    value.accounts[0].snapshot = STANDARD.encode(b"{}");
    assert!(validate_manifest(value).is_err());
}

#[cfg(unix)]
#[test]
fn passphrase_file_rejects_public_modes_symlinks_and_multiple_lines() {
    use std::os::unix::fs::{PermissionsExt, symlink};
    let temp = tempfile::tempdir().expect("tempdir");
    let path = temp.path().join("password");
    fs::write(&path, format!("{SECRET}\r\n")).expect("fixture");
    fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).expect("mode");
    assert_eq!(
        read_passphrase_file(&path)
            .expect("passphrase")
            .expose_secret(),
        SECRET
    );
    let link = temp.path().join("link");
    symlink(&path, &link).expect("symlink");
    assert!(matches!(
        read_passphrase_file(&link),
        Err(TransferError::UnsafePath)
    ));
    fs::set_permissions(&path, fs::Permissions::from_mode(0o644)).expect("mode");
    assert!(matches!(
        read_passphrase_file(&path),
        Err(TransferError::UnsafePath)
    ));
    fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).expect("mode");
    fs::write(&path, format!("{SECRET}\n\n")).expect("fixture");
    assert!(read_passphrase_file(&path).is_err());
}

#[test]
fn output_cannot_overwrite_native_state_or_clobber_without_permission() {
    let temp = tempfile::tempdir().expect("tempdir");
    let config = MarathonConfig::new(temp.path()).expect("config");
    let auth = temp.path().join("auth.json");
    fs::write(&auth, b"fixture").expect("auth");
    assert!(matches!(
        checked_output(&config, &auth, true),
        Err(TransferError::UnsafePath)
    ));
    let file = temp.path().join("existing");
    fs::write(&file, b"fixture").expect("output");
    assert!(matches!(
        checked_output(&config, &file, false),
        Err(TransferError::OutputExists)
    ));
}
