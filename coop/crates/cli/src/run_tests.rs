// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::path::Path;

use serde_json::json;

use crate::driver::claude::setup as claude_setup;
use crate::driver::gemini::setup as gemini_setup;

// -- Claude pristine --

#[test]
fn pristine_claude_no_settings_returns_session_id_and_coop_url() -> anyhow::Result<()> {
    let dir = Path::new("/tmp/test-pristine");
    let setup = claude_setup::prepare(dir, "http://127.0.0.1:8080", None, None, true, None, None)?;

    // Pristine mode: no --session-id passed to Claude Code (coop tracks it internally).
    assert_eq!(setup.extra_args.len(), 0);

    // Only COOP_URL (no COOP_HOOK_PIPE)
    assert_eq!(setup.env_vars.len(), 1);
    assert_eq!(setup.env_vars[0].0, "COOP_URL");
    assert_eq!(setup.env_vars[0].1, "http://127.0.0.1:8080");

    // Session log path for Tier 2
    assert!(setup.session_log_path.is_some());

    // No hook pipe in pristine mode
    assert!(setup.hook_pipe_path.is_none());
    Ok(())
}

#[test]
fn pristine_claude_with_settings_writes_file_without_hooks() -> anyhow::Result<()> {
    let dir = Path::new("/tmp/test-pristine");
    let settings = json!({
        "permissions": { "allow": ["Bash"] },
        "env": { "FOO": "bar" }
    });
    let setup = claude_setup::prepare(
        dir,
        "http://127.0.0.1:8080",
        Some(&settings),
        None,
        true,
        None,
        None,
    )?;

    // --settings <path> (no --session-id in pristine mode)
    assert_eq!(setup.extra_args.len(), 2);
    assert_eq!(setup.extra_args[0], "--settings");
    let settings_path = Path::new(&setup.extra_args[1]);
    assert!(settings_path.exists());

    // Settings file should NOT contain hooks (no coop hook merge)
    let written: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(settings_path)?)?;
    assert!(written.get("hooks").is_none());
    assert_eq!(written["permissions"]["allow"][0], "Bash");

    // No COOP_HOOK_PIPE
    assert!(!setup.env_vars.iter().any(|(k, _)| k == "COOP_HOOK_PIPE"));
    Ok(())
}

#[test]
fn pristine_claude_with_mcp_writes_mcp_config() -> anyhow::Result<()> {
    let dir = Path::new("/tmp/test-pristine");
    let mcp = json!({
        "my-server": { "command": "node", "args": ["server.js"] }
    });
    let setup =
        claude_setup::prepare(dir, "http://127.0.0.1:8080", None, Some(&mcp), true, None, None)?;

    // --mcp-config <path> (no --session-id in pristine mode)
    assert_eq!(setup.extra_args.len(), 2);
    assert_eq!(setup.extra_args[0], "--mcp-config");
    let mcp_path = Path::new(&setup.extra_args[1]);
    assert!(mcp_path.exists());

    let written: serde_json::Value = serde_json::from_str(&std::fs::read_to_string(mcp_path)?)?;
    assert!(written.get("mcpServers").is_some());
    assert!(written["mcpServers"]["my-server"]["command"].as_str() == Some("node"));
    Ok(())
}

// -- inject_extra_args --

#[test]
fn inject_extra_args_sh_c_appends_to_script() {
    let mut cmd = vec!["sh".into(), "-c".into(), "claude --dangerously-skip-permissions".into()];
    super::inject_extra_args(&mut cmd, &["--settings".into(), "/tmp/settings.json".into()]);

    assert_eq!(cmd.len(), 3, "should still be 3 elements (sh, -c, script)");
    assert_eq!(cmd[0], "sh");
    assert_eq!(cmd[1], "-c");
    assert_eq!(cmd[2], "claude --dangerously-skip-permissions --settings /tmp/settings.json");
}

#[test]
fn inject_extra_args_sh_c_quotes_paths_with_spaces() {
    let mut cmd = vec!["sh".into(), "-c".into(), "claude".into()];
    super::inject_extra_args(&mut cmd, &["--settings".into(), "/tmp/my dir/settings.json".into()]);

    assert_eq!(cmd[2], "claude --settings '/tmp/my dir/settings.json'");
}

