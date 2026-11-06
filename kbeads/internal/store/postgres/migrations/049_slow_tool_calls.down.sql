-- Reverse 049_slow_tool_calls.up.sql. Dropping the table forfeits the ledger
-- (indexes drop with it) — acceptable only on a full rollback, since this
-- plan's record plane has no other consumer depending on the rows persisting.
DROP TABLE IF EXISTS slow_tool_calls;
