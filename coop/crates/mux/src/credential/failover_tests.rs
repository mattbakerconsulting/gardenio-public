// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;

fn make_cache_with_chain() -> AccountCache {
    let mut cache = AccountCache::new();

    // Chain: acme-team → gardenio → (end)
    cache.accounts.insert(
        "acme-team".to_owned(),
        AccountInfo {
            name: "acme-team".to_owned(),
            bead_id: String::new(),
            fallback: Some("gardenio".to_owned()),
            provider: "claude".to_owned(),
            status: "active".to_owned(),
            credential_secret: "secret-acme".to_owned(),
            ..Default::default()
        },
    );
    cache.accounts.insert(
        "gardenio".to_owned(),
        AccountInfo {
            name: "gardenio".to_owned(),
            bead_id: String::new(),
            fallback: None,
            provider: "claude".to_owned(),
            status: "active".to_owned(),
            credential_secret: "secret-gardenio".to_owned(),
            ..Default::default()
        },
    );

    // Chain: matthewbaker1110 → gardenio → backup-acct → (end)
    // (gardenio already exists, just add the others)
    cache.accounts.insert(
        "matthewbaker1110".to_owned(),
        AccountInfo {
            name: "matthewbaker1110".to_owned(),
            bead_id: String::new(),
            fallback: Some("gardenio".to_owned()),
            provider: "claude".to_owned(),
            status: "active".to_owned(),
            credential_secret: "secret-matt".to_owned(),
            ..Default::default()
        },
    );
    // Update gardenio to chain to backup-acct for the second chain.
    // Actually, a single account can only have one fallback, so gardenio → backup-acct.
    if let Some(a) = cache.accounts.get_mut("gardenio") {
        a.fallback = Some("backup-acct".to_owned());
    }
    cache.accounts.insert(
        "backup-acct".to_owned(),
        AccountInfo {
            name: "backup-acct".to_owned(),
            bead_id: String::new(),
            fallback: None,
            provider: "openai".to_owned(),
            status: "active".to_owned(),
            credential_secret: "secret-litellm".to_owned(),
            ..Default::default()
        },
    );

    cache
}

#[test]
fn walk_fallback_chain_single_hop() -> anyhow::Result<()> {
    let mut cache = AccountCache::new();
    cache.accounts.insert(
        "acme-team".to_owned(),
        AccountInfo {
            name: "acme-team".to_owned(),
            bead_id: String::new(),
            fallback: Some("gardenio".to_owned()),
            provider: "claude".to_owned(),
            status: "active".to_owned(),
            credential_secret: "s1".to_owned(),
            ..Default::default()
        },
    );
    cache.accounts.insert(
        "gardenio".to_owned(),
        AccountInfo {
            name: "gardenio".to_owned(),
            bead_id: String::new(),
            fallback: None,
            provider: "claude".to_owned(),
            status: "active".to_owned(),
            credential_secret: "s2".to_owned(),
            ..Default::default()
        },
    );

    let chain = cache.walk_fallback_chain("acme-team");
    assert_eq!(chain, vec!["gardenio"]);
    Ok(())
}

#[test]
fn walk_fallback_chain_multi_hop() -> anyhow::Result<()> {
    let cache = make_cache_with_chain();
    let chain = cache.walk_fallback_chain("matthewbaker1110");
    assert_eq!(chain, vec!["gardenio", "backup-acct"]);
    Ok(())
}

#[test]
fn walk_fallback_chain_no_fallback() -> anyhow::Result<()> {
    let cache = make_cache_with_chain();
    let chain = cache.walk_fallback_chain("backup-acct");
    assert!(chain.is_empty());
    Ok(())
}

#[test]
fn walk_fallback_chain_unknown_account() -> anyhow::Result<()> {
    let cache = make_cache_with_chain();
    let chain = cache.walk_fallback_chain("nonexistent");
    assert!(chain.is_empty());
    Ok(())
}

