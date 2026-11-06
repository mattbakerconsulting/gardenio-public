-- Reverse 018_urgency_score.up.sql.

DROP TRIGGER IF EXISTS trg_deps_urgency_after_change ON deps;
DROP TRIGGER IF EXISTS trg_beads_urgency_after_status ON beads;
DROP TRIGGER IF EXISTS trg_beads_urgency_before_insup ON beads;

DROP FUNCTION IF EXISTS deps_urgency_after_change();
DROP FUNCTION IF EXISTS beads_urgency_after_status();
DROP FUNCTION IF EXISTS beads_urgency_before_insup();
DROP FUNCTION IF EXISTS refresh_all_urgency_scores();
DROP FUNCTION IF EXISTS refresh_urgency_score(TEXT);
DROP FUNCTION IF EXISTS count_downstream_blocked(TEXT);
DROP FUNCTION IF EXISTS compute_urgency_score(INTEGER, DOUBLE PRECISION, INTEGER);

ALTER TABLE beads DROP COLUMN IF EXISTS urgency_score;
