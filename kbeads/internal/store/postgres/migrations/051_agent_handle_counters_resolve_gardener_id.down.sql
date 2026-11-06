-- Reverse 051: restore captain_id as the primary key, gardener_id nullable again.
-- The synthetic-captain -> synthetic-gardener re-key is not undone (the counter
-- values stay correct under the new prefix).
ALTER TABLE agent_handle_counters DROP CONSTRAINT IF EXISTS agent_handle_counters_pkey;
ALTER TABLE agent_handle_counters ALTER COLUMN gardener_id DROP NOT NULL;
ALTER TABLE agent_handle_counters ADD COLUMN IF NOT EXISTS captain_id TEXT;
UPDATE agent_handle_counters SET captain_id = gardener_id WHERE captain_id IS NULL;
ALTER TABLE agent_handle_counters ALTER COLUMN captain_id SET NOT NULL;
ALTER TABLE agent_handle_counters ADD PRIMARY KEY (captain_id);
