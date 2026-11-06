-- Push device registry, per person (Lands 4.7b, gd-a7U4AgwgCA).
--
-- One row per app install: the APNs device token is the key, and user_email is
-- the signed-in human who registered it (the OIDC subject of the token that
-- called POST /v1/devices, never a body field). The push bridge resolves an
-- alert's addressed humans to their rows here, so a device receives only the
-- alerts addressed to its owner. Before this table the bridge asked for every
-- device and alerted all of them.
--
-- A token that re-registers under another person moves to that person: the
-- token names an install, and the latest sign-in on it owns it.
--
-- Migration policy (kbeads/CLAUDE.md): idempotent DDL only, no backfill. There
-- is nothing to carry over: the daemon never served /v1/devices before this.
CREATE TABLE IF NOT EXISTS push_devices (
    token        TEXT PRIMARY KEY,
    platform     TEXT NOT NULL,
    bundle_id    TEXT NOT NULL DEFAULT '',
    user_email   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_push_devices_user_email
    ON push_devices (user_email);