#[test]
fn walk_fallback_chain_detects_cycle() -> anyhow::Result<()> {
    let mut cache = AccountCache::new();

    // a → b → c → a (cycle)
    cache.accounts.insert(
        "a".to_owned(),
        AccountInfo {
            name: "a".to_owned(),
            bead_id: String::new(),
            fallback: Some("b".to_owned()),
            provider: "claude".to_owned(),
            status: "active".to_owned(),
            credential_secret: "sa".to_owned(),
            ..Default::default()
        },
    );
    cache.accounts.insert(
        "b".to_owned(),
        AccountInfo {
            name: "b".to_owned(),
            bead_id: String::new(),
            fallback: Some("c".to_owned()),
            provider: "claude".to_owned(),
            status: "active".to_owned(),
            credential_secret: "sb".to_owned(),
            ..Default::default()
        },
    );
    cache.accounts.insert(
        "c".to_owned(),
        AccountInfo {
            name: "c".to_owned(),
            bead_id: String::new(),
            fallback: Some("a".to_owned()),
            provider: "claude".to_owned(),
            status: "active".to_owned(),
            credential_secret: "sc".to_owned(),
            ..Default::default()
        },
    );

    let chain = cache.walk_fallback_chain("a");
    // Should get b, c but stop before revisiting a.
    assert_eq!(chain, vec!["b", "c"]);
    Ok(())
}

#[test]
fn projects_using_account_finds_matches() -> anyhow::Result<()> {
    let mut cache = make_cache_with_chain();

    cache.auth_configs.insert(
        "monorepo".to_owned(),
        AuthConfigInfo {
            bead_id: "bead-1".to_owned(),
            account: "acme-team".to_owned(),
            primary_account: Some("acme-team".to_owned()),
            project: "monorepo".to_owned(),
        },
    );
    cache.auth_configs.insert(
        "gardenio".to_owned(),
        AuthConfigInfo {
            bead_id: "bead-2".to_owned(),
            account: "acme-team".to_owned(),
            primary_account: Some("matthewbaker1110".to_owned()),
            project: "gardenio".to_owned(),
        },
    );
    cache.auth_configs.insert(
        "other".to_owned(),
        AuthConfigInfo {
            bead_id: "bead-3".to_owned(),
            account: "gardenio".to_owned(),
            primary_account: None,
            project: "other".to_owned(),
        },
    );

    let mut projects = cache.projects_using_account("acme-team");
    projects.sort();
    assert_eq!(projects, vec!["gardenio", "monorepo"]);

    let projects = cache.projects_using_account("gardenio");
    assert_eq!(projects, vec!["other"]);

    let projects = cache.projects_using_account("nonexistent");
    assert!(projects.is_empty());

    Ok(())
}

/// Build a cache whose accounts carry `project:<name>` labels so the
/// authorized set is computed from the project pool, not a fallback chain.
/// Pool for "monorepo" = {acct-a, acct-b, acct-c}; "solo" = {acct-solo};
/// acct-other is in no relevant pool.
fn make_cache_with_pool() -> AccountCache {
    let mut cache = AccountCache::new();
    let p = |s: &str| vec![format!("project:{s}")];
    for (name, labels) in [
        ("acct-a", p("monorepo")),
        ("acct-b", p("monorepo")),
        ("acct-c", p("monorepo")),
        ("acct-solo", p("solo")),
        ("acct-other", vec!["project:unrelated".to_owned()]),
    ] {
        cache.accounts.insert(
            name.to_owned(),
            AccountInfo {
                name: name.to_owned(),
                provider: "claude".to_owned(),
                status: "active".to_owned(),
                labels,
                ..Default::default()
            },
        );
    }
    cache
}

#[test]
fn authorized_accounts_built_from_project_pool() -> anyhow::Result<()> {
    let cache = make_cache_with_pool();

    // The authorized set is the full project:monorepo pool — every account
    // carrying the label, NOT a single fallback chain. This is what keeps
    // spread alive (ka-0000001040 item 1).
    let authorized = cache.authorized_accounts_for_project("monorepo");
    assert_eq!(authorized.len(), 3, "multi-account project must expose >1 authorized account");
    assert!(authorized.len() > 1);
    assert!(authorized.contains("acct-a"));
    assert!(authorized.contains("acct-b"));
    assert!(authorized.contains("acct-c"));
    // Accounts labeled for other projects are not in the pool.
    assert!(!authorized.contains("acct-solo"));
    assert!(!authorized.contains("acct-other"));

    Ok(())
}

#[test]
fn authorized_accounts_ignores_fallback_chain_and_config_account() -> anyhow::Result<()> {
    // Even when an auth config exists and points at one specific account,
    // and that account has NO fallback, the authorized set must still be
    // the whole labeled pool — not collapse to the single config account
    // (ka-0000001040 items 1 + 3).
    let mut cache = make_cache_with_pool();
    cache.auth_configs.insert(
        "monorepo".to_owned(),
        AuthConfigInfo {
            bead_id: "bead-auth".to_owned(),
            account: "acct-a".to_owned(), // configured (possibly stale) account
            primary_account: None,
            project: "monorepo".to_owned(),
        },
    );

    let authorized = cache.authorized_accounts_for_project("monorepo");
    assert_eq!(authorized.len(), 3);
    assert!(authorized.len() > 1);

    Ok(())
}

