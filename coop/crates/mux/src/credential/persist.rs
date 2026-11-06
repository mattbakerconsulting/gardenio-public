// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Credential persistence: load/save to JSON file with AES-256-GCM encryption at rest.
//!
//! Credentials are encrypted using AES-256-GCM with a key derived via HKDF-SHA256
//! from a master secret. The master secret is read from:
//! - `COOP_CREDENTIAL_KEY` env var (base64-encoded or raw bytes)
//! - `COOP_CREDENTIAL_KEY_FILE` env var (path to file containing key material)
//!
//! Encryption is **mandatory**. If neither variable is set, credential operations
//! fail-closed with a configuration error. The escape hatch
//! `COOP_ALLOW_PLAINTEXT_BOOTSTRAP=1` permits plaintext operation for
//! bootstrap/migration scenarios; every credential read/write in that mode emits
//! a WARN log so the unsafe configuration cannot go unnoticed.
//!
//! Key rotation is supported via `COOP_CREDENTIAL_KEY_PREVIOUS` — if the current key
//! fails to decrypt, the previous key is tried and the file is re-encrypted with the
//! new key on next save.

use std::collections::HashMap;
use std::path::Path;

use base64::Engine;
use ring::aead::{Aad, LessSafeKey, Nonce, UnboundKey, AES_256_GCM, NONCE_LEN};
use ring::hkdf;
use ring::rand::{SecureRandom, SystemRandom};
use serde::{Deserialize, Serialize};
use zeroize::{Zeroize, Zeroizing};

use crate::credential::AccountConfig;

const HKDF_SALT: &[u8] = b"coop-credential-persist-v1";
const HKDF_INFO: &[u8] = b"coop-credential-aes-256-gcm";
const ENCRYPTED_VERSION: u32 = 1;

/// Persisted credential state for all accounts.
#[derive(Debug, Default, Clone, Serialize, Deserialize)]
pub struct PersistedCredentials {
    pub accounts: HashMap<String, PersistedAccount>,
    /// Configs for accounts added dynamically at runtime (not in static config file).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub dynamic_accounts: Vec<AccountConfig>,
}

impl Drop for PersistedCredentials {
    fn drop(&mut self) {
        // Each PersistedAccount's Drop zeroizes its sensitive fields.
        self.accounts.clear();
        self.dynamic_accounts.clear();
    }
}

/// Persisted state for a single account.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PersistedAccount {
    pub access_token: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub refresh_token: Option<String>,
    /// Expiry as epoch seconds.
    #[serde(default)]
    pub expires_at: u64,
}

impl Drop for PersistedAccount {
    fn drop(&mut self) {
        self.access_token.zeroize();
        if let Some(ref mut t) = self.refresh_token {
            t.zeroize();
        }
    }
}

/// Encrypted file envelope written to disk.
#[derive(Serialize, Deserialize)]
struct EncryptedEnvelope {
    version: u32,
    nonce: String,
    ciphertext: String,
}

/// HKDF key type for AES-256 key derivation.
struct Aes256KeyType;

impl hkdf::KeyType for Aes256KeyType {
    fn len(&self) -> usize {
        32
    }
}

// ---------------------------------------------------------------------------
// Key resolution
// ---------------------------------------------------------------------------

/// Resolve master key from `COOP_CREDENTIAL_KEY` or `COOP_CREDENTIAL_KEY_FILE`.
fn resolve_master_key() -> Option<Zeroizing<Vec<u8>>> {
    if let Ok(val) = std::env::var("COOP_CREDENTIAL_KEY") {
        let b64 = &base64::engine::general_purpose::STANDARD;
        return Some(Zeroizing::new(
            b64.decode(val.as_bytes()).unwrap_or_else(|_| val.into_bytes()),
        ));
    }
    if let Ok(path) = std::env::var("COOP_CREDENTIAL_KEY_FILE") {
        match std::fs::read(&path) {
            Ok(mut contents) => {
                // Trim trailing newline commonly left by `echo` or editors.
                while contents.last() == Some(&b'\n') || contents.last() == Some(&b'\r') {
                    contents.pop();
                }
                return Some(Zeroizing::new(contents));
            }
            Err(e) => {
                tracing::warn!(path = %path, err = %e, "failed to read COOP_CREDENTIAL_KEY_FILE");
            }
        }
    }
    None
}

