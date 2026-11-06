// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;

// ── stamp_request_id ──────────────────────────────────────────────────

#[test]
fn stamp_inserts_request_id_when_absent() -> anyhow::Result<()> {
    let mut counter = 0;
    let msg = r#"{"event":"replay:get","offset":0}"#;
    let stamped = stamp_request_id(&mut counter, msg);

    assert_eq!(stamped.request_id, "1");
    let parsed: serde_json::Value = serde_json::from_str(&stamped.text)?;
    assert_eq!(parsed["request_id"], "1");
    assert_eq!(parsed["event"], "replay:get");
    Ok(())
}

#[test]
fn stamp_replaces_existing_request_id() -> anyhow::Result<()> {
    let mut counter = 0;
    let msg = r#"{"event":"agent:get","request_id":"r5"}"#;
    let stamped = stamp_request_id(&mut counter, msg);

    assert_eq!(stamped.request_id, "1");
    let parsed: serde_json::Value = serde_json::from_str(&stamped.text)?;
    assert_eq!(parsed["request_id"], "1");
    // Original client request_id is replaced, not duplicated.
    let obj = parsed.as_object().ok_or_else(|| anyhow::anyhow!("expected JSON object"))?;
    assert_eq!(obj.iter().filter(|(k, _)| k.as_str() == "request_id").count(), 1);
    Ok(())
}

