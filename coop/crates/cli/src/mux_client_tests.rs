// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;
use serial_test::serial;
use std::collections::HashMap;

fn env_from(vars: &[(&str, &str)]) -> impl Fn(&str) -> Option<String> {
    let map: HashMap<String, String> =
        vars.iter().map(|&(k, v)| (k.to_owned(), v.to_owned())).collect();
    move |name: &str| map.get(name).cloned()
}

#[test]
#[serial]
fn outbound_only_false_when_unset() {
    std::env::remove_var("COOP_OUTBOUND_ONLY");
    assert!(!outbound_only());
}

#[test]
#[serial]
fn outbound_only_true_when_set_truthy() {
    std::env::set_var("COOP_OUTBOUND_ONLY", "1");
    assert!(outbound_only());
    std::env::remove_var("COOP_OUTBOUND_ONLY");
}

#[test]
#[serial]
fn outbound_only_false_when_set_empty() {
    // An explicitly-empty value is falsy — mirrors the COOP_MUX_URL="" disable
    // idiom so an empty export never accidentally suppresses registration.
    std::env::set_var("COOP_OUTBOUND_ONLY", "");
    assert!(!outbound_only());
    std::env::remove_var("COOP_OUTBOUND_ONLY");
}

#[test]
fn detect_metadata_returns_agent_outside_k8s() {
    let result = detect_metadata_with("claude", &[], env_from(&[]));
    assert_eq!(result.get("agent"), Some(&Value::String("claude".into())));
    // No k8s key when not in Kubernetes.
    assert_eq!(result.get("k8s"), None);
}

#[test]
fn detect_metadata_returns_k8s_when_env_set() {
    let result = detect_metadata_with(
        "claude",
        &[],
        env_from(&[("KUBERNETES_SERVICE_HOST", "10.0.0.1"), ("POD_NAME", "my-pod-abc123")]),
    );

    assert_eq!(result.get("agent"), Some(&Value::String("claude".into())));
    let k8s = result.get("k8s").expect("expected k8s key");
    assert_eq!(k8s.get("pod"), Some(&Value::String("my-pod-abc123".into())));
    // No other fields should be present since we didn't set them.
    assert_eq!(k8s.get("namespace"), None);
    assert_eq!(k8s.get("node"), None);
    assert_eq!(k8s.get("ip"), None);
    assert_eq!(k8s.get("service_account"), None);
}

#[test]
fn detect_metadata_pod_name_takes_priority_over_hostname() {
    let result = detect_metadata_with(
        "claude",
        &[],
        env_from(&[
            ("KUBERNETES_SERVICE_HOST", "10.0.0.1"),
            ("POD_NAME", "real-pod"),
            ("HOSTNAME", "hostname-fallback"),
        ]),
    );

    let k8s = result.get("k8s").expect("expected k8s key");
    assert_eq!(k8s.get("pod"), Some(&Value::String("real-pod".into())));
}

#[test]
fn detect_metadata_falls_back_to_hostname() {
    let result = detect_metadata_with(
        "claude",
        &[],
        env_from(&[("KUBERNETES_SERVICE_HOST", "10.0.0.1"), ("HOSTNAME", "hostname-fallback")]),
    );

    let k8s = result.get("k8s").expect("expected k8s key");
    assert_eq!(k8s.get("pod"), Some(&Value::String("hostname-fallback".into())));
}

#[test]
fn detect_metadata_all_fields() {
    let result = detect_metadata_with(
        "gemini",
        &[],
        env_from(&[
            ("KUBERNETES_SERVICE_HOST", "10.0.0.1"),
            ("POD_NAME", "my-pod"),
            ("POD_NAMESPACE", "default"),
            ("NODE_NAME", "node-1"),
            ("POD_IP", "10.0.1.5"),
            ("POD_SERVICE_ACCOUNT", "my-sa"),
            ("BOAT_RUNTIME_CLASS", "runsc"),
        ]),
    );

    assert_eq!(result.get("agent"), Some(&Value::String("gemini".into())));
    let k8s = result.get("k8s").expect("expected k8s key");
    assert_eq!(k8s.get("pod"), Some(&Value::String("my-pod".into())));
    assert_eq!(k8s.get("namespace"), Some(&Value::String("default".into())));
    assert_eq!(k8s.get("node"), Some(&Value::String("node-1".into())));
    assert_eq!(k8s.get("ip"), Some(&Value::String("10.0.1.5".into())));
    assert_eq!(k8s.get("service_account"), Some(&Value::String("my-sa".into())));
    assert_eq!(k8s.get("runtime_class"), Some(&Value::String("runsc".into())));
}

#[test]
fn detect_metadata_runtime_class_absent_when_env_unset() {
    // No BOAT_RUNTIME_CLASS means the pod runs on the cluster default runtime;
    // the key is omitted (the UI renders "default") rather than sent empty.
    let result = detect_metadata_with(
        "claude",
        &[],
        env_from(&[("KUBERNETES_SERVICE_HOST", "10.0.0.1"), ("POD_NAME", "my-pod")]),
    );

    let k8s = result.get("k8s").expect("expected k8s key");
    assert_eq!(k8s.get("runtime_class"), None);
}

#[test]
fn detect_metadata_flat_labels() {
    let labels: Vec<String> = vec!["role=worker".into(), "team=infra".into()];
    let result = detect_metadata_with("claude", &labels, env_from(&[]));

    assert_eq!(result.get("agent"), Some(&Value::String("claude".into())));
    assert_eq!(result.get("role"), Some(&Value::String("worker".into())));
    assert_eq!(result.get("team"), Some(&Value::String("infra".into())));
}