#[test]
fn inject_extra_args_direct_command_extends_vector() {
    let mut cmd = vec!["claude".into(), "--dangerously-skip-permissions".into()];
    super::inject_extra_args(&mut cmd, &["--settings".into(), "/tmp/settings.json".into()]);

    assert_eq!(
        cmd,
        vec!["claude", "--dangerously-skip-permissions", "--settings", "/tmp/settings.json"]
    );
}

#[test]
fn inject_extra_args_empty_is_noop() {
    let mut cmd = vec!["sh".into(), "-c".into(), "claude".into()];
    let original = cmd.clone();
    super::inject_extra_args(&mut cmd, &[]);
    assert_eq!(cmd, original);
}

#[test]
fn inject_extra_args_resume_with_settings() {
    let mut cmd = vec!["sh".into(), "-c".into(), "claude --dangerously-skip-permissions".into()];
    super::inject_extra_args(
        &mut cmd,
        &["--resume".into(), "sess-42".into(), "--settings".into(), "/tmp/s.json".into()],
    );

    assert_eq!(
        cmd[2],
        "claude --dangerously-skip-permissions --resume sess-42 --settings /tmp/s.json"
    );
}

#[test]
fn inject_extra_args_sh_c_single_quote_in_path() {
    let mut cmd = vec!["sh".into(), "-c".into(), "claude".into()];
    super::inject_extra_args(
        &mut cmd,
        &["--settings".into(), "/tmp/it's/here/settings.json".into()],
    );

    // Single quote inside the path must be escaped as '\'' inside single-quoted string.
    assert_eq!(cmd[2], "claude --settings '/tmp/it'\\''s/here/settings.json'");
}

#[test]
fn inject_extra_args_empty_command_vector() {
    let mut cmd: Vec<String> = vec![];
    super::inject_extra_args(&mut cmd, &["--settings".into(), "/tmp/s.json".into()]);

    // Empty command: falls through to the extend branch, args just get appended.
    assert_eq!(cmd, vec!["--settings", "/tmp/s.json"]);
}

#[test]
fn inject_extra_args_four_element_with_dash_c_not_detected_as_sh_c() {
    // More than 3 elements with "-c" present should NOT trigger the sh -c path.
    let mut cmd = vec!["env".into(), "FOO=bar".into(), "sh".into(), "-c".into(), "claude".into()];
    super::inject_extra_args(&mut cmd, &["--settings".into(), "/tmp/s.json".into()]);

    // Should extend the vector directly, not modify any script string.
    assert_eq!(cmd, vec!["env", "FOO=bar", "sh", "-c", "claude", "--settings", "/tmp/s.json"]);
}

#[test]
fn inject_extra_args_bash_c_detected_as_shell() {
    // "bash -c <script>" should be handled the same as "sh -c <script>".
    let mut cmd = vec!["bash".into(), "-c".into(), "claude --dangerously-skip-permissions".into()];
    super::inject_extra_args(&mut cmd, &["--resume".into(), "sess-99".into()]);

    assert_eq!(cmd.len(), 3, "should still be 3 elements (bash, -c, script)");
    assert_eq!(cmd[0], "bash");
    assert_eq!(cmd[1], "-c");
    assert_eq!(cmd[2], "claude --dangerously-skip-permissions --resume sess-99");
}

// -- Gemini pristine --

#[test]
fn pristine_gemini_with_settings_and_mcp() -> anyhow::Result<()> {
    let settings = json!({ "theme": "dark" });
    let mcp = json!({
        "tool-server": { "command": "python", "args": ["serve.py"] }
    });
    let setup = gemini_setup::prepare("http://127.0.0.1:8080", Some(&settings), Some(&mcp), true)?;

    // No CLI args for Gemini
    assert!(setup.extra_args.is_empty());

    // COOP_URL + GEMINI_CLI_SYSTEM_SETTINGS_PATH
    assert_eq!(setup.env_vars.len(), 2);
    assert_eq!(setup.env_vars[0].0, "COOP_URL");
    let settings_env = setup.env_vars.iter().find(|(k, _)| k == "GEMINI_CLI_SYSTEM_SETTINGS_PATH");
    assert!(settings_env.is_some());

    // Settings file has MCP embedded and no hooks
    let settings_val = settings_env.map(|(_, v)| v.clone()).unwrap_or_default();
    let settings_path = Path::new(&settings_val);
    let written: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(settings_path)?)?;
    assert_eq!(written["theme"], "dark");
    assert!(written["mcpServers"]["tool-server"]["command"].as_str() == Some("python"));
    assert!(written.get("hooks").is_none());

    // No session log path for Gemini
    assert!(setup.session_log_path.is_none());

    // No hook pipe in pristine mode
    assert!(setup.hook_pipe_path.is_none());
    Ok(())
}

