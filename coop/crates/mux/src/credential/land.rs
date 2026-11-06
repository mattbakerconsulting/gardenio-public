// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! The land wall: a land's purse account is never shared with the fleet.
//!
//! The Lands provisioner creates one account per tenant land, `lands-<land>`,
//! billed to that land's own LiteLLM key (its purse). coopmux must hold that
//! account so the land's sessions can use it, but no fleet session may ever be
//! moved onto it, staged with it, or pushed its profile — and a land session
//! may hold only its own account. Every selection and distribution path asks
//! [`land_wall_allows`] before pairing a session with an account.
//!
//! Membership is by name only, the same convention as the controller's
//! `purse.AccountName` / `purse.LandOfAccount`
//! (gardenio/controller/internal/lands/purse/purse.go). There is no label.

use std::collections::HashSet;

use crate::state::MuxState;

/// Name prefix of every land account (`purse.AccountName`).
pub const LAND_ACCOUNT_PREFIX: &str = "lands-";

/// The land an account bills, or `None` for a fleet account. Mirrors
/// `purse.LandOfAccount`, except that the degenerate name `lands-` is still a
/// land account (of the empty land) so no fleet session can be handed it.
pub fn land_of_account(account: &str) -> Option<&str> {
    account.strip_prefix(LAND_ACCOUNT_PREFIX)
}

/// The account a land bills (`purse.AccountName`).
pub fn land_account_name(land: &str) -> String {
    format!("{LAND_ACCOUNT_PREFIX}{land}")
}

/// Whether a session of `session_land` (`None` = a fleet session) may hold or
/// receive `account`. Fails closed both ways: a fleet session never sees a land
/// account, and a land session sees only its own.
pub fn land_wall_allows(session_land: Option<&str>, account: &str) -> bool {
    land_of_account(account) == session_land
}

/// The land a session belongs to, or `None` for a fleet session.
///
/// A session is of land `X` when a land principal registered it (its metadata
/// carries the binding `land: X`, see `transport::land_principal`), when its
/// assigned or booted `account` is `lands-X`, or when its metadata `project`
/// is `X` and `lands-X` is one of the `known_land_accounts` (see
/// [`known_land_accounts`]). The binding comes first: the account is what the
/// session's own coop reports, so a bound session cannot report its way into
/// another land or the fleet, and it stays a land session before coopmux
/// knows its land's account.
pub fn session_land(
    metadata: &serde_json::Value,
    account: Option<&str>,
    known_land_accounts: &HashSet<String>,
) -> Option<String> {
    if let Some(land) = metadata
        .get(crate::transport::land_principal::LAND_BINDING_KEY)
        .and_then(|l| l.as_str())
        .filter(|l| !l.is_empty())
    {
        return Some(land.to_owned());
    }
    if let Some(land) = account.and_then(land_of_account) {
        return Some(land.to_owned());
    }
    let project = metadata.get("project").and_then(|p| p.as_str()).filter(|p| !p.is_empty())?;
    known_land_accounts.contains(&land_account_name(project)).then(|| project.to_owned())
}

/// Every land account the credential broker or the account cache knows.
pub async fn known_land_accounts(state: &MuxState) -> HashSet<String> {
    let mut known: HashSet<String> = state
        .credential_broker
        .status_list()
        .await
        .into_iter()
        .map(|a| a.name)
        .filter(|name| land_of_account(name).is_some())
        .collect();
    let cache = state.account_cache.read().await;
    known.extend(cache.all_accounts().keys().filter(|n| land_of_account(n).is_some()).cloned());
    known
}

/// Fleet accounts a project-less rebalance may move a session onto: the
/// bead-usable accounts, or every broker account when there is no account
/// cache yet, minus every land account.
pub async fn fleet_rebalance_candidates(state: &MuxState) -> HashSet<String> {
    let usable = state.account_cache.read().await.usable_accounts();
    let base = if usable.is_empty() {
        // No account cache yet (beads client unavailable): every broker account
        // is a candidate, preserving the broker-only selection.
        state.credential_broker.status_list().await.into_iter().map(|a| a.name).collect()
    } else {
        usable
    };
    base.into_iter().filter(|name| land_of_account(name).is_none()).collect()
}

#[cfg(test)]
#[path = "land_tests.rs"]
mod tests;
