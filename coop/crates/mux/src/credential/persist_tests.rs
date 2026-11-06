// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use serial_test::serial;

use super::*;

/// Names of every env var consulted by the policy resolver. Tests scrub all of
/// these before exercising the resolver to prevent ambient host config from
/// leaking in and flipping the outcome.
const ENV_VARS: &[&str] = &[
    "COOP_CREDENTIAL_KEY",
    "COOP_CREDENTIAL_KEY_FILE",
    "COOP_CREDENTIAL_KEY_PREVIOUS",
    "COOP_ALLOW_PLAINTEXT_BOOTSTRAP",
];

/// Clear every credential-related env var; tests must run with
/// `#[serial]` because env mutation is process-global.
fn clear_credential_env() {
    for k in ENV_VARS {
        std::env::remove_var(k);
    }
}

fn set_env(key: &str, value: &str) {
    std::env::set_var(key, value);
}

fn test_key() -> Vec<u8> {
    b"test-master-key-for-unit-tests!".to_vec()
}

fn other_key() -> Vec<u8> {
    b"other-key-for-rotation-testing!".to_vec()
}

fn sample_creds() -> PersistedCredentials {
    let mut creds = PersistedCredentials::default();
    creds.accounts.insert(
        "test-account".into(),
        PersistedAccount {
            access_token: "sk-secret-token-12345".into(),
            refresh_token: Some("rt-refresh-67890".into()),
            expires_at: 1_700_000_000,
        },
    );
    creds
}

#[test]
fn roundtrip_plaintext() -> anyhow::Result<()> {
    let tmp = tempfile::NamedTempFile::new()?;
    let creds = sample_creds();

    save_inner(tmp.path(), &creds, None)?;
    let loaded = load_inner(tmp.path(), None, None)?;

    assert_eq!(loaded.accounts.len(), 1);
    assert_eq!(loaded.accounts["test-account"].access_token, "sk-secret-token-12345");
    assert_eq!(loaded.accounts["test-account"].refresh_token, Some("rt-refresh-67890".into()));
    assert_eq!(loaded.accounts["test-account"].expires_at, 1_700_000_000);

    // Verify it's actually plaintext on disk.
    let raw = std::fs::read_to_string(tmp.path())?;
    assert!(raw.contains("sk-secret-token-12345"));

    Ok(())
}

#[test]
fn roundtrip_encrypted() -> anyhow::Result<()> {
    let tmp = tempfile::NamedTempFile::new()?;
    let creds = sample_creds();
    let key = test_key();

    save_inner(tmp.path(), &creds, Some(&key))?;

    // File must contain encrypted envelope, not plaintext secrets.
    let raw = std::fs::read_to_string(tmp.path())?;
    assert!(raw.contains("ciphertext"), "expected encrypted envelope");
    assert!(raw.contains("nonce"), "expected nonce in envelope");
    assert!(
        !raw.contains("sk-secret-token-12345"),
        "plaintext token must not appear in encrypted file"
    );

    // Load with same key.
    let loaded = load_inner(tmp.path(), Some(&key), None)?;
    assert_eq!(loaded.accounts["test-account"].access_token, "sk-secret-token-12345");
    assert_eq!(loaded.accounts["test-account"].refresh_token, Some("rt-refresh-67890".into()));

    Ok(())
}

#[test]
fn migration_plaintext_to_encrypted() -> anyhow::Result<()> {
    let tmp = tempfile::NamedTempFile::new()?;
    let creds = sample_creds();
    let key = test_key();

    // Save as plaintext (no key).
    save_inner(tmp.path(), &creds, None)?;
    let raw_before = std::fs::read_to_string(tmp.path())?;
    assert!(raw_before.contains("sk-secret-token-12345"), "should be plaintext initially");

    // Load with a key — triggers auto-migration.
    let loaded = load_inner(tmp.path(), Some(&key), None)?;
    assert_eq!(loaded.accounts["test-account"].access_token, "sk-secret-token-12345");

    // File should now be encrypted.
    let raw_after = std::fs::read_to_string(tmp.path())?;
    assert!(raw_after.contains("ciphertext"), "file should be encrypted after migration");
    assert!(
        !raw_after.contains("sk-secret-token-12345"),
        "plaintext token must not appear after migration"
    );

    Ok(())
}