/// Resolve previous master key for key rotation (`COOP_CREDENTIAL_KEY_PREVIOUS`).
fn resolve_previous_key() -> Option<Zeroizing<Vec<u8>>> {
    let val = std::env::var("COOP_CREDENTIAL_KEY_PREVIOUS").ok()?;
    let b64 = &base64::engine::general_purpose::STANDARD;
    Some(Zeroizing::new(b64.decode(val.as_bytes()).unwrap_or_else(|_| val.into_bytes())))
}

/// Result of resolving the credential-encryption policy from the environment.
#[cfg_attr(test, derive(Debug))]
enum KeyPolicy {
    /// Master key configured — encrypt at rest.
    Key(Zeroizing<Vec<u8>>),
    /// No key, but `COOP_ALLOW_PLAINTEXT_BOOTSTRAP=1` — operate on plaintext and
    /// WARN on every credential read/write.
    BootstrapPlaintext,
}

/// Resolve the credential-encryption policy from the environment.
///
/// Returns an error (fail-closed) when no key is configured and the bootstrap
/// escape hatch is not explicitly set, so the daemon refuses to start rather
/// than silently writing plaintext credentials.
fn resolve_key_policy() -> anyhow::Result<KeyPolicy> {
    if let Some(key) = resolve_master_key() {
        return Ok(KeyPolicy::Key(key));
    }
    if std::env::var("COOP_ALLOW_PLAINTEXT_BOOTSTRAP").as_deref() == Ok("1") {
        return Ok(KeyPolicy::BootstrapPlaintext);
    }
    anyhow::bail!(
        "credential encryption key not configured — refusing to operate on credentials. \
         Set COOP_CREDENTIAL_KEY or COOP_CREDENTIAL_KEY_FILE. \
         As a last-resort bootstrap escape hatch (NOT for production), \
         set COOP_ALLOW_PLAINTEXT_BOOTSTRAP=1."
    )
}

// ---------------------------------------------------------------------------
// Crypto helpers
// ---------------------------------------------------------------------------

/// Derive AES-256-GCM key from master key material via HKDF-SHA256.
fn derive_key(master: &[u8]) -> anyhow::Result<LessSafeKey> {
    let salt = hkdf::Salt::new(hkdf::HKDF_SHA256, HKDF_SALT);
    let prk = salt.extract(master);
    let okm = prk
        .expand(&[HKDF_INFO], Aes256KeyType)
        .map_err(|_| anyhow::anyhow!("HKDF expand failed"))?;
    let mut key_bytes = Zeroizing::new([0u8; 32]);
    okm.fill(&mut *key_bytes).map_err(|_| anyhow::anyhow!("HKDF fill failed"))?;
    let unbound = UnboundKey::new(&AES_256_GCM, &*key_bytes)
        .map_err(|_| anyhow::anyhow!("invalid AES-256-GCM key material"))?;
    Ok(LessSafeKey::new(unbound))
}

/// Encrypt plaintext with AES-256-GCM, returning (ciphertext‖tag, nonce).
fn encrypt_aead(key: &LessSafeKey, plaintext: &[u8]) -> anyhow::Result<(Vec<u8>, [u8; NONCE_LEN])> {
    let rng = SystemRandom::new();
    let mut nonce_bytes = [0u8; NONCE_LEN];
    rng.fill(&mut nonce_bytes).map_err(|_| anyhow::anyhow!("failed to generate random nonce"))?;
    let nonce = Nonce::assume_unique_for_key(nonce_bytes);

    let mut in_out = plaintext.to_vec();
    key.seal_in_place_append_tag(nonce, Aad::empty(), &mut in_out)
        .map_err(|_| anyhow::anyhow!("AES-256-GCM encryption failed"))?;

    Ok((in_out, nonce_bytes))
}

/// Decrypt ciphertext with AES-256-GCM, returning plaintext wrapped in Zeroizing.
fn decrypt_aead(
    key: &LessSafeKey,
    nonce_bytes: &[u8; NONCE_LEN],
    ciphertext: &[u8],
) -> anyhow::Result<Zeroizing<Vec<u8>>> {
    let nonce = Nonce::assume_unique_for_key(*nonce_bytes);
    let mut buf = ciphertext.to_vec();
    let plaintext = key.open_in_place(nonce, Aad::empty(), &mut buf).map_err(|_| {
        anyhow::anyhow!("AES-256-GCM decryption failed — wrong key or corrupted data")
    })?;
    let result = Zeroizing::new(plaintext.to_vec());
    buf.zeroize();
    Ok(result)
}