#[test]
fn authorized_accounts_single_account_pool() -> anyhow::Result<()> {
    let cache = make_cache_with_pool();
    let authorized = cache.authorized_accounts_for_project("solo");
    assert_eq!(authorized.len(), 1);
    assert!(authorized.contains("acct-solo"));
    Ok(())
}

#[test]
fn authorized_accounts_unknown_project_returns_empty() -> anyhow::Result<()> {
    let cache = make_cache_with_pool();
    let authorized = cache.authorized_accounts_for_project("nonexistent");
    assert!(authorized.is_empty());
    Ok(())
}

#[test]
fn authorized_accounts_empty_project_returns_empty() -> anyhow::Result<()> {
    let cache = make_cache_with_pool();
    assert!(cache.authorized_accounts_for_project("").is_empty());
    Ok(())
}

#[tokio::test]
async fn authorized_accounts_from_refreshed_pool_is_multi() -> anyhow::Result<()> {
    // End-to-end via refresh(): account beads carrying project:gardenio
    // labels produce an authorized set of size > 1 (the E2E invariant).
    let stub = StubBeadsSource {
        accounts: vec![
            make_account_bead_labeled("acct-1", None, vec!["project:gardenio".to_owned()]),
            make_account_bead_labeled("acct-2", None, vec!["project:gardenio".to_owned()]),
            make_account_bead_labeled("acct-3", None, vec!["project:other".to_owned()]),
        ],
        auth_configs: vec![],
        role_accounts: vec![],
    };
    let mut cache = AccountCache::new();
    cache.refresh(&stub).await?;

    let authorized = cache.authorized_accounts_for_project("gardenio");
    assert!(authorized.len() > 1, "multi-account project pool must yield >1 authorized");
    assert_eq!(authorized.len(), 2);
    assert!(authorized.contains("acct-1"));
    assert!(authorized.contains("acct-2"));

    Ok(())
}

#[test]
fn account_for_project_returns_configured_account() -> anyhow::Result<()> {
    let mut cache = AccountCache::new();

    cache.auth_configs.insert(
        "monorepo".to_owned(),
        AuthConfigInfo {
            bead_id: "bead-1".to_owned(),
            account: "matthewbaker-acme".to_owned(),
            primary_account: None,
            project: "monorepo".to_owned(),
        },
    );
    cache.auth_configs.insert(
        "gardenio".to_owned(),
        AuthConfigInfo {
            bead_id: "bead-2".to_owned(),
            account: "matthewbaker1110".to_owned(),
            primary_account: None,
            project: "gardenio".to_owned(),
        },
    );

    // Each project should get its configured account.
    assert_eq!(cache.account_for_project("monorepo"), Some("matthewbaker-acme".to_owned()));
    assert_eq!(cache.account_for_project("gardenio"), Some("matthewbaker1110".to_owned()));

    // Unknown project returns None (falls back to pool LB).
    assert_eq!(cache.account_for_project("unknown-project"), None);

    Ok(())
}

struct StubBeadsSource {
    accounts: Vec<AccountBead>,
    auth_configs: Vec<AuthConfig>,
    role_accounts: Vec<RoleAccount>,
}

impl BeadsSource for StubBeadsSource {
    async fn list_account_beads(&self) -> anyhow::Result<Vec<AccountBead>> {
        Ok(self.accounts.clone())
    }

    async fn list_auth_configs(&self) -> anyhow::Result<Vec<AuthConfig>> {
        Ok(self.auth_configs.clone())
    }

    async fn list_role_accounts(&self) -> anyhow::Result<Vec<RoleAccount>> {
        Ok(self.role_accounts.clone())
    }
}

fn make_account_bead(name: &str, fallback: Option<&str>) -> AccountBead {
    make_account_bead_labeled(name, fallback, vec![])
}

