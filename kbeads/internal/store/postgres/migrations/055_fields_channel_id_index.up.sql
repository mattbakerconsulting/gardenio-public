-- Perf (gd-349dQSDqoU): index the Commons channel lookup.
--
-- Every Commons read of a channel's messages filters on
-- `fields ? 'channel_id' AND fields->>'channel_id' = $1`: the channel list's
-- last entry per channel (internal/commons/inbox.go readLast), the unread
-- counts per channel (unread.go channelUnread) and the channel timeline
-- (model.go readTimeline). Without an index each lookup is a sequential scan
-- of beads. Measured 2026-10-21 on 2026.294.11: 0.39-0.55 s per lookup on a
-- quiet daemon (0.009-0.012 s for the same shape on the indexed thread_id),
-- one per channel (39 in land gardenio), so GET /garden/c and
-- GET /garden/c/unread took 5-8 s and opening a channel about 2.5 s.
--
-- Partial B-tree expression index, same shape and rationale as 031
-- (schedule_id), 034 (trigger_id), 037 (thread_id) and 052 (spawned_by):
-- single-key equality on a sparse JSON key (about 1.5k of 24k beads carry it).
-- Idempotent DDL only, not CONCURRENTLY (matches 037).

CREATE INDEX IF NOT EXISTS idx_beads_fields_channel_id
    ON beads ((fields->>'channel_id'))
    WHERE fields ? 'channel_id';

ANALYZE beads;