// ---------------------------------------------------------------------------
// Internal load/save (testable, key material passed explicitly)
// ---------------------------------------------------------------------------

fn load_inner(
    path: &Path,
    current_key: Option<&[u8]>,
    previous_key: Option<&[u8]>,
) -> anyhow::Result<PersistedCredentials> {
    let contents = std::fs::read_to_string(path)?;

    // Try encrypted envelope first.
    if let Ok(envelope) = serde_json::from_str::<EncryptedEnvelope>(&contents) {
        if envelope.version == ENCRYPTED_VERSION {
            let (creds, needs_reencrypt) = decrypt_envelope(&envelope, current_key, previous_key)?;
            if needs_reencrypt {
                if let Err(e) = save_inner(path, &creds, current_key) {
                    tracing::warn!(err = %e, "failed to re-encrypt credentials after key rotation");
                }
            }
            return Ok(creds);
        }
    }

    // Fall back to plaintext.
    let creds: PersistedCredentials = serde_json::from_str(&contents)?;

    // Auto-encrypt if key is available (migration from plaintext).
    if current_key.is_some() {
        tracing::info!("migrating plaintext credentials to encrypted format");
        if let Err(e) = save_inner(path, &creds, current_key) {
            tracing::warn!(err = %e, "failed to encrypt credentials during migration");
        }
    }

    Ok(creds)
}

fn save_inner(
    path: &Path,
    creds: &PersistedCredentials,
    current_key: Option<&[u8]>,
) -> anyhow::Result<()> {
    use std::sync::atomic::{AtomicU32, Ordering};
    static COUNTER: AtomicU32 = AtomicU32::new(0);

    let json = Zeroizing::new(serde_json::to_string_pretty(creds)?);

    let output = if let Some(master) = current_key {
        let key = derive_key(master)?;
        let (ciphertext, nonce_bytes) = encrypt_aead(&key, json.as_bytes())?;
        let b64 = &base64::engine::general_purpose::STANDARD;
        let envelope = EncryptedEnvelope {
            version: ENCRYPTED_VERSION,
            nonce: b64.encode(nonce_bytes),
            ciphertext: b64.encode(&ciphertext),
        };
        serde_json::to_string_pretty(&envelope)?
    } else {
        static WARN_PLAINTEXT: std::sync::Once = std::sync::Once::new();
        WARN_PLAINTEXT.call_once(|| {
            tracing::warn!(
                "no credential encryption key configured — storing plaintext; \
                 set COOP_CREDENTIAL_KEY or COOP_CREDENTIAL_KEY_FILE to enable encryption"
            );
        });
        json.as_str().to_owned()
    };

    let seq = COUNTER.fetch_add(1, Ordering::Relaxed);
    let tmp_name = format!(
        "{}.{}.{}.tmp",
        path.file_name().unwrap_or_default().to_string_lossy(),
        std::process::id(),
        seq,
    );
    let tmp_path = path.with_file_name(tmp_name);
    std::fs::write(&tmp_path, &output)?;

    // Restrict permissions to owner-only (0600) before rename so the
    // credential file is never world-readable, even momentarily.
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&tmp_path, std::fs::Permissions::from_mode(0o600))?;
    }

    std::fs::rename(&tmp_path, path)?;
    Ok(())
}