fn make_account_bead_labeled(
    name: &str,
    fallback: Option<&str>,
    labels: Vec<String>,
) -> AccountBead {
    AccountBead {
        name: name.to_owned(),
        id: format!("bead-{name}"),
        fallback: fallback.map(str::to_owned),
        provider: "claude".to_owned(),
        status: "active".to_owned(),
        credential_secret: format!("secret-{name}"),
        throttle_status: String::new(),
        utilization_5h: String::new(),
        utilization_7d: String::new(),
        throttle_reset_at: String::new(),
        representative_claim: String::new(),
        billing_period_hours: String::new(),
        labels,
    }
}

#[tokio::test]
async fn refresh_rejects_self_loop_fallback() -> anyhow::Result<()> {
    // Account whose fallback points at itself — must be cleared on load.
    let stub = StubBeadsSource {
        accounts: vec![
            make_account_bead("gardenio3", Some("gardenio3")),
            make_account_bead("gardenio", None),
        ],
        auth_configs: vec![],
        role_accounts: vec![],
    };

    let mut cache = AccountCache::new();
    cache.refresh(&stub).await?;

    let info = cache.accounts.get("gardenio3").expect("gardenio3 in cache");
    assert_eq!(info.fallback, None, "self-loop fallback must be cleared");

    // Walking the chain must produce no warning and no entries.
    let chain = cache.walk_fallback_chain("gardenio3");
    assert!(chain.is_empty());

    Ok(())
}

#[tokio::test]
async fn refresh_preserves_valid_fallback() -> anyhow::Result<()> {
    let stub = StubBeadsSource {
        accounts: vec![
            make_account_bead("gardenio3", Some("matthewbaker-acme")),
            make_account_bead("matthewbaker-acme", None),
        ],
        auth_configs: vec![],
        role_accounts: vec![],
    };

    let mut cache = AccountCache::new();
    cache.refresh(&stub).await?;

    let info = cache.accounts.get("gardenio3").expect("gardenio3 in cache");
    assert_eq!(info.fallback.as_deref(), Some("matthewbaker-acme"));

    Ok(())
}

#[tokio::test]
async fn refresh_loads_role_accounts() -> anyhow::Result<()> {
    let stub = StubBeadsSource {
        accounts: vec![make_account_bead("litellm-bedrock", None)],
        auth_configs: vec![],
        role_accounts: vec![RoleAccount {
            role: "fsm-triage".to_owned(),
            account: "litellm-bedrock".to_owned(),
        }],
    };

    let mut cache = AccountCache::new();
    cache.refresh(&stub).await?;

    assert_eq!(cache.account_for_role("fsm-triage"), Some("litellm-bedrock".to_owned()));
    assert_eq!(cache.account_for_role("unknown-role"), None);

    Ok(())
}

#[tokio::test]
async fn fetch_builds_standalone_cache_without_shared_lock() -> anyhow::Result<()> {
    // The refresh loop must build the fresh cache OFF-LOCK, then swap it in
    // under a brief write lock — never hold the account-cache write guard
    // across the three beads RPCs (ka-0000000143). `fetch` is the off-lock
    // constructor: it takes only `&client` (no `&mut self`, no lock) and
    // returns a fully-populated cache.
    let stub = StubBeadsSource {
        accounts: vec![make_account_bead("gardenio3", Some("matthewbaker-acme"))],
        auth_configs: vec![AuthConfig {
            bead_id: "cfg-1".to_owned(),
            account: "matthewbaker1110".to_owned(),
            primary_account: None,
            project: "gardenio".to_owned(),
        }],
        role_accounts: vec![RoleAccount {
            role: "fsm-triage".to_owned(),
            account: "litellm-bedrock".to_owned(),
        }],
    };

    let cache = AccountCache::fetch(&stub).await?;

    assert!(cache.accounts.contains_key("gardenio3"), "accounts populated");
    assert_eq!(cache.account_for_project("gardenio"), Some("matthewbaker1110".to_owned()));
    assert_eq!(cache.account_for_role("fsm-triage"), Some("litellm-bedrock".to_owned()));

    Ok(())
}

