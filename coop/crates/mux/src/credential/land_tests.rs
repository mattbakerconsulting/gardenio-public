// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::collections::HashSet;

use super::*;

fn known(names: &[&str]) -> HashSet<String> {
    names.iter().map(|n| (*n).to_owned()).collect()
}

#[test]
fn land_of_account_mirrors_purse_land_of_account() {
    assert_eq!(land_of_account("lands-acme"), Some("acme"));
    assert_eq!(land_of_account("lands-a-b"), Some("a-b"));
    assert_eq!(land_of_account("gardenio"), None);
    assert_eq!(land_of_account("acme-lands-x"), None);
    assert_eq!(land_account_name("acme"), "lands-acme");
    // The degenerate name is still walled off from the fleet.
    assert_eq!(land_of_account("lands-"), Some(""));
    assert!(!land_wall_allows(None, "lands-"));
}

#[test]
fn wall_fails_closed_both_ways() {
    assert!(land_wall_allows(None, "fleet-a"));
    assert!(!land_wall_allows(None, "lands-x"));
    assert!(land_wall_allows(Some("x"), "lands-x"));
    assert!(!land_wall_allows(Some("x"), "lands-y"));
    assert!(!land_wall_allows(Some("x"), "fleet-a"));
}

#[test]
fn session_land_from_account_or_known_project() {
    let lands = known(&["lands-x"]);
    let proj_x = serde_json::json!({ "project": "x" });
    let proj_y = serde_json::json!({ "project": "y" });

    // (a) the assigned/booted account decides, whatever the project.
    assert_eq!(session_land(&proj_y, Some("lands-z"), &lands).as_deref(), Some("z"));
    // (b) the project is a land only when its account is known.
    assert_eq!(session_land(&proj_x, None, &lands).as_deref(), Some("x"));
    assert_eq!(session_land(&proj_x, Some("fleet-a"), &lands).as_deref(), Some("x"));
    assert_eq!(session_land(&proj_y, Some("fleet-a"), &lands), None);
    assert_eq!(session_land(&serde_json::Value::Null, Some("fleet-a"), &lands), None);
    assert_eq!(
        session_land(&serde_json::json!({ "project": "" }), None, &known(&["lands-"])),
        None
    );
}

#[test]
fn land_binding_decides_before_account_or_project() {
    // A land principal's session (gd-zVTBvaSIdG) is of its land even when
    // coopmux does not know the land's account yet, and whatever account its
    // coop reports or project it names.
    let bound = serde_json::json!({ "project": "x", "land": "x" });
    assert_eq!(session_land(&bound, None, &known(&[])).as_deref(), Some("x"));
    assert_eq!(session_land(&bound, Some("fleet-a"), &known(&[])).as_deref(), Some("x"));
    assert_eq!(session_land(&bound, Some("lands-y"), &known(&["lands-y"])).as_deref(), Some("x"));
    let lying = serde_json::json!({ "project": "gardenio", "land": "x" });
    assert_eq!(session_land(&lying, Some("fleet-a"), &known(&[])).as_deref(), Some("x"));
    assert!(!land_wall_allows(Some("x"), "fleet-a"));
}