#[test]
fn stamp_increments_counter() -> anyhow::Result<()> {
    let mut counter = 0;
    let _ = stamp_request_id(&mut counter, r#"{"event":"a"}"#);
    let second = stamp_request_id(&mut counter, r#"{"event":"b"}"#);
    assert_eq!(second.request_id, "2");
    Ok(())
}

// ── restore_request_id ────────────────────────────────────────────────

#[test]
fn restore_removes_bridge_request_id_when_client_omitted_one() -> anyhow::Result<()> {
    let json = r#"{"event":"replay","data":"abc","request_id":"1"}"#;
    let restored = restore_request_id(json, None);
    let parsed: serde_json::Value = serde_json::from_str(&restored)?;
    assert!(parsed.get("request_id").is_none());
    assert_eq!(parsed["event"], "replay");
    assert_eq!(parsed["data"], "abc");
    Ok(())
}

#[test]
fn restore_replaces_bridge_request_id_with_client_request_id() -> anyhow::Result<()> {
    let json = r#"{"event":"agent","request_id":"17"}"#;
    let restored = restore_request_id(json, Some("client-r5"));
    let parsed: serde_json::Value = serde_json::from_str(&restored)?;
    assert_eq!(parsed["request_id"], "client-r5");
    Ok(())
}

// ── extract_route_info ────────────────────────────────────────────────

#[test]
fn route_info_extracts_both_fields() {
    let json = r#"{"event":"replay","request_id":"42"}"#;
    let info = extract_route_info(json);
    assert_eq!(info.event, Some("replay"));
    assert_eq!(info.request_id, Some("42"));
}

#[test]
fn route_info_handles_missing_fields() {
    let json = r#"{"event":"pty"}"#;
    let info = extract_route_info(json);
    assert_eq!(info.event, Some("pty"));
    assert!(info.request_id.is_none());
}

#[test]
fn route_info_handles_invalid_json() {
    let info = extract_route_info("not json");
    assert!(info.event.is_none());
    assert!(info.request_id.is_none());
}

// ── stamp + strip round-trip ──────────────────────────────────────────

#[test]
fn stamp_then_strip_round_trips() -> anyhow::Result<()> {
    let mut counter = 0;
    let original = r#"{"event":"replay:get","offset":0}"#;

    let stamped = stamp_request_id(&mut counter, original);
    // Verify stamp added request_id
    let with_rid: serde_json::Value = serde_json::from_str(&stamped.text)?;
    assert!(with_rid.get("request_id").is_some());

    // Simulate upstream response (same JSON with request_id preserved)
    let response = r#"{"event":"replay","data":"abc","request_id":"1"}"#;
    let restored = restore_request_id(response, None);
    let parsed: serde_json::Value = serde_json::from_str(&restored)?;
    assert!(parsed.get("request_id").is_none());
    assert_eq!(parsed["event"], "replay");
    assert_eq!(parsed["data"], "abc");
    Ok(())
}

// ── client_had_rid detection ──────────────────────────────────────────

#[test]
fn detect_fire_and_forget_message() {
    // Client sends replay:get without request_id (fire-and-forget)
    let msg = r#"{"event":"replay:get","offset":0}"#;
    let had_rid = extract_route_info(msg).request_id.is_some();
    assert!(!had_rid, "fire-and-forget should not have request_id");
}

#[test]
fn detect_rpc_message() {
    // Client sends agent:get with request_id (RPC)
    let msg = r#"{"event":"agent:get","request_id":"r1"}"#;
    let had_rid = extract_route_info(msg).request_id.is_some();
    assert!(had_rid, "RPC message should have request_id");
}

#[test]
fn authentication_does_not_create_pending_request() {
    assert!(!expects_response(Some("auth")));
    assert!(expects_response(Some("agent:get")));
    assert!(expects_response(None));
}

// ── pending request bounds and expiry ────────────────────────────────

fn pending_request(client_id: ClientId, deadline: Instant) -> PendingRequest {
    PendingRequest { client_id, client_request_id: Some(format!("client-{client_id}")), deadline }
}

#[test]
fn pending_requests_enforce_per_client_limit() {
    let mut pending = PendingRequests::default();
    let deadline = Instant::now() + PENDING_REQUEST_TIMEOUT;
    for index in 0..MAX_PENDING_PER_CLIENT {
        assert!(pending.try_insert(index.to_string(), pending_request(7, deadline)).is_ok());
    }
    assert_eq!(
        pending.try_insert("overflow".to_owned(), pending_request(7, deadline)),
        Err(PendingInsertError::ClientFull)
    );
    assert_eq!(pending.count_for_client(7), MAX_PENDING_PER_CLIENT);
}

#[test]
fn pending_requests_enforce_bridge_limit() {
    let mut pending = PendingRequests::default();
    let deadline = Instant::now() + PENDING_REQUEST_TIMEOUT;
    for index in 0..MAX_PENDING_REQUESTS {
        let client_id = (index / MAX_PENDING_PER_CLIENT) as ClientId;
        assert!(pending
            .try_insert(index.to_string(), pending_request(client_id, deadline))
            .is_ok());
    }
    assert_eq!(
        pending.try_insert(
            "overflow".to_owned(),
            pending_request(MAX_PENDING_REQUESTS as ClientId, deadline)
        ),
        Err(PendingInsertError::BridgeFull)
    );
}

#[test]
fn pending_requests_expire_and_release_client_capacity() {
    let mut pending = PendingRequests::default();
    let now = Instant::now();
    assert!(pending.try_insert("expired".to_owned(), pending_request(1, now)).is_ok());
    assert!(pending
        .try_insert("live".to_owned(), pending_request(1, now + PENDING_REQUEST_TIMEOUT))
        .is_ok());

    let expired = pending.expire(now);
    assert_eq!(expired.len(), 1);
    assert_eq!(pending.requests.len(), 1);
    assert_eq!(pending.count_for_client(1), 1);
}

#[test]
fn pending_requests_drop_disconnected_clients_without_replay_state() {
    let mut pending = PendingRequests::default();
    let deadline = Instant::now() + PENDING_REQUEST_TIMEOUT;
    assert!(pending.try_insert("gone".to_owned(), pending_request(1, deadline)).is_ok());
    assert!(pending.try_insert("active".to_owned(), pending_request(2, deadline)).is_ok());

    let active = HashSet::from([2]);
    let removed = pending.remove_disconnected(&active);
    assert_eq!(removed.len(), 1);
    assert_eq!(pending.requests.len(), 1);
    assert_eq!(pending.count_for_client(1), 0);
    assert_eq!(pending.count_for_client(2), 1);
}

#[test]
fn pending_error_restores_client_request_id() -> anyhow::Result<()> {
    let text =
        pending_error_message(Some("client-r7"), "upstream_timeout", "upstream request timed out");
    let parsed: serde_json::Value = serde_json::from_str(&text)?;
    assert_eq!(parsed["event"], "error");
    assert_eq!(parsed["code"], "upstream_timeout");
    assert_eq!(parsed["request_id"], "client-r7");
    Ok(())
}

// ── subscription flags ────────────────────────────────────────────────

#[test]
fn subscription_flags_replay_requires_pty() {
    let flags = SubscriptionFlags { pty: false, screen: false, state: false };
    assert!(!flags.matches_event(Some("replay")));

    let flags = SubscriptionFlags { pty: true, screen: false, state: false };
    assert!(flags.matches_event(Some("replay")));
}

// ── bounded channel backpressure ───────────────────────────────────

#[test]
fn client_channel_rejects_when_full() {
    let (tx, _rx) = mpsc::channel::<Arc<str>>(CLIENT_CHANNEL_CAPACITY);
    for i in 0..CLIENT_CHANNEL_CAPACITY {
        let msg: Arc<str> = Arc::from(format!("msg-{i}"));
        assert!(tx.try_send(msg).is_ok(), "should accept up to capacity");
    }
    let overflow: Arc<str> = Arc::from("overflow");
    assert!(tx.try_send(overflow).is_err(), "should reject when full");
}

#[test]
fn upstream_channel_rejects_when_full() {
    let (tx, _rx) = mpsc::channel::<(ClientId, String)>(UPSTREAM_CHANNEL_CAPACITY);
    for i in 0..UPSTREAM_CHANNEL_CAPACITY {
        assert!(tx.try_send((1, format!("msg-{i}"))).is_ok());
    }
    assert!(tx.try_send((1, "overflow".into())).is_err());
}

#[test]
fn drop_threshold_is_less_than_channel_capacity() {
    // Sanity: the threshold for disconnecting a slow client must be reachable
    // within the channel capacity (otherwise a client would never be evicted).
    assert!(CLIENT_DROP_THRESHOLD < CLIENT_CHANNEL_CAPACITY as u32);
}

// ── build_ws_url ──────────────────────────────────────────────────────

#[test]
fn build_ws_url_http_to_ws() {
    let url = build_ws_url("http://localhost:3000", None, "pty,state");
    assert_eq!(url, "ws://localhost:3000/ws?subscribe=pty,state");
}

#[test]
fn build_ws_url_https_to_wss() {
    let url = build_ws_url("https://example.com", Some("tok"), "pty");
    assert_eq!(url, "wss://example.com/ws?subscribe=pty&token=tok");
}