#[tokio::test]
async fn refresh_swaps_out_stale_state() -> anyhow::Result<()> {
    // `refresh` is now a build-then-swap over `fetch`: any entry present
    // before the refresh but absent from the new payload must be gone
    // afterwards (no stale carry-over from the in-place clears we removed).
    let mut cache = AccountCache::new();
    cache
        .refresh(&StubBeadsSource {
            accounts: vec![make_account_bead("old-account", None)],
            auth_configs: vec![AuthConfig {
                bead_id: "cfg-old".to_owned(),
                account: "old-account".to_owned(),
                primary_account: None,
                project: "old-project".to_owned(),
            }],
            role_accounts: vec![RoleAccount {
                role: "old-role".to_owned(),
                account: "old-account".to_owned(),
            }],
        })
        .await?;
    assert!(cache.accounts.contains_key("old-account"));

    cache
        .refresh(&StubBeadsSource {
            accounts: vec![make_account_bead("new-account", None)],
            auth_configs: vec![],
            role_accounts: vec![],
        })
        .await?;

    assert!(cache.accounts.contains_key("new-account"), "new payload applied");
    assert!(!cache.accounts.contains_key("old-account"), "stale account dropped");
    assert_eq!(cache.account_for_project("old-project"), None, "stale auth config dropped");
    assert_eq!(cache.account_for_role("old-role"), None, "stale role pin dropped");

    Ok(())
}

#[test]
fn account_for_role_walks_comma_separated_roles() {
    let mut cache = AccountCache::new();
    cache.role_accounts_mut().insert("thread".to_owned(), "litellm-bedrock".to_owned());

    // First component with a pin wins. Matches resolveRoleAccount on
    // the controller side.
    assert_eq!(cache.account_for_role("crew,thread"), Some("litellm-bedrock".to_owned()));
    assert_eq!(cache.account_for_role("thread,crew"), Some("litellm-bedrock".to_owned()));
    // No matching pin → None (caller falls back to project default).
    assert_eq!(cache.account_for_role("crew,gardener"), None);
    // Empty role string is a no-op.
    assert_eq!(cache.account_for_role(""), None);
}

#[tokio::test]
async fn refresh_tolerates_role_account_rpc_failure() -> anyhow::Result<()> {
    // The role-account refresh is best-effort. A daemon that doesn't
    // yet support `/v1/beads?type=role` (or that 500s) must NOT collapse
    // the whole refresh — sessions just fall through to project-default
    // assignment, which is the pre-ka-0000002136 behavior.
    struct PartialStub {
        accounts: Vec<AccountBead>,
    }
    impl BeadsSource for PartialStub {
        async fn list_account_beads(&self) -> anyhow::Result<Vec<AccountBead>> {
            Ok(self.accounts.clone())
        }
        async fn list_auth_configs(&self) -> anyhow::Result<Vec<AuthConfig>> {
            Ok(vec![])
        }
        async fn list_role_accounts(&self) -> anyhow::Result<Vec<RoleAccount>> {
            Err(anyhow::anyhow!("daemon doesn't support role endpoint"))
        }
    }

    let mut cache = AccountCache::new();
    cache.refresh(&PartialStub { accounts: vec![make_account_bead("a", None)] }).await?;
    assert!(cache.accounts.contains_key("a"));
    assert_eq!(cache.account_for_role("fsm-triage"), None);

    Ok(())
}

// ── account_usable: bead-authoritative health gate for staging ───────

fn cache_with_account(status: &str, throttle: &str) -> AccountCache {
    let mut cache = AccountCache::new();
    cache.accounts.insert(
        "acct".to_owned(),
        AccountInfo {
            name: "acct".to_owned(),
            status: status.to_owned(),
            throttle_status: throttle.to_owned(),
            provider: "claude".to_owned(),
            ..Default::default()
        },
    );
    cache
}

#[test]
fn account_usable_open_account_is_usable() {
    let cache = cache_with_account("open", "");
    assert!(cache.account_usable("acct"));
}

#[test]
fn account_usable_active_account_is_usable() {
    // A non-exhausted account is usable regardless of throttle_status —
    // even an empty (never-probed) one.
    let cache = cache_with_account("active", "");
    assert!(cache.account_usable("acct"));
}

#[test]
fn account_usable_disabled_is_never_usable() {
    // Operator-disabled (e.g. org turned off Claude Code subscription
    // access — the matthewbaker1110 case) must never be a rotation target.
    let cache = cache_with_account("disabled", "allowed");
    assert!(!cache.account_usable("acct"));
}

#[test]
fn account_usable_exhausted_rejected_is_unusable() {
    let cache = cache_with_account("exhausted", "rejected");
    assert!(!cache.account_usable("acct"));
}

#[test]
fn account_usable_exhausted_unprobed_is_unusable() {
    // exhausted + empty throttle_status: throttle_allows("") is false, so
    // an exhausted account with no recovery signal is NOT a safe target.
    let cache = cache_with_account("exhausted", "");
    assert!(!cache.account_usable("acct"));
}