#[test]
fn key_rotation() -> anyhow::Result<()> {
    let tmp = tempfile::NamedTempFile::new()?;
    let creds = sample_creds();
    let old_key = test_key();
    let new_key = other_key();

    // Save with old key.
    save_inner(tmp.path(), &creds, Some(&old_key))?;

    // Load with new key + old key as previous — should succeed and re-encrypt.
    let loaded = load_inner(tmp.path(), Some(&new_key), Some(&old_key))?;
    assert_eq!(loaded.accounts["test-account"].access_token, "sk-secret-token-12345");

    // Now loading with only the new key (no previous) should work
    // because the file was re-encrypted.
    let loaded2 = load_inner(tmp.path(), Some(&new_key), None)?;
    assert_eq!(loaded2.accounts["test-account"].access_token, "sk-secret-token-12345");

    Ok(())
}

#[test]
fn wrong_key_fails() -> anyhow::Result<()> {
    let tmp = tempfile::NamedTempFile::new()?;
    let creds = sample_creds();
    let key = test_key();
    let wrong_key = other_key();

    save_inner(tmp.path(), &creds, Some(&key))?;

    // Loading with wrong key and no previous should fail.
    let result = load_inner(tmp.path(), Some(&wrong_key), None);
    assert!(result.is_err());

    Ok(())
}

#[test]
fn encrypted_no_key_fails() -> anyhow::Result<()> {
    let tmp = tempfile::NamedTempFile::new()?;
    let creds = sample_creds();
    let key = test_key();

    save_inner(tmp.path(), &creds, Some(&key))?;

    // Loading encrypted file without any key should fail.
    let result = load_inner(tmp.path(), None, None);
    assert!(result.is_err());

    Ok(())
}

#[test]
fn dynamic_accounts_preserved() -> anyhow::Result<()> {
    let tmp = tempfile::NamedTempFile::new()?;
    let key = test_key();

    let mut creds = sample_creds();
    creds.dynamic_accounts.push(AccountConfig {
        name: "dynamic-1".into(),
        provider: "claude".into(),
        env_key: None,
        model: Some("claude-opus-4-6".into()),
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: true,
    });

    save_inner(tmp.path(), &creds, Some(&key))?;
    let loaded = load_inner(tmp.path(), Some(&key), None)?;

    assert_eq!(loaded.dynamic_accounts.len(), 1);
    assert_eq!(loaded.dynamic_accounts[0].name, "dynamic-1");
    assert_eq!(loaded.dynamic_accounts[0].provider, "claude");

    Ok(())
}

#[test]
fn empty_credentials_roundtrip() -> anyhow::Result<()> {
    let tmp = tempfile::NamedTempFile::new()?;
    let creds = PersistedCredentials::default();
    let key = test_key();

    save_inner(tmp.path(), &creds, Some(&key))?;
    let loaded = load_inner(tmp.path(), Some(&key), None)?;
    assert!(loaded.accounts.is_empty());
    assert!(loaded.dynamic_accounts.is_empty());

    Ok(())
}

// ---------------------------------------------------------------------------
// Fail-closed policy tests (W1 secrets hardening — ka-0000000955)
//
// `resolve_key_policy` and the public `load`/`save` consult process-global env
// vars, so these tests must be `#[serial]` to avoid clobbering each other or
// other suites running in parallel.
// ---------------------------------------------------------------------------

#[test]
#[serial]
fn policy_errors_when_no_key_and_no_bootstrap() {
    clear_credential_env();
    let result = resolve_key_policy();
    let msg = match result {
        Ok(_) => String::new(),
        Err(e) => e.to_string(),
    };
    assert!(!msg.is_empty(), "policy must fail-closed when no key configured");
    assert!(msg.contains("COOP_CREDENTIAL_KEY"), "error must mention COOP_CREDENTIAL_KEY: {msg}");
    assert!(
        msg.contains("COOP_ALLOW_PLAINTEXT_BOOTSTRAP"),
        "error must mention the escape hatch: {msg}",
    );
}

#[test]
#[serial]
fn policy_accepts_bootstrap_escape_hatch() -> anyhow::Result<()> {
    clear_credential_env();
    set_env("COOP_ALLOW_PLAINTEXT_BOOTSTRAP", "1");
    let policy = resolve_key_policy()?;
    assert!(matches!(policy, KeyPolicy::BootstrapPlaintext));
    clear_credential_env();
    Ok(())
}

