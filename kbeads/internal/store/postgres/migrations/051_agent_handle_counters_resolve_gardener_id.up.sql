-- Phase 4 of the captain -> gardener rename (gd-3AHtqh5XXB, plan gd-5O9QUlA8zr):
-- collapse the captain_id / gardener_id pair on agent_handle_counters (045 + 050)
-- down to ONE column, gardener_id, as the primary key. The only reader/writer of
-- this table is queryAllocateAgentHandleSeq inside the kbeads daemon, so the
-- schema change and the Go change ship together as one kbeads release.
--
-- Legacy synthetic ids are re-keyed (`synthetic-captain:<src>` ->
-- `synthetic-gardener:<src>`) because the controller now stamps the new prefix;
-- without this the counter for e.g. `unknown` would restart at 1 and hand out
-- `unknown-1..N` handles that already exist. Where both a legacy and a re-keyed
-- row exist the larger next_seq wins, so a sequence never rewinds. The table
-- holds one row per gardener, so this is not a bead-scale backfill.
--
-- The first statement in the branch takes ACCESS EXCLUSIVE on the table so an
-- old pod's INSERT cannot commit between the dedupe DELETE and ADD PRIMARY KEY
-- (it would duplicate gardener_id and leave the migration dirty); the lock
-- is held to the end of the transaction.
--
-- Idempotent: guarded on captain_id still existing, and runs in one transaction.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'agent_handle_counters'
          AND column_name = 'captain_id'
    ) THEN
        LOCK TABLE agent_handle_counters IN ACCESS EXCLUSIVE MODE;

        UPDATE agent_handle_counters
           SET gardener_id = regexp_replace(
                   COALESCE(gardener_id, captain_id),
                   '^synthetic-captain:', 'synthetic-gardener:');

        UPDATE agent_handle_counters c
           SET next_seq = m.max_seq
          FROM (SELECT gardener_id, MAX(next_seq) AS max_seq
                  FROM agent_handle_counters
                 GROUP BY gardener_id
                HAVING COUNT(*) > 1) m
         WHERE c.gardener_id = m.gardener_id;

        DELETE FROM agent_handle_counters a
         USING agent_handle_counters b
         WHERE a.gardener_id = b.gardener_id AND a.ctid < b.ctid;

        -- Dropping captain_id also drops the primary key that lived on it.
        ALTER TABLE agent_handle_counters DROP COLUMN captain_id;
        ALTER TABLE agent_handle_counters ALTER COLUMN gardener_id SET NOT NULL;
        ALTER TABLE agent_handle_counters ADD PRIMARY KEY (gardener_id);
    END IF;
END
$$;