#[test]
fn account_usable_exhausted_but_recovered_is_usable() {
    // 5h window reset → throttle_status flips back to "allowed" while the
    // bead status may still read exhausted until the controller sweeps it.
    // Such an account is usable again (matches controller fallbackTier 1).
    let cache = cache_with_account("exhausted", "allowed");
    assert!(cache.account_usable("acct"));
}

#[test]
fn account_usable_exhausted_allowed_warning_is_usable() {
    let cache = cache_with_account("exhausted", "allowed_warning");
    assert!(cache.account_usable("acct"));
}

#[test]
fn account_usable_unknown_account_is_not_usable() {
    // No bead record → no health signal → must not stage.
    let cache = AccountCache::new();
    assert!(!cache.account_usable("never-seen"));
}

#[test]
fn throttle_allows_vocabulary() {
    assert!(!throttle_allows(""));
    assert!(!throttle_allows("rejected"));
    assert!(throttle_allows("allowed"));
    assert!(throttle_allows("allowed_warning"));
}

#[test]
fn account_labeled_for_project_matches_exactly() {
    let labels = vec!["project:gardenio".to_owned(), "tier:max".to_owned()];
    assert!(account_labeled_for_project(&labels, "project:gardenio"));
    assert!(!account_labeled_for_project(&labels, "project:monorepo"));
    // Prefix-only must NOT match (no substring matching).
    assert!(!account_labeled_for_project(&labels, "project:gas"));
    assert!(!account_labeled_for_project(&[], "project:gardenio"));
}

// ── cooldown_until_from: exhaustion-writer horizon (item 4) ──────────

#[test]
fn cooldown_until_from_uses_billing_period_hours() {
    // 1-hour billing window → cooldown ~1h out, well before the 5h default.
    let now = now_unix();
    let cooldown = cooldown_until_from("1");
    let default = cooldown_until_from("");
    // Both are strings; compare as ISO-8601 (lexicographically ordered for
    // same-format UTC timestamps).
    assert!(cooldown < default, "1h window must be sooner than the 5h default");
    // Sanity: the timestamp is in the future relative to now.
    assert!(cooldown.as_str() > iso8601_at(now).as_str());
}

#[test]
fn cooldown_until_from_defaults_when_unset_or_invalid() {
    // Empty, zero, and non-numeric all fall back to the 5h default.
    let expected = cooldown_until_from("");
    // Compare horizons by recomputing within the same second window is
    // flaky; instead assert each variant equals the default-window length
    // by checking they parse to the same form (all default => identical
    // unless a second tick intervenes, so just assert non-empty + future).
    for v in ["", "0", "abc", "-3", "  "] {
        let got = cooldown_until_from(v);
        assert!(!got.is_empty());
        assert!(got.ends_with('Z'));
        // Default-window variants should match `expected` modulo a possible
        // 1-second tick; assert they share the date prefix at minimum.
        assert_eq!(&got[..13], &expected[..13], "variant {v:?} should use default window");
    }
}

// ── usable_accounts: bead-authoritative usable set (ka-0000006526) ────

fn account_info(name: &str, status: &str, throttle: &str, fallback: Option<&str>) -> AccountInfo {
    AccountInfo {
        name: name.to_owned(),
        status: status.to_owned(),
        throttle_status: throttle.to_owned(),
        fallback: fallback.map(|s| s.to_owned()),
        provider: "claude".to_owned(),
        ..Default::default()
    }
}

#[test]
fn usable_accounts_filters_exhausted_and_disabled() {
    let mut cache = AccountCache::new();
    cache.accounts.insert("healthy".to_owned(), account_info("healthy", "active", "allowed", None));
    // The fimbaz shape: exhausted + rejected on its bead.
    cache
        .accounts
        .insert("fimbaz".to_owned(), account_info("fimbaz", "exhausted", "rejected", None));
    cache
        .accounts
        .insert("disabled".to_owned(), account_info("disabled", "disabled", "allowed", None));
    // Recovered: bead still exhausted but throttle flipped back to allowed.
    cache
        .accounts
        .insert("recovered".to_owned(), account_info("recovered", "exhausted", "allowed", None));

    let usable = cache.usable_accounts();
    assert!(usable.contains("healthy"));
    assert!(usable.contains("recovered"));
    assert!(!usable.contains("fimbaz"), "exhausted+rejected must be excluded");
    assert!(!usable.contains("disabled"), "operator-disabled must be excluded");
    assert_eq!(usable.len(), 2);
}