// -- Signal forwarding to PTY pgroup (ka-0000005033) --

/// `forward_signal_to_pty_pgroup` must successfully deliver the requested
/// signal to the foreground process group of a forked child. We verify with
/// the harmless `SIGUSR1` so we don't risk killing test infrastructure on
/// failure: the child installs a trap that increments a counter file, then
/// we read it after forwarding.
///
/// Regression test for ka-0000005033: SIGTERM from the operator must reach
/// the PTY child's process group so traps inside the child can fire before
/// NativePty::drop escalates to SIGHUP/SIGKILL.
#[test]
fn forward_signal_delivers_to_pty_process_group() -> anyhow::Result<()> {
    use nix::sys::signal::Signal;
    use std::process::{Command, Stdio};
    use std::time::{Duration, Instant};

    let marker = format!("/tmp/coop-fwd-sig-test-{}", std::process::id());
    let _ = std::fs::remove_file(&marker);

    // Spawn a bash that:
    //  1. setsid's itself so it owns a fresh process group (matching forkpty)
    //  2. installs USR1 trap → writes USR1_FIRED to the marker
    //  3. sleeps in a loop responsive to signals
    let script = format!(
        r#"setsid bash -c 'trap "echo USR1_FIRED >> {marker}; exit 0" USR1; echo READY > {marker}; for _ in $(seq 1 30); do sleep 0.2; done; echo TIMEOUT >> {marker}' &
        echo $! > {marker}.pid
        wait
        "#
    );
    let mut child = Command::new("bash")
        .arg("-c")
        .arg(&script)
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()?;

    // Wait for READY + pid file to exist.
    let start = Instant::now();
    let pid_path = format!("{marker}.pid");
    let mut child_pid: Option<i32> = None;
    while start.elapsed() < Duration::from_secs(5) {
        if std::path::Path::new(&marker).exists() && std::path::Path::new(&pid_path).exists() {
            if let Ok(s) = std::fs::read_to_string(&marker) {
                if s.contains("READY") {
                    let pid_str = std::fs::read_to_string(&pid_path)?;
                    child_pid = pid_str.trim().parse::<i32>().ok();
                    break;
                }
            }
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    let pid = child_pid.expect("inner bash never reached READY");

    // Forward USR1 to the inner bash's PG via our helper.
    super::forward_signal_to_pty_pgroup(Some(pid), Signal::SIGUSR1);

    // Poll for USR1_FIRED.
    let start = Instant::now();
    let mut fired = false;
    while start.elapsed() < Duration::from_secs(3) {
        if let Ok(s) = std::fs::read_to_string(&marker) {
            if s.contains("USR1_FIRED") {
                fired = true;
                break;
            }
        }
        std::thread::sleep(Duration::from_millis(100));
    }

    let _ = child.wait();
    let _ = std::fs::remove_file(&marker);
    let _ = std::fs::remove_file(&pid_path);

    assert!(fired, "USR1 trap should have fired in the PTY child group");
    Ok(())
}

#[test]
fn forward_signal_handles_missing_pid_gracefully() {
    use nix::sys::signal::Signal;
    // None / 0 / negative inputs must not panic — they're best-effort no-ops.
    super::forward_signal_to_pty_pgroup(None, Signal::SIGTERM);
    super::forward_signal_to_pty_pgroup(Some(0), Signal::SIGTERM);
    super::forward_signal_to_pty_pgroup(Some(-42), Signal::SIGTERM);
}

#[test]
fn forward_signal_swallows_esrch_for_dead_children() {
    use nix::sys::signal::Signal;
    // PID we'll almost certainly never own a session for. The call must
    // log & return rather than propagate the ESRCH (or EPERM) error.
    super::forward_signal_to_pty_pgroup(Some(999_999), Signal::SIGTERM);
}

// -- S3 resume: place restored transcript at Claude read path (ka-0000007228) --

#[cfg(feature = "s3")]
#[tokio::test]
async fn place_restored_transcript_copies_to_claude_path() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let src = dir.path().join("coop/sessions/key/session.jsonl");
    std::fs::create_dir_all(src.parent().expect("parent"))?;
    std::fs::write(&src, "{\"sessionId\":\"abc\"}\n")?;

    // Claude's read path does not exist yet (fresh projects/<hash>/ dir).
    let claude_log = dir.path().join("projects/-some-proj/abc.jsonl");
    assert!(!claude_log.exists());

    let placed = super::place_restored_transcript(&src, &claude_log).await?;
    assert!(placed, "transcript should be placed");
    assert!(claude_log.exists(), "transcript must exist at Claude read path");
    assert_eq!(std::fs::read_to_string(&claude_log)?, "{\"sessionId\":\"abc\"}\n");
    Ok(())
}

#[cfg(feature = "s3")]
#[tokio::test]
async fn place_restored_transcript_keeps_existing_local_copy() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let src = dir.path().join("session.jsonl");
    std::fs::write(&src, "from-s3\n")?;

    // A fresher local copy already exists — must NOT be overwritten.
    let claude_log = dir.path().join("projects/-some-proj/abc.jsonl");
    std::fs::create_dir_all(claude_log.parent().expect("parent"))?;
    std::fs::write(&claude_log, "fresher-local\n")?;

    let placed = super::place_restored_transcript(&src, &claude_log).await?;
    assert!(placed);
    assert_eq!(std::fs::read_to_string(&claude_log)?, "fresher-local\n");
    Ok(())
}

