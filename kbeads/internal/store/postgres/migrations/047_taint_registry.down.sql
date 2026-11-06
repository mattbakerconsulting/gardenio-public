-- Reverse 047_taint_registry.up.sql. Drops all taint data — safe only
-- because phase 1 lands no propagation or read-gate code that depends on
-- these tables existing.
DROP TABLE IF EXISTS taint_audit;
DROP TABLE IF EXISTS taint_grants;
DROP INDEX IF EXISTS idx_bead_taints_taint;
DROP TABLE IF EXISTS bead_taints;
DROP TABLE IF EXISTS taints;