#[test]
fn usable_accounts_empty_cache_is_empty() {
    let cache = AccountCache::new();
    assert!(cache.usable_accounts().is_empty());
}

// ── find_fallback_for_project: switch-target bead-health gate ─────────
//
// Regression for ka-0000006526: coopmux's project-scoped rebalance selected
// `fimbaz` — an account the controller had already marked status=exhausted /
// throttle_status=rejected on its bead — because the broker's in-process
// AccountStatus still read Healthy (this coopmux had not locally observed a 429
// on it yet). The selector must consult the bead-authoritative account_usable
// rubric so a bead-condemned account is never handed to a live session.

fn broker_all_healthy(names: &[&str]) -> Arc<crate::credential::broker::CredentialBroker> {
    use crate::credential::{AccountConfig, CredentialConfig};
    let configs: Vec<AccountConfig> = names
        .iter()
        .map(|name| AccountConfig {
            name: (*name).to_owned(),
            provider: "claude".to_owned(),
            env_key: None,
            model: None,
            token_url: None,
            client_id: None,
            auth_url: None,
            device_auth_url: None,
            reauth: true,
        })
        .collect();
    let (tx, _rx) = tokio::sync::broadcast::channel(16);
    crate::credential::broker::CredentialBroker::new(
        CredentialConfig { accounts: configs },
        tx,
        None,
    )
}

#[tokio::test]
async fn find_fallback_skips_bead_exhausted_even_when_broker_healthy() -> anyhow::Result<()> {
    // Chain for project daily-bundle: current=shannon → fimbaz → gardenio2.
    // fimbaz is broker-Healthy but bead-exhausted+rejected; gardenio2 is fully
    // healthy. The selector must skip fimbaz and land on gardenio2.
    let mut cache = AccountCache::new();
    cache.accounts.insert(
        "shannon".to_owned(),
        account_info("shannon", "exhausted", "rejected", Some("fimbaz")),
    );
    cache.accounts.insert(
        "fimbaz".to_owned(),
        account_info("fimbaz", "exhausted", "rejected", Some("gardenio2")),
    );
    cache
        .accounts
        .insert("gardenio2".to_owned(), account_info("gardenio2", "active", "allowed", None));
    cache.auth_configs.insert(
        "daily-bundle".to_owned(),
        AuthConfigInfo {
            bead_id: "bead-db".to_owned(),
            account: "shannon".to_owned(),
            primary_account: None,
            project: "daily-bundle".to_owned(),
        },
    );

    // Broker reads ALL of them Healthy (fresh process, no local 429s observed) —
    // the exact post-restart state that let the bug fire.
    let broker = broker_all_healthy(&["shannon", "fimbaz", "gardenio2"]);
    broker.set_token("shannon", "k".into(), None, None).await?;
    broker.set_token("fimbaz", "k".into(), None, None).await?;
    broker.set_token("gardenio2", "k".into(), None, None).await?;

    let got = cache.find_fallback_for_project("daily-bundle", &broker).await;
    assert_eq!(got, Some("gardenio2".to_owned()), "must skip bead-exhausted fimbaz");
    Ok(())
}

#[tokio::test]
async fn find_fallback_returns_none_when_all_bead_condemned() -> anyhow::Result<()> {
    // Every fallback is bead-exhausted+rejected → no safe target, even though
    // the broker reads them Healthy. Better to strand than to 429-loop.
    let mut cache = AccountCache::new();
    cache.accounts.insert("a".to_owned(), account_info("a", "active", "allowed", Some("b")));
    cache.accounts.insert("b".to_owned(), account_info("b", "exhausted", "rejected", Some("c")));
    cache.accounts.insert("c".to_owned(), account_info("c", "exhausted", "rejected", None));
    cache.auth_configs.insert(
        "proj".to_owned(),
        AuthConfigInfo {
            bead_id: "bead-proj".to_owned(),
            account: "a".to_owned(),
            primary_account: None,
            project: "proj".to_owned(),
        },
    );

    let broker = broker_all_healthy(&["a", "b", "c"]);
    broker.set_token("b", "k".into(), None, None).await?;
    broker.set_token("c", "k".into(), None, None).await?;

    let got = cache.find_fallback_for_project("proj", &broker).await;
    assert_eq!(got, None, "all fallbacks bead-condemned → no target");
    Ok(())
}