// -- force-clean predicate (--s3-no-restore, gardenio ka-0000007242) --

#[test]
fn force_clean_active_reflects_no_restore_flag() {
    // The controller sets --s3-no-restore / COOP_S3_NO_RESTORE when the prior
    // life crossed a turn/token/wake threshold. When set, prepare() must both
    // skip the S3 restore and drop any local --resume hint so the session starts
    // fresh; force_clean_active is the single predicate both sites consult.
    assert!(super::force_clean_active(true), "no-restore set => force clean");
    assert!(!super::force_clean_active(false), "default => resume as usual");
}

/// Drive the full `prepare()` path with force-clean ON and a local `--resume`
/// hint set: the hint must be dropped (the S3 restore is already skipped when no
/// bucket is configured) so the session boots fresh instead of resuming the
/// oversized transcript the controller flagged. Exercises both force-clean
/// suppression sites end-to-end through the real run codepath (the coverage
/// build skips claude_/attach_-prefixed integration tests, so this carries the
/// branch coverage for the `--s3-no-restore` logic). Mirrors
/// prepare_unknown_agent_constructs_lifecycle_and_runs for the run/cancel dance.
#[tokio::test]
async fn prepare_force_clean_drops_local_resume_hint() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    // A real, resumable-looking local transcript. Force-clean must ignore it.
    let stale = dir.path().join("stale-session.jsonl");
    std::fs::write(&stale, "{\"sessionId\":\"deadbeef\"}\n")?;

    let mut config = crate::config::Config::test();
    config.s3_no_restore = true;
    config.resume = Some(stale.display().to_string());

    let prepared = crate::run::prepare(config).await?;
    let shutdown = prepared.store.lifecycle.shutdown.clone();

    let handle = tokio::spawn(prepared.run());
    tokio::time::sleep(std::time::Duration::from_millis(200)).await;
    shutdown.cancel();
    let _ = handle.await??;
    // Reaching here proves prepare() ran the force-clean branches (skip restore +
    // clear the local hint) without resurrecting the stale transcript.
    Ok(())
}

// -- initial_profile_from_env (account-display seeding, ka-0000002509) --

use crate::driver::AgentType;

/// Build an env lookup from a fixed set of pairs for deterministic tests.
fn env_from(pairs: &[(&str, &str)]) -> impl Fn(&str) -> Option<String> {
    let map: std::collections::HashMap<String, String> =
        pairs.iter().map(|(k, v)| ((*k).to_owned(), (*v).to_owned())).collect();
    move |name: &str| map.get(name).cloned()
}

