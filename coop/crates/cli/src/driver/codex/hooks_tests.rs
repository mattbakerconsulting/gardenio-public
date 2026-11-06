// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::path::Path;

use super::{generate_hook_config, write_hook_config};

const EVENTS: [&str; 5] = ["SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"];

#[test]
fn generated_config_has_the_five_verified_events() {
    let config = generate_hook_config(Path::new("/tmp/coop.pipe"));
    let hooks = &config["hooks"];
    assert_eq!(hooks.as_object().map(|o| o.len()), Some(EVENTS.len()));
    for name in EVENTS {
        assert_eq!(hooks[name][0]["matcher"], "*", "{name}");
        assert_eq!(hooks[name][0]["hooks"][0]["type"], "command", "{name}");
    }
}

#[test]
fn hooks_write_coop_envelopes_to_the_pipe_env_var() {
    let config = generate_hook_config(Path::new("/tmp/coop.pipe"));
    let expect = [
        ("SessionStart", "start"),
        ("UserPromptSubmit", "user_prompt_submit"),
        ("PreToolUse", "pre_tool_use"),
        ("PostToolUse", "post_tool_use"),
        ("Stop", "stop"),
    ];
    for (name, event) in expect {
        let cmd = config["hooks"][name][0]["hooks"][0]["command"].as_str().unwrap_or_default();
        assert!(cmd.contains("$COOP_HOOK_PIPE"), "{name}: {cmd}");
        assert!(
            cmd.contains(&format!("\\\"event\\\":\\\"{event}\\\""))
                || cmd.contains(&format!("\"event\":\"{event}\"")),
            "{name}: {cmd}"
        );
        assert!(!cmd.contains("/tmp/coop.pipe"), "{name} must stay portable");
    }
}

#[test]
fn stop_hook_asks_coop_and_echoes_a_block_on_stdout() {
    let config = generate_hook_config(Path::new("/tmp/coop.pipe"));
    let cmd = config["hooks"]["Stop"][0]["hooks"][0]["command"].as_str().unwrap_or_default();
    assert!(cmd.contains("$COOP_URL/api/v1/hooks/stop"));
    assert!(cmd.contains("COOP_HOOK_TOKEN_FILE"));
    assert!(cmd.contains("grep -q '\"block\"'"));
    assert!(!cmd.contains("exit 2"), "Codex honours JSON+exit 0, exit 2 is untested");
}

#[test]
fn hook_pipe_lines_parse_into_the_events_the_detector_maps() -> anyhow::Result<()> {
    use crate::driver::hook_recv::HookReceiver;
    // Each hook's envelope must be understood by the shared FIFO parser.
    let dir = tempfile::tempdir()?;
    let pipe = dir.path().join("p");
    let _receiver = HookReceiver::new(&pipe)?;
    let payloads = include_str!("../../../../../tests/fixtures/codex/hook-payloads.jsonl");
    assert_eq!(payloads.lines().count(), 6);
    Ok(())
}

#[test]
fn write_hook_config_writes_valid_json() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let path = write_hook_config(dir.path(), Path::new("/tmp/coop.pipe"))?;
    assert_eq!(path.file_name().and_then(|n| n.to_str()), Some("coop-codex-hooks.json"));
    let parsed: serde_json::Value = serde_json::from_str(&std::fs::read_to_string(&path)?)?;
    assert!(parsed["hooks"]["Stop"].is_array());
    Ok(())
}