#[test]
fn detect_metadata_dot_notation_nesting() {
    let labels: Vec<String> = vec!["gastown.role=worker".into()];
    let result = detect_metadata_with("claude", &labels, env_from(&[]));

    let gastown = result.get("gastown").expect("expected gastown key");
    assert_eq!(gastown.get("role"), Some(&Value::String("worker".into())));
}

#[test]
fn detect_metadata_deep_nesting() {
    let labels: Vec<String> = vec!["a.b.c=v".into()];
    let result = detect_metadata_with("claude", &labels, env_from(&[]));

    let a = result.get("a").expect("expected a key");
    let b = a.get("b").expect("expected b key");
    assert_eq!(b.get("c"), Some(&Value::String("v".into())));
}

#[test]
fn detect_metadata_labels_and_k8s_coexist() {
    let labels: Vec<String> = vec!["role=worker".into()];
    let result = detect_metadata_with(
        "claude",
        &labels,
        env_from(&[("KUBERNETES_SERVICE_HOST", "10.0.0.1"), ("POD_NAME", "my-pod")]),
    );

    assert_eq!(result.get("agent"), Some(&Value::String("claude".into())));
    assert_eq!(result.get("role"), Some(&Value::String("worker".into())));
    let k8s = result.get("k8s").expect("expected k8s key");
    assert_eq!(k8s.get("pod"), Some(&Value::String("my-pod".into())));
}

#[test]
fn detect_metadata_malformed_label_skipped() {
    let labels: Vec<String> = vec!["no-equals-sign".into(), "valid=yes".into()];
    let result = detect_metadata_with("claude", &labels, env_from(&[]));

    // Malformed label is skipped, valid one is present.
    assert_eq!(result.get("no-equals-sign"), None);
    assert_eq!(result.get("valid"), Some(&Value::String("yes".into())));
}

#[test]
fn detect_metadata_empty_value_is_valid() {
    let labels: Vec<String> = vec!["key=".into()];
    let result = detect_metadata_with("claude", &labels, env_from(&[]));

    assert_eq!(result.get("key"), Some(&Value::String(String::new())));
}

#[test]
fn detect_metadata_surfaces_boat_role() {
    let result = detect_metadata_with(
        "claude",
        &[],
        env_from(&[
            ("BOAT_PROJECT", "monorepo"),
            ("BOAT_ROLE", "fsm-triage"),
            ("BOAT_AGENT_BEAD_ID", "ka-abc123"),
        ]),
    );

    assert_eq!(result.get("project"), Some(&Value::String("monorepo".into())));
    assert_eq!(result.get("role"), Some(&Value::String("fsm-triage".into())));
    assert_eq!(result.get("agent_bead_id"), Some(&Value::String("ka-abc123".into())));
}

#[test]
fn detect_metadata_skips_role_when_env_empty() {
    // Empty BOAT_ROLE shouldn't produce a `role` key — coopmux's account
    // lookup would otherwise treat "" as a role name and look up a non-
    // existent role bead, adding latency on every register.
    let result = detect_metadata_with(
        "claude",
        &[],
        env_from(&[("BOAT_PROJECT", "monorepo"), ("BOAT_ROLE", "")]),
    );

    assert_eq!(result.get("project"), Some(&Value::String("monorepo".into())));
    assert_eq!(result.get("role"), None);
}

#[test]
fn detect_metadata_surfaces_expected_model_from_anthropic_model() {
    // The controller-baked ANTHROPIC_MODEL is surfaced as `expected_model` so a
    // downstream consumer can flag a served-vs-expected divergence.
    let result = detect_metadata_with(
        "claude",
        &[],
        env_from(&[("ANTHROPIC_MODEL", "claude-opus-5-oauth")]),
    );

    assert_eq!(result.get("expected_model"), Some(&Value::String("claude-opus-5-oauth".into())));
}

#[test]
fn detect_metadata_expected_model_prefers_anthropic_over_claude() {
    // ANTHROPIC_MODEL is Claude Code's primary selector; when both are set it
    // wins over the CLAUDE_MODEL mirror.
    let result = detect_metadata_with(
        "claude",
        &[],
        env_from(&[
            ("ANTHROPIC_MODEL", "claude-opus-5-oauth"),
            ("CLAUDE_MODEL", "claude-opus-4-8-oauth"),
        ]),
    );

    assert_eq!(result.get("expected_model"), Some(&Value::String("claude-opus-5-oauth".into())));
}

#[test]
fn detect_metadata_expected_model_falls_back_to_claude_model() {
    // With ANTHROPIC_MODEL unset, CLAUDE_MODEL supplies the expected model.
    let result =
        detect_metadata_with("claude", &[], env_from(&[("CLAUDE_MODEL", "claude-opus-4-8-oauth")]));

    assert_eq!(result.get("expected_model"), Some(&Value::String("claude-opus-4-8-oauth".into())));
}

#[test]
fn detect_metadata_skips_expected_model_when_unset_or_empty() {
    // No model env -> no key. An explicitly-empty ANTHROPIC_MODEL is treated as
    // unset and must not shadow a populated CLAUDE_MODEL nor emit an empty key.
    let none = detect_metadata_with("claude", &[], env_from(&[]));
    assert_eq!(none.get("expected_model"), None);

    let empty_anthropic = detect_metadata_with(
        "claude",
        &[],
        env_from(&[("ANTHROPIC_MODEL", ""), ("CLAUDE_MODEL", "claude-opus-4-8-oauth")]),
    );
    assert_eq!(
        empty_anthropic.get("expected_model"),
        Some(&Value::String("claude-opus-4-8-oauth".into()))
    );

    let both_empty = detect_metadata_with(
        "claude",
        &[],
        env_from(&[("ANTHROPIC_MODEL", ""), ("CLAUDE_MODEL", "")]),
    );
    assert_eq!(both_empty.get("expected_model"), None);
}