#[test]
fn initial_profile_seeds_named_account_from_oauth_token() {
    let get = env_from(&[
        ("BOAT_ACCOUNT", "matthewcbaker0122"),
        ("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-xyz"),
        ("ANTHROPIC_BASE_URL", "http://gardenio-litellm:4000"),
        ("ANTHROPIC_MODEL", "claude-opus-4-8-oauth"),
    ]);
    let (name, creds) = super::initial_profile_from(AgentType::Claude, get)
        .expect("should seed a profile when BOAT_ACCOUNT + token are set");
    assert_eq!(name, "matthewcbaker0122");
    assert_eq!(creds.get("CLAUDE_CODE_OAUTH_TOKEN").map(String::as_str), Some("sk-ant-oat-xyz"));
    // Routing env rides along so a switch-back restores the same upstream wiring.
    assert_eq!(
        creds.get("ANTHROPIC_BASE_URL").map(String::as_str),
        Some("http://gardenio-litellm:4000")
    );
    assert_eq!(creds.get("ANTHROPIC_MODEL").map(String::as_str), Some("claude-opus-4-8-oauth"));
}

#[test]
fn initial_profile_none_when_account_unset() {
    // No BOAT_ACCOUNT → no accurate account identity to report → no seed.
    let get = env_from(&[("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-xyz")]);
    assert!(super::initial_profile_from(AgentType::Claude, get).is_none());
}

#[test]
fn initial_profile_none_when_account_empty() {
    let get = env_from(&[("BOAT_ACCOUNT", ""), ("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-xyz")]);
    assert!(super::initial_profile_from(AgentType::Claude, get).is_none());
}

#[test]
fn initial_profile_none_when_no_credential() {
    // BOAT_ACCOUNT alone, with neither a token nor a base_url, is not usable.
    let get = env_from(&[("BOAT_ACCOUNT", "lonely-account")]);
    assert!(super::initial_profile_from(AgentType::Claude, get).is_none());
}

#[test]
fn initial_profile_seeds_bedrock_style_with_base_url_only() {
    // Bedrock-routed accounts authenticate via base_url + headers and may carry
    // no OAuth token — a base_url alone still counts as a usable profile.
    let get = env_from(&[
        ("BOAT_ACCOUNT", "litellm-bedrock"),
        ("ANTHROPIC_BASE_URL", "http://gardenio-litellm:4000"),
        ("ANTHROPIC_CUSTOM_HEADERS", "x-litellm-api-key: Bearer sk-proj-1"),
    ]);
    let (name, creds) = super::initial_profile_from(AgentType::Claude, get)
        .expect("base_url alone should be a usable profile");
    assert_eq!(name, "litellm-bedrock");
    assert_eq!(
        creds.get("ANTHROPIC_BASE_URL").map(String::as_str),
        Some("http://gardenio-litellm:4000")
    );
    assert!(!creds.contains_key("CLAUDE_CODE_OAUTH_TOKEN"));
}

/// The seeded profile must become Active so active_name() reports the account
/// the pod is actually using — the core fix for the blank/None dashboard value.
#[tokio::test]
async fn seeded_profile_is_reported_as_active_name() -> anyhow::Result<()> {
    let get = env_from(&[
        ("BOAT_ACCOUNT", "matthewcbaker0122"),
        ("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-xyz"),
    ]);
    let (name, creds) = super::initial_profile_from(AgentType::Claude, get).expect("seed");

    let profile = crate::profile::ProfileState::new();
    profile
        .register(vec![crate::profile::ProfileEntry { name, credentials: creds, usable: None }])
        .await;

    assert_eq!(profile.active_name().await.as_deref(), Some("matthewcbaker0122"));
    Ok(())
}

// -- T1: real-env seed-call-site coverage (DIM-C gap, ka-0000004278) --
//
// The tests above prove the pure DERIVATION helper (`initial_profile_from` with
// an injected lookup) and that a manually-registered profile becomes active.
// Neither proves the WIRING: that `seed_initial_profile` — the thing `prepare`
// actually calls — reads the REAL `BOAT_ACCOUNT` from `std::env` and registers
// it so `active_name()` reports it. The Playwright e2e suite can't catch a
// regression here either: it drives a mock coop that hardcodes `account_name`.
// These tests close that gap by driving the real env→active_name() path.
//
// They mutate process-global env, so they are `#[serial]` and restore prior
// values via a guard that runs even on panic.

