// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;

fn env(pairs: &[(&str, &str)]) -> Vec<(String, String)> {
    pairs.iter().map(|(k, v)| ((*k).to_owned(), (*v).to_owned())).collect()
}

fn node_options(env: &[(String, String)]) -> Vec<&str> {
    env.iter().filter(|(k, _)| k == "NODE_OPTIONS").map(|(_, v)| v.as_str()).collect()
}

#[test]
fn the_node_heap_is_capped_below_the_pod_limit_by_default() {
    std::env::remove_var("NODE_OPTIONS");
    let child = child_env(&env(&[("A", "1")]));
    assert_eq!(node_options(&child), ["--max-old-space-size=1024"]);
    assert!(child.contains(&("A".to_owned(), "1".to_owned())));
}

#[test]
fn an_explicit_heap_setting_wins() {
    std::env::remove_var("NODE_OPTIONS");
    let child = child_env(&env(&[("NODE_OPTIONS", "--max-old-space-size=1536")]));
    assert_eq!(node_options(&child), ["--max-old-space-size=1536"]);
}

#[test]
fn other_node_options_are_kept_alongside_the_cap() {
    std::env::remove_var("NODE_OPTIONS");
    let child = child_env(&env(&[("NODE_OPTIONS", "--trace-warnings")]));
    assert_eq!(node_options(&child), ["--trace-warnings --max-old-space-size=1024"]);
}

#[test]
fn a_session_record_can_only_exist_after_ready_or_a_turn_ends() {
    let with = |method: &str| Reaction {
        synthetic: vec![serde_json::json!({ "method": method, "params": {} })],
        ..Reaction::default()
    };
    assert!(session_may_exist(&with("coop/ready")));
    assert!(session_may_exist(&with("coop/turnCompleted")));
    assert!(!session_may_exist(&with("coop/turnStarted")));
    assert!(!session_may_exist(&Reaction::default()));
}