/// Decrypt an encrypted envelope, trying the current key first, then the previous key.
/// Returns the credentials and whether re-encryption with the current key is needed.
fn decrypt_envelope(
    envelope: &EncryptedEnvelope,
    current_key: Option<&[u8]>,
    previous_key: Option<&[u8]>,
) -> anyhow::Result<(PersistedCredentials, bool)> {
    let b64 = &base64::engine::general_purpose::STANDARD;
    let nonce_vec = b64.decode(&envelope.nonce)?;
    let nonce_bytes: [u8; NONCE_LEN] = nonce_vec
        .try_into()
        .map_err(|_| anyhow::anyhow!("invalid nonce length in encrypted credentials"))?;
    let ciphertext = b64.decode(&envelope.ciphertext)?;

    // Try current key.
    if let Some(master) = current_key {
        let key = derive_key(master)?;
        if let Ok(plaintext) = decrypt_aead(&key, &nonce_bytes, &ciphertext) {
            let json = Zeroizing::new(
                String::from_utf8(plaintext.to_vec())
                    .map_err(|_| anyhow::anyhow!("decrypted data is not valid UTF-8"))?,
            );
            let creds: PersistedCredentials = serde_json::from_str(&json)?;
            return Ok((creds, false));
        }
        tracing::debug!("current key failed to decrypt, trying previous key");
    }

    // Try previous key for rotation.
    if let Some(prev_master) = previous_key {
        let prev_key = derive_key(prev_master)?;
        let plaintext = decrypt_aead(&prev_key, &nonce_bytes, &ciphertext)?;
        let json = Zeroizing::new(
            String::from_utf8(plaintext.to_vec())
                .map_err(|_| anyhow::anyhow!("decrypted data is not valid UTF-8"))?,
        );
        let creds: PersistedCredentials = serde_json::from_str(&json)?;
        tracing::info!(
            "decrypted with previous key — credentials will be re-encrypted with current key"
        );
        return Ok((creds, true));
    }

    anyhow::bail!(
        "cannot decrypt credentials — neither current nor previous key succeeded; \
         check COOP_CREDENTIAL_KEY and COOP_CREDENTIAL_KEY_PREVIOUS"
    )
}

// ---------------------------------------------------------------------------
// Public API (resolves keys from environment)
// ---------------------------------------------------------------------------

/// Validate the credential-encryption configuration without touching any
/// credential file. Call this early in startup so the daemon refuses to come up
/// when `COOP_CREDENTIAL_KEY` / `COOP_CREDENTIAL_KEY_FILE` are unconfigured and
/// `COOP_ALLOW_PLAINTEXT_BOOTSTRAP=1` is not explicitly set — even when no
/// `credentials.json` exists yet.
pub fn validate_key_policy() -> anyhow::Result<()> {
    resolve_key_policy().map(|_| ())
}

/// Load persisted credentials from a JSON file.
///
/// Requires `COOP_CREDENTIAL_KEY` or `COOP_CREDENTIAL_KEY_FILE` to be configured.
/// Returns an error if neither is set unless `COOP_ALLOW_PLAINTEXT_BOOTSTRAP=1` is
/// explicitly set as a bootstrap escape hatch (in which case a WARN is emitted on
/// every read). Supports both encrypted and plaintext file formats; plaintext files
/// are auto-encrypted on load when a key is available. Key rotation is handled
/// transparently via `COOP_CREDENTIAL_KEY_PREVIOUS`.
pub fn load(path: &Path) -> anyhow::Result<PersistedCredentials> {
    let policy = resolve_key_policy()?;
    let previous = resolve_previous_key();
    match policy {
        KeyPolicy::Key(key) => {
            load_inner(path, Some(key.as_slice()), previous.as_ref().map(|k| k.as_slice()))
        }
        KeyPolicy::BootstrapPlaintext => {
            tracing::warn!(
                path = %path.display(),
                "COOP_ALLOW_PLAINTEXT_BOOTSTRAP=1 — reading credentials WITHOUT encryption. \
                 Configure COOP_CREDENTIAL_KEY or COOP_CREDENTIAL_KEY_FILE for production."
            );
            load_inner(path, None, previous.as_ref().map(|k| k.as_slice()))
        }
    }
}

/// Save persisted credentials to a JSON file atomically (write tmp + rename).
///
/// Requires `COOP_CREDENTIAL_KEY` or `COOP_CREDENTIAL_KEY_FILE` to be configured.
/// Returns an error if neither is set unless `COOP_ALLOW_PLAINTEXT_BOOTSTRAP=1` is
/// explicitly set as a bootstrap escape hatch (in which case a WARN is emitted on
/// every write). Uses a unique temp filename (PID + counter) to avoid corruption
/// from concurrent writes.
pub fn save(path: &Path, creds: &PersistedCredentials) -> anyhow::Result<()> {
    let policy = resolve_key_policy()?;
    match policy {
        KeyPolicy::Key(key) => save_inner(path, creds, Some(key.as_slice())),
        KeyPolicy::BootstrapPlaintext => {
            tracing::warn!(
                path = %path.display(),
                "COOP_ALLOW_PLAINTEXT_BOOTSTRAP=1 — writing credentials WITHOUT encryption. \
                 Configure COOP_CREDENTIAL_KEY or COOP_CREDENTIAL_KEY_FILE for production."
            );
            save_inner(path, creds, None)
        }
    }
}

#[cfg(test)]
#[path = "persist_tests.rs"]
mod tests;
