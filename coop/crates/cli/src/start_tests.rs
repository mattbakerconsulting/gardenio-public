// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::collections::BTreeMap;

use super::*;

#[test]
fn default_start_config_is_empty() {
    let config = StartConfig::default();
    assert!(config.text.is_none());
    assert!(config.shell.is_empty());
    assert!(config.event.is_empty());
}

#[test]
fn deserialize_empty_object_is_defaults() -> anyhow::Result<()> {
    let config: StartConfig = serde_json::from_str("{}")?;
    assert!(config.text.is_none());
    assert!(config.shell.is_empty());
    assert!(config.event.is_empty());
    Ok(())
}

#[test]
fn roundtrip_text_only() -> anyhow::Result<()> {
    let config =
        StartConfig { text: Some("hello world".to_owned()), shell: vec![], event: BTreeMap::new() };
    let json = serde_json::to_string(&config)?;
    let parsed: StartConfig = serde_json::from_str(&json)?;
    assert_eq!(parsed.text.as_deref(), Some("hello world"));
    assert!(parsed.shell.is_empty());
    Ok(())
}

#[test]
fn roundtrip_shell_only() -> anyhow::Result<()> {
    let config = StartConfig {
        text: None,
        shell: vec!["echo hello".to_owned(), "echo world".to_owned()],
        event: BTreeMap::new(),
    };
    let json = serde_json::to_string(&config)?;
    let parsed: StartConfig = serde_json::from_str(&json)?;
    assert!(parsed.text.is_none());
    assert_eq!(parsed.shell.len(), 2);
    Ok(())
}

#[test]
fn roundtrip_with_events() -> anyhow::Result<()> {
    let mut events = BTreeMap::new();
    events.insert(
        "clear".to_owned(),
        StartEventConfig {
            text: Some("clear context".to_owned()),
            shell: vec!["echo clearing".to_owned()],
        },
    );
    let config = StartConfig {
        text: Some("default text".to_owned()),
        shell: vec!["echo default".to_owned()],
        event: events,
    };
    let json = serde_json::to_string(&config)?;
    let parsed: StartConfig = serde_json::from_str(&json)?;
    assert_eq!(parsed.text.as_deref(), Some("default text"));
    assert_eq!(parsed.shell.len(), 1);
    assert!(parsed.event.contains_key("clear"));
    let clear = &parsed.event["clear"];
    assert_eq!(clear.text.as_deref(), Some("clear context"));
    assert_eq!(clear.shell.len(), 1);
    Ok(())
}

#[test]
fn roundtrip_combined() -> anyhow::Result<()> {
    let mut events = BTreeMap::new();
    events.insert(
        "resume".to_owned(),
        StartEventConfig { text: Some("resumed".to_owned()), shell: vec![] },
    );
    let config = StartConfig {
        text: Some("top-level".to_owned()),
        shell: vec!["cmd1".to_owned(), "cmd2".to_owned()],
        event: events,
    };
    let json = serde_json::to_string(&config)?;
    let parsed: StartConfig = serde_json::from_str(&json)?;
    assert_eq!(parsed.text.as_deref(), Some("top-level"));
    assert_eq!(parsed.shell.len(), 2);
    assert!(parsed.event.contains_key("resume"));
    Ok(())
}

#[test]
fn empty_config_returns_empty() {
    let config = StartConfig::default();
    assert_eq!(compose_start_script(&config, "start"), "");
}

#[test]
fn text_only_returns_base64_printf() {
    let config = StartConfig { text: Some("hello".to_owned()), ..Default::default() };
    let script = compose_start_script(&config, "start");
    assert!(script.contains("printf '%s'"));
    assert!(script.contains("base64 -d"));
    // Verify base64 encoding of "hello"
    let encoded = base64::engine::general_purpose::STANDARD.encode(b"hello");
    assert!(script.contains(&encoded));
}

#[test]
fn shell_only_returns_commands() {
    let config = StartConfig {
        shell: vec!["echo one".to_owned(), "echo two".to_owned()],
        ..Default::default()
    };
    let script = compose_start_script(&config, "start");
    assert_eq!(script, "echo one\necho two");
}

#[test]
fn text_and_shell_combined() {
    let config = StartConfig {
        text: Some("ctx".to_owned()),
        shell: vec!["echo done".to_owned()],
        ..Default::default()
    };
    let script = compose_start_script(&config, "start");
    let lines: Vec<&str> = script.lines().collect();
    assert_eq!(lines.len(), 2);
    assert!(lines[0].contains("base64 -d"), "first line should be text: {}", lines[0]);
    assert_eq!(lines[1], "echo done");
}

#[test]
fn event_override_used_when_source_matches() {
    let mut events = BTreeMap::new();
    events.insert(
        "clear".to_owned(),
        StartEventConfig {
            text: Some("override text".to_owned()),
            shell: vec!["echo override".to_owned()],
        },
    );
    let config = StartConfig {
        text: Some("default text".to_owned()),
        shell: vec!["echo default".to_owned()],
        event: events,
    };
    let script = compose_start_script(&config, "clear");
    assert!(script.contains("override"), "should use event override: {script}");
    assert!(!script.contains("default"), "should not contain default: {script}");
}

#[test]
fn fallback_to_top_level_when_source_not_in_event_map() {
    let mut events = BTreeMap::new();
    events.insert(
        "clear".to_owned(),
        StartEventConfig { text: Some("clear text".to_owned()), shell: vec![] },
    );
    let config = StartConfig {
        text: Some("default text".to_owned()),
        shell: vec!["echo fallback".to_owned()],
        event: events,
    };
    let script = compose_start_script(&config, "resume");
    assert!(script.contains("fallback"), "should fall back to top-level: {script}");
}

#[test]
fn no_fallback_when_top_level_also_empty() {
    let mut events = BTreeMap::new();
    events.insert("clear".to_owned(), StartEventConfig::default());
    let config = StartConfig { event: events, ..Default::default() };
    assert_eq!(compose_start_script(&config, "resume"), "");
}

#[test]
fn empty_text_is_skipped() {
    let config = StartConfig { text: Some(String::new()), ..Default::default() };
    assert_eq!(compose_start_script(&config, "start"), "");
}

#[test]
fn empty_shell_commands_are_skipped() {
    let config = StartConfig {
        shell: vec![String::new(), "echo real".to_owned(), String::new()],
        ..Default::default()
    };
    assert_eq!(compose_start_script(&config, "start"), "echo real");
}

#[test]
fn start_state_emit_increments_seq() {
    let state = StartState::new(StartConfig::default());
    let mut rx = state.start_tx.subscribe();

    let e1 = state.emit("start".to_owned(), None, false);
    assert_eq!(e1.seq, 0);
    assert_eq!(e1.source, "start");
    assert!(!e1.injected);

    let e2 = state.emit("resume".to_owned(), Some("sess-1".to_owned()), true);
    assert_eq!(e2.seq, 1);
    assert_eq!(e2.source, "resume");
    assert!(e2.injected);

    let received = rx.try_recv().expect("should receive event");
    assert_eq!(received.seq, 0);
}