#[test]
#[serial]
fn policy_rejects_non_one_bootstrap_value() {
    clear_credential_env();
    // Only the exact string "1" enables the escape hatch — "true", "yes", etc.
    // must NOT silently unlock plaintext mode.
    set_env("COOP_ALLOW_PLAINTEXT_BOOTSTRAP", "true");
    let result = resolve_key_policy();
    clear_credential_env();
    let msg = match result {
        Ok(_) => String::new(),
        Err(e) => e.to_string(),
    };
    assert!(!msg.is_empty(), "non-1 bootstrap value must not enable plaintext mode");
    assert!(msg.contains("COOP_ALLOW_PLAINTEXT_BOOTSTRAP"));
}

#[test]
#[serial]
fn policy_returns_key_from_env() -> anyhow::Result<()> {
    clear_credential_env();
    set_env("COOP_CREDENTIAL_KEY", "raw-master-key-material-32-bytes!!");
    let policy = resolve_key_policy()?;
    assert!(matches!(policy, KeyPolicy::Key(_)));
    clear_credential_env();
    Ok(())
}

#[test]
#[serial]
fn policy_returns_key_from_key_file() -> anyhow::Result<()> {
    clear_credential_env();
    let tmp = tempfile::NamedTempFile::new()?;
    std::fs::write(tmp.path(), b"file-master-key-material-32-bytes")?;
    let Some(path_str) = tmp.path().to_str() else {
        anyhow::bail!("tempfile path is not utf-8");
    };
    set_env("COOP_CREDENTIAL_KEY_FILE", path_str);
    let policy = resolve_key_policy()?;
    assert!(matches!(policy, KeyPolicy::Key(_)));
    clear_credential_env();
    Ok(())
}

#[test]
#[serial]
fn public_save_errors_when_no_key_and_no_bootstrap() -> anyhow::Result<()> {
    clear_credential_env();
    let tmp = tempfile::NamedTempFile::new()?;
    let creds = sample_creds();
    let result = save(tmp.path(), &creds);
    let Err(err) = result else {
        anyhow::bail!("public save must fail-closed when no key configured");
    };
    assert!(err.to_string().contains("COOP_CREDENTIAL_KEY"));
    Ok(())
}

#[test]
#[serial]
fn public_load_errors_when_no_key_and_no_bootstrap() -> anyhow::Result<()> {
    // Pre-seed a plaintext file using the lower-level helper so we know the
    // failure comes from the policy gate and not a missing file.
    let tmp = tempfile::NamedTempFile::new()?;
    save_inner(tmp.path(), &sample_creds(), None)?;

    clear_credential_env();
    let result = load(tmp.path());
    let Err(err) = result else {
        anyhow::bail!("public load must fail-closed when no key configured");
    };
    assert!(err.to_string().contains("COOP_CREDENTIAL_KEY"));
    Ok(())
}

#[test]
#[serial]
fn public_save_and_load_succeed_in_bootstrap_mode() -> anyhow::Result<()> {
    clear_credential_env();
    set_env("COOP_ALLOW_PLAINTEXT_BOOTSTRAP", "1");

    let tmp = tempfile::NamedTempFile::new()?;
    let creds = sample_creds();

    save(tmp.path(), &creds)?;

    // Bootstrap mode writes plaintext — verify the file is not an encrypted
    // envelope so we can be sure the WARN path is the one being exercised.
    let raw = std::fs::read_to_string(tmp.path())?;
    assert!(
        raw.contains("sk-secret-token-12345"),
        "bootstrap mode should write plaintext credentials",
    );
    assert!(!raw.contains("ciphertext"), "bootstrap mode must not encrypt");

    let loaded = load(tmp.path())?;
    assert_eq!(loaded.accounts["test-account"].access_token, "sk-secret-token-12345");

    clear_credential_env();
    Ok(())
}

#[test]
#[serial]
fn public_save_and_load_succeed_with_key() -> anyhow::Result<()> {
    clear_credential_env();
    set_env("COOP_CREDENTIAL_KEY", "raw-master-key-material-32-bytes!!");

    let tmp = tempfile::NamedTempFile::new()?;
    let creds = sample_creds();

    save(tmp.path(), &creds)?;

    let raw = std::fs::read_to_string(tmp.path())?;
    assert!(raw.contains("ciphertext"), "configured-key path must encrypt at rest");
    assert!(!raw.contains("sk-secret-token-12345"));

    let loaded = load(tmp.path())?;
    assert_eq!(loaded.accounts["test-account"].access_token, "sk-secret-token-12345");

    clear_credential_env();
    Ok(())
}