#[tokio::test]
async fn find_fallback_still_skips_broker_throttled_bead_healthy() -> anyhow::Result<()> {
    // The original gate must still hold: a bead-healthy account this coopmux
    // has locally marked Throttled (saw a 429) is skipped in favor of a
    // fully-healthy successor.
    let mut cache = AccountCache::new();
    cache.accounts.insert("cur".to_owned(), account_info("cur", "active", "allowed", Some("t")));
    cache.accounts.insert("t".to_owned(), account_info("t", "active", "allowed", Some("ok")));
    cache.accounts.insert("ok".to_owned(), account_info("ok", "active", "allowed", None));
    cache.auth_configs.insert(
        "proj".to_owned(),
        AuthConfigInfo {
            bead_id: "bead-proj".to_owned(),
            account: "cur".to_owned(),
            primary_account: None,
            project: "proj".to_owned(),
        },
    );

    let broker = broker_all_healthy(&["cur", "t", "ok"]);
    broker.set_token("t", "k".into(), None, None).await?;
    broker.set_token("ok", "k".into(), None, None).await?;
    // Locally observed 429 on `t` → broker Throttled.
    broker.mark_throttled("t").await;

    let got = cache.find_fallback_for_project("proj", &broker).await;
    assert_eq!(got, Some("ok".to_owned()), "broker-throttled account still skipped");
    Ok(())
}

// ── Land wall: a land's purse account has no fallback and is never one ──
//
// gd-bD6BNm16oA: even if someone sets `fallback` on a `lands-x` bead, or points
// a fleet account's `fallback` at one, failover ignores it.

fn auth_config(project: &str, account: &str) -> AuthConfigInfo {
    AuthConfigInfo {
        bead_id: format!("bead-{project}"),
        account: account.to_owned(),
        primary_account: None,
        project: project.to_owned(),
    }
}

#[tokio::test]
async fn fallback_on_a_land_account_is_ignored() -> anyhow::Result<()> {
    let mut cache = AccountCache::new();
    cache
        .accounts
        .insert("lands-x".to_owned(), account_info("lands-x", "active", "allowed", Some("fleet")));
    cache.accounts.insert("fleet".to_owned(), account_info("fleet", "active", "allowed", None));
    cache.auth_configs.insert("x".to_owned(), auth_config("x", "lands-x"));

    let broker = broker_all_healthy(&["lands-x", "fleet"]);
    broker.set_token("fleet", "k".into(), None, None).await?;

    assert!(cache.walk_fallback_chain("lands-x").is_empty());
    assert_eq!(cache.find_fallback_for_project("x", &broker).await, None);
    Ok(())
}

#[tokio::test]
async fn land_project_has_no_fallback_even_on_a_fleet_account() -> anyhow::Result<()> {
    // Project x's config:auth names a fleet account, but `lands-x` exists: it
    // is a land, and a land never fails over onto the fleet.
    let mut cache = AccountCache::new();
    cache.accounts.insert("lands-x".to_owned(), account_info("lands-x", "active", "allowed", None));
    cache.accounts.insert("a".to_owned(), account_info("a", "active", "allowed", Some("b")));
    cache.accounts.insert("b".to_owned(), account_info("b", "active", "allowed", None));
    cache.auth_configs.insert("x".to_owned(), auth_config("x", "a"));

    let broker = broker_all_healthy(&["lands-x", "a", "b"]);
    broker.set_token("b", "k".into(), None, None).await?;

    assert_eq!(cache.find_fallback_for_project("x", &broker).await, None);
    Ok(())
}

#[tokio::test]
async fn fleet_chain_stops_before_a_land_account() -> anyhow::Result<()> {
    // a → lands-x → b: the chain must not hand out lands-x, nor follow its
    // fallback through to b.
    let mut cache = AccountCache::new();
    cache.accounts.insert("a".to_owned(), account_info("a", "active", "allowed", Some("lands-x")));
    cache
        .accounts
        .insert("lands-x".to_owned(), account_info("lands-x", "active", "allowed", Some("b")));
    cache.accounts.insert("b".to_owned(), account_info("b", "active", "allowed", None));
    cache.auth_configs.insert("proj".to_owned(), auth_config("proj", "a"));

    let broker = broker_all_healthy(&["a", "lands-x", "b"]);
    broker.set_token("lands-x", "k".into(), None, None).await?;
    broker.set_token("b", "k".into(), None, None).await?;

    assert!(cache.walk_fallback_chain("a").is_empty());
    assert_eq!(cache.find_fallback_for_project("proj", &broker).await, None);
    Ok(())
}