/// RAII guard that sets env vars for the test and restores their prior values
/// (or removes them if previously unset) on drop — so a panicking assert can't
/// leak `BOAT_ACCOUNT`/token into another serial test.
struct EnvGuard {
    prev: Vec<(&'static str, Option<String>)>,
}

impl EnvGuard {
    fn set(pairs: &[(&'static str, &str)]) -> Self {
        let prev = pairs
            .iter()
            .map(|(k, v)| {
                let old = std::env::var(k).ok();
                std::env::set_var(k, v);
                (*k, old)
            })
            .collect();
        Self { prev }
    }
}

impl Drop for EnvGuard {
    fn drop(&mut self) {
        for (k, old) in &self.prev {
            match old {
                Some(v) => std::env::set_var(k, v),
                None => std::env::remove_var(k),
            }
        }
    }
}

/// The real wiring: `seed_initial_profile` reads `BOAT_ACCOUNT` +
/// `CLAUDE_CODE_OAUTH_TOKEN` from the actual environment (as the controller
/// bakes them) and registers the account so `active_name()` returns it. This is
/// the exact call `prepare` makes; if the seed block were deleted, mis-guarded,
/// or the env-key list drifted from what the controller injects, this fails —
/// whereas the mock-driven e2e suite would stay green.
#[tokio::test]
#[serial_test::serial]
async fn seed_initial_profile_reports_boat_account_as_active_from_real_env() {
    let _env = EnvGuard::set(&[
        ("BOAT_ACCOUNT", "gardenio2"),
        ("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-real-env"),
    ]);

    let profile = crate::profile::ProfileState::new();
    super::seed_initial_profile(&profile, AgentType::Claude).await;

    assert_eq!(
        profile.active_name().await.as_deref(),
        Some("gardenio2"),
        "seed_initial_profile should make BOAT_ACCOUNT the active profile from real env"
    );
}

/// Don't-invent-an-account: with `BOAT_ACCOUNT` unset, the real seed path must
/// leave the profile store empty so `active_name()` stays `None` (the dashboard
/// shows nothing rather than a fabricated account). Mirrors the pure-helper
/// `initial_profile_none_when_account_unset` test at the call-site level.
#[tokio::test]
#[serial_test::serial]
async fn seed_initial_profile_no_active_when_boat_account_unset() {
    // Ensure BOAT_ACCOUNT is absent regardless of ambient env, then restore.
    let prev = std::env::var("BOAT_ACCOUNT").ok();
    std::env::remove_var("BOAT_ACCOUNT");

    let profile = crate::profile::ProfileState::new();
    super::seed_initial_profile(&profile, AgentType::Claude).await;
    let active = profile.active_name().await;

    if let Some(v) = prev {
        std::env::set_var("BOAT_ACCOUNT", v);
    }
    assert_eq!(active, None, "no BOAT_ACCOUNT must not seed/activate any account");
}

// -- x-litellm-tags preservation across credential switches (ka-0000000333) --
//
// The controller bakes ANTHROPIC_CUSTOM_HEADERS as two lines (api-key + tags);
// broker-pushed rotation credentials carry only the api-key line. The merge in
// write_switch_metadata must re-attach the pod's tags line so per-agent spend
// attribution survives rotation.

const POD_HEADERS: &str = "x-litellm-api-key: Bearer sk-old\n\
    x-litellm-tags: agent:crew-1,role:crew,project:gardenio,model:claude-opus-4-8";

#[test]
fn merge_litellm_tags_appends_pod_tags_to_broker_headers() {
    let merged = super::merge_litellm_tags("x-litellm-api-key: Bearer sk-new", POD_HEADERS);
    assert_eq!(
        merged,
        "x-litellm-api-key: Bearer sk-new\n\
         x-litellm-tags: agent:crew-1,role:crew,project:gardenio,model:claude-opus-4-8"
    );
}

#[test]
fn merge_litellm_tags_keeps_incoming_tags_when_present() {
    // A tags-aware broker (future) wins over the pod env — no duplicate line.
    let incoming = "x-litellm-api-key: Bearer sk-new\nx-litellm-tags: agent:other";
    assert_eq!(super::merge_litellm_tags(incoming, POD_HEADERS), incoming);
}

#[test]
fn merge_litellm_tags_noop_when_pod_env_has_no_tags() {
    let incoming = "x-litellm-api-key: Bearer sk-new";
    assert_eq!(super::merge_litellm_tags(incoming, "x-litellm-api-key: Bearer sk-old"), incoming);
}

#[test]
fn merged_switch_credentials_reattaches_tags_line() {
    let creds = std::collections::HashMap::from([
        ("ANTHROPIC_CUSTOM_HEADERS".to_owned(), "x-litellm-api-key: Bearer sk-new".to_owned()),
        ("CLAUDE_CODE_OAUTH_TOKEN".to_owned(), "sk-ant-oat-new".to_owned()),
    ]);
    let get = env_from(&[("ANTHROPIC_CUSTOM_HEADERS", POD_HEADERS)]);

    let merged = super::merged_switch_credentials(Some(&creds), get).expect("some creds");
    assert_eq!(
        merged.get("ANTHROPIC_CUSTOM_HEADERS").map(String::as_str),
        Some(
            "x-litellm-api-key: Bearer sk-new\n\
             x-litellm-tags: agent:crew-1,role:crew,project:gardenio,model:claude-opus-4-8"
        )
    );
    // Other credential keys pass through untouched.
    assert_eq!(merged.get("CLAUDE_CODE_OAUTH_TOKEN").map(String::as_str), Some("sk-ant-oat-new"));
}

#[test]
fn merged_switch_credentials_untouched_without_headers_key() {
    // A switch that doesn't carry ANTHROPIC_CUSTOM_HEADERS (e.g. plain OAuth
    // failover) must pass through unchanged — no tags line invented.
    let creds = std::collections::HashMap::from([(
        "CLAUDE_CODE_OAUTH_TOKEN".to_owned(),
        "sk-ant-oat-new".to_owned(),
    )]);
    let get = env_from(&[("ANTHROPIC_CUSTOM_HEADERS", POD_HEADERS)]);

    let merged = super::merged_switch_credentials(Some(&creds), get).expect("some creds");
    assert!(!merged.contains_key("ANTHROPIC_CUSTOM_HEADERS"));
}

#[test]
fn merged_switch_credentials_none_passthrough() {
    let get = env_from(&[("ANTHROPIC_CUSTOM_HEADERS", POD_HEADERS)]);
    assert!(super::merged_switch_credentials(None, get).is_none());
}

/// Drive the full `prepare()` → `run()` path for a non-agent (Unknown) session
/// so the `LifecycleState` construction — including the `idle_suspended` bit
/// added for snapshot-suspend (ka-0000002921) — is exercised end-to-end. The
/// `echo` command exits immediately; on a non-switch terminal exit the run loop
/// then awaits a switch or shutdown, so we cancel the shutdown token to let it
/// finalize. (Named without a `claude_`/`attach_` prefix so it runs under the
/// coverage build, which skips those integration prefixes.)
#[tokio::test]
async fn prepare_unknown_agent_constructs_lifecycle_and_runs() -> anyhow::Result<()> {
    let prepared = crate::run::prepare(crate::config::Config::test()).await?;
    let shutdown = prepared.store.lifecycle.shutdown.clone();
    let lifecycle_store = std::sync::Arc::clone(&prepared.store);
    // The bit starts clear; a plain echo exit is not an idle-suspend.
    assert!(
        !lifecycle_store.lifecycle.idle_suspended.load(std::sync::atomic::Ordering::Acquire),
        "idle_suspended must start clear"
    );

    let handle = tokio::spawn(prepared.run());
    // `echo` exits promptly; the run loop then awaits switch-or-shutdown, so
    // cancel the shutdown token to release it.
    tokio::time::sleep(std::time::Duration::from_millis(200)).await;
    shutdown.cancel();
    let result = handle.await??;

    assert!(
        !result.store.lifecycle.idle_suspended.load(std::sync::atomic::Ordering::Acquire),
        "a normal exit must not set idle_suspended"
    );
    Ok(())
}

// -- Codex credentials (gd-hoYFy5n8mh) --

const CODEX_DOC: &str = r#"{"auth_mode":"chatgpt","tokens":{"access_token":"at","refresh_token":"gardenio-broker-managed"}}"#;

#[test]
fn initial_profile_seeds_codex_login_and_model() {
    let get = env_from(&[
        ("BOAT_ACCOUNT", "codex-a"),
        ("CODEX_AUTH_JSON", CODEX_DOC),
        ("CODEX_MODEL", "gpt-5-codex"),
    ]);
    let (name, creds) = super::initial_profile_from(AgentType::Codex, get)
        .expect("a Codex login alone is a usable credential");
    assert_eq!(name, "codex-a");
    assert_eq!(creds.get("CODEX_AUTH_JSON").map(String::as_str), Some(CODEX_DOC));
    assert_eq!(creds.get("CODEX_MODEL").map(String::as_str), Some("gpt-5-codex"));
    assert!(!creds.contains_key("OPENAI_API_KEY"));
}

#[test]
#[serial_test::serial]
fn apply_credentials_writes_codex_login_and_keeps_it_out_of_env() {
    let dir = tempfile::tempdir().unwrap();
    std::env::set_var("CODEX_HOME", dir.path());
    let mut env = Vec::new();
    let overrides = std::collections::HashMap::from([
        ("CODEX_AUTH_JSON".to_owned(), CODEX_DOC.to_owned()),
        ("CODEX_MODEL".to_owned(), "gpt-5-codex".to_owned()),
    ]);
    super::apply_credentials(&mut env, &overrides, AgentType::Codex);
    std::env::remove_var("CODEX_HOME");

    assert_eq!(std::fs::read_to_string(dir.path().join("auth.json")).unwrap(), CODEX_DOC);
    assert_eq!(env, [("CODEX_MODEL".to_owned(), "gpt-5-codex".to_owned())]);
}

#[test]
#[serial_test::serial]
fn apply_credentials_does_not_write_codex_login_for_other_agents() {
    let dir = tempfile::tempdir().unwrap();
    std::env::set_var("CODEX_HOME", dir.path());
    let overrides =
        std::collections::HashMap::from([("CODEX_AUTH_JSON".to_owned(), CODEX_DOC.to_owned())]);
    super::apply_credentials(&mut Vec::new(), &overrides, AgentType::Gemini);
    std::env::remove_var("CODEX_HOME");
    assert!(!dir.path().join("auth.json").exists());
}

#[test]
fn initial_profile_ignores_other_harness_env() {
    // A Claude pod with a stray Codex login in its env seeds only Claude keys...
    let get = env_from(&[
        ("BOAT_ACCOUNT", "claude-a"),
        ("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-xyz"),
        ("CODEX_AUTH_JSON", CODEX_DOC),
        ("CODEX_MODEL", "gpt-5-codex"),
    ]);
    let (_, creds) = super::initial_profile_from(AgentType::Claude, get).expect("claude profile");
    assert!(!creds.contains_key("CODEX_AUTH_JSON") && !creds.contains_key("CODEX_MODEL"));
    assert!(creds.contains_key("CLAUDE_CODE_OAUTH_TOKEN"));

    // ...and a Codex pod never seeds a Claude token.
    let get = env_from(&[
        ("BOAT_ACCOUNT", "codex-a"),
        ("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-xyz"),
        ("CODEX_AUTH_JSON", CODEX_DOC),
    ]);
    let (_, creds) = super::initial_profile_from(AgentType::Codex, get).expect("codex profile");
    assert!(!creds.contains_key("CLAUDE_CODE_OAUTH_TOKEN"));
    assert!(creds.contains_key("CODEX_AUTH_JSON"));

    // A Codex pod with only a Claude token has nothing to seed.
    let get = env_from(&[("BOAT_ACCOUNT", "x"), ("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-xyz")]);
    assert!(super::initial_profile_from(AgentType::Codex, get).is_none());
}

#[test]
fn switch_file_is_owner_only_even_over_a_stale_world_readable_file() {
    use std::os::unix::fs::PermissionsExt;

    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("coop_credential_switch.json");
    std::fs::write(&path, "stale").unwrap();
    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();

    super::write_switch_file(path.to_str().unwrap(), "{\"credentials\":{}}").unwrap();

    assert_eq!(std::fs::metadata(&path).unwrap().permissions().mode() & 0o777, 0o600);
    assert_eq!(std::fs::read_to_string(&path).unwrap(), "{\"credentials\":{}}");

    // A fresh file is created 0600 too.
    let fresh = dir.path().join("fresh.json");
    super::write_switch_file(fresh.to_str().unwrap(), "{}").unwrap();
    assert_eq!(std::fs::metadata(&fresh).unwrap().permissions().mode() & 0o777, 0o600);
}
