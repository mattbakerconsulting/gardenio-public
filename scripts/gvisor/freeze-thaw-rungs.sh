#!/usr/bin/env bash
# freeze-thaw-rungs.sh — codify the gVisor freeze→thaw assertion ladder as 7
# NAMED rungs with stable FT_RUNG_* markers (Rig R1, bead ka-0000005639; epic
# ka-0000000867; canary ka-0000001355).
#
# WHAT THIS IS
# ------------
# A PURE, SOURCEABLE shell classifier. It reads the `FT_*` sentinel stream that
# the freeze + thaw node scripts ALREADY emit (gardenio/controller/internal/
# podmanager/freezethaw_node.go) plus three harness-observed post-thaw signals,
# and classifies one freeze→thaw cycle into 7 ordered rungs. For each rung it
# emits a stable marker:
#
#     FT_RUNG_<name>=ok|fail|skip
#     FT_RUNG_<name>_REASON=<one-line reason>
#
# and identifies the FIRST failing rung as the cycle's classified mode:
#
#     FT_FIRST_FAILED_RUNG=<name>|none
#     FT_FIRST_FAILED_REASON=<reason of that rung>
#
# It is OFF the live freeze/thaw path — it only PARSES sentinels, never drives
# runsc, never touches the controller, the freezeThaw flag, or prod. The HARD
# HOLD on canary ka-0000001355 (one canary, zero orphans, prod untouched) is
# respected by construction: sourcing this file runs no side effects.
#
# WHO CONSUMES IT
# ---------------
# The canary driver (R4) concatenates the freeze node stdout, the thaw node
# stdout, and its own post-thaw observations into ONE stream and feeds it here.
#   - Rig R2 (ka-0000006948) captures the full FT_RUNG_* set into a gb bundle on
#     the first failing rung.
#   - Rig R3 (ka-0000004124, scripts/gvisor/file-next-layer-bug.sh) files a
#     deduped next-layer bug keyed on (FT_FIRST_FAILED_RUNG + reason signature).
# This file is the single source of FT_FIRST_FAILED_RUNG and the per-rung markers
# both rigs key on.
#
# THE 7 RUNGS (ordered — the task's canonical order)
# --------------------------------------------------
#   1. snapshot_persisted  freeze produced a structurally-thawable snapshot
#   2. restore_rc0         the count-gated barrier restore returned a live sentry
#   3. netns_routable      the restored sentry's netns carries eth0 + a default route
#   4. cni_confirmed       Pod B's CNI ADD committed a result (NON-GATING localizer
#                          for rung 3 — the WHY; never the classified failure mode)
#   5. rehydrate_acks      both gb and coop acked the thaw epoch (gb_ack + coop_ack)
#   6. work_intact         the /work file token survived the freeze→thaw
#   7. ram_intact          the in-RAM counter resumed climbing PAST its freeze value
#
# "First failing rung = classified mode": the ladder is walked 1→7; the first
# rung that definitively FAILS is the classified failure mode, and every rung
# below it is reported `skip` (the cycle never reached it). A rung whose evidence
# is merely ABSENT does NOT stop the ladder unless that rung's absent-policy is
# `fail` (see the per-rung table below) — it is reported `skip` and the walk
# continues.
#
# SENTINEL PROVENANCE
# -------------------
# Rungs 1–4 read sentinels the NODE SCRIPTS already emit (no node-side change):
#   FT_CHECKPOINT_RC FT_IMAGE_PRESENT FT_SANDBOX_STATE FT_BUNDLE_OK FT_IMAGE_OK
#   FT_SUB_RC FT_RESTORED_PID FT_RESTORED_NETNS FT_B_NETNS
#   FT_NETNS_IFACES FT_NETNS_DEFROUTE FT_CNI_CONFIRMED
#   (+ FT_CNI_POD_IP FT_NETNS_PINNED FT_NETNS_L3_REASSERTED FT_NETNS_SELF_IPAM
#      surfaced as rung-3 diagnostics, mirroring freezethaw.go's lo-only error.)
# Rungs 5–7 read sentinels the canary harness (R4) is responsible for emitting
# from its post-thaw observations — R1 DEFINES them as the contract. Values are
# matched CASE-SENSITIVELY against the exact positive token; any other value
# (including a casing/spelling variant like OK / INTACT / true) is treated as a
# failure, so the harness must emit the exact lowercase tokens shown:
#   FT_REHYDRATE_GB_ACK=ok|fail      gb consumer acked the thaw epoch
#   FT_REHYDRATE_COOP_ACK=ok|fail    coop consumer acked the thaw epoch
#   FT_WORK_TOKEN=intact|lost        the /work file token (state.bin) survived
#   FT_RAM_RESUMED=yes|no            the in-RAM counter climbed past the freeze value
#
# Absent-policy for these harness sentinels:
#   rung 6 work_intact / rung 7 ram_intact — each reads ONE sentinel; absent
#     => SKIP (the harness did not measure that signal).
#   rung 5 rehydrate_acks — reads TWO sentinels and is FAIL-CLOSED PER CONSUMER,
#     mirroring the controller's ack gate (freezethaw_rehydrate.go waitForAcks
#     requires EVERY required consumer to ack OK). So:
#       - BOTH ack sentinels absent  => SKIP (the harness did not exercise the
#         rehydration trigger at all — e.g. a standalone restore with no acks).
#       - exactly ONE present        => FAIL: a required consumer's ack was not
#         observed, which is the fail-closed gate's verdict, not a not-measured
#         skip. HARNESS CONTRACT: emit BOTH ack sentinels together, or NEITHER —
#         never exactly one. A lone ack is treated as a partial-measurement
#         failure precisely so an unbalanced emission cannot silently pass.
#
# THE FT_ROOT_RC OVERLOAD (why rung 2 keys on FT_RESTORED_PID, not FT_ROOT_RC)
# ---------------------------------------------------------------------------
# freezethaw_node.go emits FT_ROOT_RC TWICE on the lo-only path: first as the
# real barrier rc (0, line ~1409), then AGAIN as 128 (line ~1492) when the
# restored netns is lo-only — AFTER FT_RESTORED_PID has already been printed.
# So a last-wins parse sees FT_ROOT_RC=128 on a cycle whose barrier actually
# SUCCEEDED. Keying restore_rc0 (rung 2) on the presence of FT_RESTORED_PID
# cleanly separates "barrier failed" (no PID — the script exited before [6]) from
# "barrier ok, netns lo-only" (PID present, then FT_ROOT_RC=128), so the lo-only
# case classifies as netns_routable (rung 3), not restore_rc0 (rung 2). This is
# the one place the naive predicate misclassifies, and the reason rung 2 does not
# read FT_ROOT_RC at all.
#
# PER-RUNG ABSENT-POLICY
# ----------------------
#   rung 1 snapshot_persisted  primary FT_CHECKPOINT_RC absent => FAIL (freeze
#                              produced no sentinels — nothing was persisted)
#   rung 2 restore_rc0         all of FT_RESTORED_PID/FT_SUB_RC/FT_ROOT_RC absent
#                              => FAIL (thaw produced no sentinels). Only reached
#                              when rung 1 passed, so absence here is a real thaw
#                              failure, not a not-run freeze.
#   rung 3 netns_routable      FT_NETNS_IFACES absent => SKIP (legacy/older script
#                              that predates Fix A — controller treats it the same)
#   rung 4 cni_confirmed       NON-GATING: ok|skip only, never fail. Absent OR
#                              present-no => SKIP (best-effort diagnostic; the
#                              controller never asserts it — see the evaluator)
#   rung 5 rehydrate_acks      BOTH ack sentinels absent => SKIP; exactly ONE
#                              present => FAIL (fail-closed per consumer — emit
#                              both ack sentinels or neither; see the contract note)
#   rung 6 work_intact         FT_WORK_TOKEN absent => SKIP (harness didn't measure)
#   rung 7 ram_intact          FT_RAM_RESUMED absent => SKIP (harness didn't measure)
#
# USAGE
# -----
#   source scripts/gvisor/freeze-thaw-rungs.sh
#   ft_classify_rungs <sentinel-file>      # or: ... | ft_classify_rungs -
#   # prints the FT_RUNG_* marker block to stdout; returns 0 if no rung FAILED
#   # (FT_FIRST_FAILED_RUNG=none), 1 otherwise.
#
# The marker block is plain `KEY=value` lines — eval-safe, grep-able, and shaped
# exactly like the node-script sentinels R2/R3 already parse.

# Guard against side effects when sourced: define functions only.
# shellcheck shell=bash

# ----------------------------------------------------------------------------
# Rung names, in ladder order. STABLE CONTRACT — R2/R3 key on these. Do not
# rename without updating both rigs (they capture the full set generically, but
# the names are what a human reads in a filed bug / forensic bundle).
# ----------------------------------------------------------------------------
FT_RUNG_NAMES=(
  snapshot_persisted
  restore_rc0
  netns_routable
  cni_confirmed
  rehydrate_acks
  work_intact
  ram_intact
)

# ftr_reset clears the parse + verdict state. Called at the top of every
# ft_classify_rungs so repeated calls in one process (e.g. the test harness) do
# not leak state between cycles.
ftr_reset() {
  unset FTR_S        # sentinel KEY -> value (last-wins)
  unset FTR_SEEN     # sentinel KEY -> 1 if the key ever appeared
  declare -gA FTR_S=()
  declare -gA FTR_SEEN=()
}

# ftr_load reads KEY=value sentinel lines from a file ($1) or stdin ($1 == "-").
# Last value wins for a repeated key (matches the node scripts' own re-emit
# semantics, e.g. the FT_ROOT_RC overload). FTR_SEEN records mere PRESENCE so the
# classifier can distinguish "absent" from "present but empty".
ftr_load() {
  local src="${1:-/dev/stdin}"
  [ "$src" = "-" ] && src=/dev/stdin
  local line key val
  while IFS= read -r line || [ -n "$line" ]; do
    # Only KEY=value lines whose key looks like a sentinel (FT_*). Everything
    # else (log lines, blank lines, the node scripts' `log ...` stderr that may
    # have been merged in) is ignored.
    case "$line" in
      FT_*=*) ;;
      *) continue ;;
    esac
    key="${line%%=*}"
    val="${line#*=}"
    FTR_S["$key"]="$val"
    FTR_SEEN["$key"]=1
  done < "$src"
}

# ftr_get KEY -> prints the sentinel value (empty if absent).
ftr_get() { printf '%s' "${FTR_S[$1]-}"; }

# ftr_seen KEY -> returns 0 if the key appeared at all (even empty), else 1.
ftr_seen() { [ -n "${FTR_SEEN[$1]-}" ]; }

# Verdict + reason for the rung currently being evaluated. Each rung evaluator
# sets these two globals; the ladder reads them.
FTR_VERDICT=""
FTR_REASON=""

# ----------------------------------------------------------------------------
# Per-rung evaluators. Each sets FTR_VERDICT to ok|fail|skip and FTR_REASON to a
# one-line human reason. They assume ftr_load has populated FTR_S/FTR_SEEN.
# ----------------------------------------------------------------------------

# Rung 1 — snapshot_persisted: the freeze produced a structurally-thawable
# snapshot. Mirrors the SIX-conjunct freeze post-state assert block the
# controller fail-closes on before building Snapshot{} (freezethaw.go:1264-1311):
# checkpoint rc 0, image present after checkpoint, sandbox still running
# (--leave-running kept Pod A alive), container_count baked into the statefile,
# OCI bundle complete, and the 3.0G image safely relocated to the controller-owned
# path.
#
# FT_IMAGE_OK is emitted at BOTH freeze (relocation re-verify, node:596, strict)
# AND thaw (pre-restore re-verify, node:731, tolerant). The parse is last-wins
# (matching the node scripts' own re-emit semantics), so in a combined
# freeze+thaw stream the THAW-side value wins. That is DELIBERATE and correct
# here: the only case where the two differ is an image reaped between freeze and
# thaw (freeze=yes, thaw=no + FT_ROOT_RC=128), and reading the thaw `no` lets this
# rung classify the reaped-image durability fault as a snapshot_persisted failure
# — peeling it off BEFORE rung 2 ever interprets the overloaded FT_ROOT_RC=128
# (the synthesis "secondary hazard"). On a normal cycle both are `yes`, so
# last-wins is a no-op.
ftr_rung_snapshot_persisted() {
  if ! ftr_seen FT_CHECKPOINT_RC; then
    FTR_VERDICT=fail
    FTR_REASON="no freeze sentinels (FT_CHECKPOINT_RC absent) — freeze produced no parseable output; nothing was persisted"
    return
  fi
  local crc img sbst count bundle imgok
  crc="$(ftr_get FT_CHECKPOINT_RC)"
  img="$(ftr_get FT_IMAGE_PRESENT)"
  sbst="$(ftr_get FT_SANDBOX_STATE)"
  count="$(ftr_get FT_CONTAINER_COUNT)"
  bundle="$(ftr_get FT_BUNDLE_OK)"
  imgok="$(ftr_get FT_IMAGE_OK)"
  if [ "$crc" != "0" ]; then
    FTR_VERDICT=fail; FTR_REASON="runsc checkpoint rc=$crc (expected 0)"; return
  fi
  # FT_IMAGE_PRESENT disambiguation (bug ka-0000004931). The freeze node script
  # emits FT_IMAGE_PRESENT=yes|no on the line right after FT_CHECKPOINT_RC. Before
  # the freeze-side HMNTT/hard-deadline fix, a WEDGED host-ns image probe hung the
  # freeze exec until the harness killed it, TRUNCATING the sentinel stream between
  # FT_CHECKPOINT_RC and FT_IMAGE_PRESENT — so this rung saw FT_IMAGE_PRESENT
  # ABSENT (never emitted) or PRESENT-BUT-EMPTY, NOT a genuine "=no". Both still
  # fail (fail-closed is correct: no verified image), but the REASON must be
  # unambiguous so an auto-filed signature (Rig R3) does not label a hang+truncate
  # as "image absent". Use ftr_seen (present-vs-absent) + emptiness to split the
  # three cases: (a) key never seen => stream TRUNCATED after FT_CHECKPOINT_RC;
  # (b) key seen but value empty => same truncation shape, partial line; (c) key
  # seen with a real value != yes (e.g. "no") => a GENUINE post-checkpoint image
  # absence the probe positively observed.
  if ! ftr_seen FT_IMAGE_PRESENT; then
    FTR_VERDICT=fail
    FTR_REASON="freeze sentinel stream TRUNCATED after FT_CHECKPOINT_RC=$crc (FT_IMAGE_PRESENT absent) — the checkpoint ran but the image probe did not emit; a wedged/killed host-ns probe (pre-fix hang), NOT an observed image absence"
    return
  fi
  if [ -z "$img" ]; then
    FTR_VERDICT=fail
    FTR_REASON="freeze sentinel stream TRUNCATED at FT_IMAGE_PRESENT (present but empty) after FT_CHECKPOINT_RC=$crc — the image probe was cut off mid-line; a wedged/killed host-ns probe (pre-fix hang), NOT an observed image absence"
    return
  fi
  if [ "$img" != "yes" ]; then
    FTR_VERDICT=fail; FTR_REASON="checkpoint image absent after checkpoint (FT_IMAGE_PRESENT=$img) — the host-ns probe positively observed no image at the checkpoint path"; return
  fi
  if [ "$sbst" != "running" ]; then
    FTR_VERDICT=fail; FTR_REASON="sandbox not running after --leave-running checkpoint (FT_SANDBOX_STATE=$sbst) — the checkpoint killed the live pod"; return
  fi
  # container_count must be baked into the statefile and >= 1 (the barrier target;
  # freezethaw.go:1279-1282). A non-numeric/absent count is untrusted -> 0 -> fail.
  case "$count" in
    ''|*[!0-9]*) count=0 ;;
  esac
  if [ "$count" -lt 1 ]; then
    FTR_VERDICT=fail; FTR_REASON="container_count not baked into statefile (FT_CONTAINER_COUNT=$(ftr_get FT_CONTAINER_COUNT)) — roster/checkpoint shape unknown"; return
  fi
  if [ "$bundle" != "yes" ]; then
    FTR_VERDICT=fail; FTR_REASON="incomplete OCI bundle (FT_BUNDLE_OK=$bundle) — config.json/overlay-opts missing; snapshot un-thawable"; return
  fi
  if [ "$imgok" != "yes" ]; then
    FTR_VERDICT=fail; FTR_REASON="checkpoint image not safe at restore (FT_IMAGE_OK=$imgok) — image absent/empty/un-relocated or reaped between freeze and thaw"; return
  fi
  FTR_VERDICT=ok
  FTR_REASON="checkpoint rc=0, image present+relocated, sandbox running, container_count=$count, bundle complete"
}

# Rung 2 — restore_rc0: the count-gated barrier restore returned a live restored
# sentry. Keyed on FT_RESTORED_PID PRESENCE (not FT_ROOT_RC — see the overload
# note in the header) plus FT_SUB_RC==0 plus the netns INODE proof
# (FT_RESTORED_NETNS == FT_B_NETNS, freezethaw.go:2029). A missing PID means the
# barrier failed and the node script exited before [6]; cite FT_ROOT_RC/FT_SUB_RC.
ftr_rung_restore_rc0() {
  local pid subrc rootrc rns bns
  pid="$(ftr_get FT_RESTORED_PID)"
  subrc="$(ftr_get FT_SUB_RC)"
  rootrc="$(ftr_get FT_ROOT_RC)"
  rns="$(ftr_get FT_RESTORED_NETNS)"
  bns="$(ftr_get FT_B_NETNS)"
  if [ -z "$pid" ]; then
    if ! ftr_seen FT_RESTORED_PID && ! ftr_seen FT_SUB_RC && ! ftr_seen FT_ROOT_RC; then
      FTR_VERDICT=fail
      FTR_REASON="no thaw sentinels (FT_RESTORED_PID/FT_SUB_RC/FT_ROOT_RC absent) — thaw produced no parseable output"
      return
    fi
    FTR_VERDICT=fail
    FTR_REASON="barrier restore produced no restored sentry pid (FT_ROOT_RC=${rootrc:-?} FT_SUB_RC=${subrc:-?}) — the count-gated restore failed before [6]"
    return
  fi
  if [ "$subrc" != "0" ]; then
    FTR_VERDICT=fail
    FTR_REASON="sub-container restore rc=${subrc:-?} (root pid=$pid present but a sub-container restore failed at the barrier)"
    return
  fi
  # Inode proof: the restored sentry must share Pod B's kubelet-owned netns inode.
  if [ -z "$rns" ] || [ -z "$bns" ] || [ "$rns" != "$bns" ]; then
    FTR_VERDICT=fail
    FTR_REASON="restored netns inode ${rns:-<empty>} != Pod B netns ${bns:-<empty>} — sentry landed in the wrong netns"
    return
  fi
  FTR_VERDICT=ok
  FTR_REASON="restored sentry pid=$pid, FT_SUB_RC=0, netns inode matches Pod B ($rns)"
}

# Rung 3 — netns_routable: the restored sentry's OWN netns carries a real network
# (>=1 non-loopback iface AND a default route). This is Fix A (ka-0000007713):
# the inode proof in rung 2 only proves WHERE the sentry landed, not that the
# netns is routable. A lo-only restore passes rung 2 but fails here. The reason
# embeds the Fix-C/L3/pin diagnostics (mirroring freezethaw.go:2054) so the WHY
# (rung 4's concern) is not lost when this rung is the classified mode.
ftr_rung_netns_routable() {
  if ! ftr_seen FT_NETNS_IFACES; then
    FTR_VERDICT=skip
    FTR_REASON="FT_NETNS_IFACES not emitted (legacy/pre-Fix-A node script) — netns content not asserted"
    return
  fi
  local ifaces defroute diag
  ifaces="$(ftr_get FT_NETNS_IFACES)"
  defroute="$(ftr_get FT_NETNS_DEFROUTE)"
  diag="cni-confirmed=$(ftr_get FT_CNI_CONFIRMED) cni-pod-ip=$(ftr_get FT_CNI_POD_IP) pinned=$(ftr_get FT_NETNS_PINNED) l3-reasserted=$(ftr_get FT_NETNS_L3_REASSERTED) self-ipam=$(ftr_get FT_NETNS_SELF_IPAM)"
  # Numeric guard: a non-numeric iface count is treated as 0 (untrusted output).
  case "$ifaces" in
    ''|*[!0-9]*) ifaces=0 ;;
  esac
  if [ "$ifaces" -lt 1 ] || [ "$defroute" != "yes" ]; then
    FTR_VERDICT=fail
    FTR_REASON="lo-only netns (non-loopback ifaces=$ifaces, default-route=${defroute:-?}) — CNI eth0 absent/unrouted in the restore target; rehydrate-ack would fail network-unreachable [$diag]"
    return
  fi
  FTR_VERDICT=ok
  FTR_REASON="netns routable (non-loopback ifaces=$ifaces, default-route=yes) [$diag]"
}

# Rung 4 — cni_confirmed: did Pod B's CNI ADD commit an authoritative result
# before the netns pin (Fix C, ka-0000001099)? This is a NON-GATING LOCALIZER:
# verdict is ok or skip, NEVER fail.
#
# WHY NEVER FAIL (verified against source, freezethaw.go:1993,2055 + node:889-895):
# FT_CNI_CONFIRMED is best-effort and is NEVER asserted by the controller — its
# only Go uses are diagnostic format args. On a CNI miss the node script logs a
# WARN and pins+restores ANYWAY, relying on Fix-C self-IPAM to populate the netns
# another way; the netns's actual health is then proven by rung 3's DIRECT content
# read (FT_NETNS_IFACES/FT_NETNS_DEFROUTE), not by this sentinel. So a thaw can be
# genuinely routable with FT_CNI_CONFIRMED=no. Because the ladder orders
# netns_routable (3) BEFORE cni_confirmed (4), this rung is only ever REACHED when
# rung 3 already passed (the netns IS routable) — at which point `cni=no` means
# "routable via self-IPAM, CNI ADD was not confirmed", a healthy outcome, NOT a
# failure. Gating here would flag good thaws. The cni signal is also embedded in
# rung 3's reason so a lo-only failure keeps the localizer even when rung 4 is
# skipped as unreached.
ftr_rung_cni_confirmed() {
  if ! ftr_seen FT_CNI_CONFIRMED; then
    FTR_VERDICT=skip
    FTR_REASON="FT_CNI_CONFIRMED not emitted (legacy/pre-Fix-C node script) — CNI confirmation not observed"
    return
  fi
  local cni podip
  cni="$(ftr_get FT_CNI_CONFIRMED)"
  podip="$(ftr_get FT_CNI_POD_IP)"
  if [ "$cni" = "yes" ]; then
    FTR_VERDICT=ok
    FTR_REASON="CNI ADD confirmed (committed pod-ip=${podip:-<none>})"
    return
  fi
  # cni!=yes but rung 3 already proved the netns routable (this rung was reached) —
  # the placeholder is routable via Fix-C self-IPAM. Record as a non-failing skip
  # so it never becomes the classified mode of a healthy thaw.
  FTR_VERDICT=skip
  FTR_REASON="CNI ADD not confirmed (FT_CNI_CONFIRMED=$cni) but netns is routable (rung 3 passed) — provisioned via Fix-C self-IPAM (cni-pod-ip=${podip:-<empty>}); non-gating"
}

# Rung 5 — rehydrate_acks: both rehydration consumers acked the thaw epoch
# (gb_ack + coop_ack). The controller's fail-closed gate (freezethaw_rehydrate.go
# waitForAcks) releases only when EVERY required consumer acks OK; this rung
# mirrors that fail-closed-per-consumer semantics. The canary harness reports each
# consumer's outcome as FT_REHYDRATE_GB_ACK / FT_REHYDRATE_COOP_ACK.
#
# Absent handling is deliberately asymmetric vs rungs 6/7 (which each read one
# sentinel and skip on absence): only when BOTH ack sentinels are absent does the
# harness clearly not exercise rehydration at all -> SKIP. If exactly ONE is
# present, a required consumer's ack was NOT observed, which under the controller's
# fail-closed gate is a release-denied verdict, NOT a not-measured skip -> FAIL,
# naming the missing/failed consumer(s). The harness contract is therefore "emit
# both ack sentinels or neither"; a lone ack is treated as a partial-measurement
# failure so an unbalanced emission cannot silently pass a half-acked thaw.
ftr_rung_rehydrate_acks() {
  if ! ftr_seen FT_REHYDRATE_GB_ACK && ! ftr_seen FT_REHYDRATE_COOP_ACK; then
    FTR_VERDICT=skip
    FTR_REASON="no ack sentinels (FT_REHYDRATE_GB_ACK/FT_REHYDRATE_COOP_ACK) — harness did not exercise rehydration"
    return
  fi
  local gb coop
  gb="$(ftr_get FT_REHYDRATE_GB_ACK)"
  coop="$(ftr_get FT_REHYDRATE_COOP_ACK)"
  local failed=""
  [ "$gb" = "ok" ] || failed="gb(${gb:-<absent>})"
  if [ "$coop" != "ok" ]; then
    [ -n "$failed" ] && failed="$failed,"
    failed="${failed}coop(${coop:-<absent>})"
  fi
  if [ -n "$failed" ]; then
    FTR_VERDICT=fail
    FTR_REASON="rehydration consumer(s) did not ack OK for the thaw epoch: $failed"
    return
  fi
  FTR_VERDICT=ok
  FTR_REASON="gb and coop both acked the thaw epoch OK"
}

# Rung 6 — work_intact: the /work FILE token (the sentinel written to
# /work/state.bin pre-freeze) survived the freeze→thaw. Harness reports
# FT_WORK_TOKEN=intact|lost.
ftr_rung_work_intact() {
  if ! ftr_seen FT_WORK_TOKEN; then
    FTR_VERDICT=skip
    FTR_REASON="FT_WORK_TOKEN not emitted — harness did not measure the /work file token"
    return
  fi
  local tok
  tok="$(ftr_get FT_WORK_TOKEN)"
  if [ "$tok" != "intact" ]; then
    FTR_VERDICT=fail
    FTR_REASON="/work file token did not survive (FT_WORK_TOKEN=$tok) — disk state lost across freeze→thaw"
    return
  fi
  FTR_VERDICT=ok
  FTR_REASON="/work file token survived the freeze→thaw (FT_WORK_TOKEN=intact)"
}

# Rung 7 — ram_intact: the in-RAM monotonic counter resumed climbing PAST its
# freeze value, proving the restored process is the SAME live process with its
# memory (not a cold restart from zero). Harness reports FT_RAM_RESUMED=yes|no.
ftr_rung_ram_intact() {
  if ! ftr_seen FT_RAM_RESUMED; then
    FTR_VERDICT=skip
    FTR_REASON="FT_RAM_RESUMED not emitted — harness did not measure in-RAM resume"
    return
  fi
  local ram
  ram="$(ftr_get FT_RAM_RESUMED)"
  if [ "$ram" != "yes" ]; then
    FTR_VERDICT=fail
    FTR_REASON="in-RAM counter did not resume past freeze (FT_RAM_RESUMED=$ram) — restored process lost its memory / cold-restarted"
    return
  fi
  FTR_VERDICT=ok
  FTR_REASON="in-RAM counter resumed climbing past the freeze value (FT_RAM_RESUMED=yes)"
}

# ----------------------------------------------------------------------------
# ft_classify_rungs <sentinel-file|-> — the public entry point.
#
# Reads the sentinel stream, walks the 7 rungs in order, and prints the marker
# block. The FIRST rung whose verdict is `fail` is the classified mode; every
# rung after it is forced to `skip` (the cycle never reached it). Rungs before
# the first failure keep their own ok/skip verdict.
#
# Returns 0 when no rung failed (FT_FIRST_FAILED_RUNG=none), 1 otherwise.
# ----------------------------------------------------------------------------
ft_classify_rungs() {
  ftr_reset
  ftr_load "${1:-/dev/stdin}"

  local first_failed="none"
  local first_reason=""
  local name verdict reason

  for name in "${FT_RUNG_NAMES[@]}"; do
    if [ "$first_failed" != "none" ]; then
      # A prior rung already failed — everything downstream is unreached.
      verdict=skip
      reason="not reached — first failing rung was $first_failed"
    else
      FTR_VERDICT=""
      FTR_REASON=""
      "ftr_rung_${name}"
      verdict="$FTR_VERDICT"
      reason="$FTR_REASON"
      if [ "$verdict" = "fail" ]; then
        first_failed="$name"
        first_reason="$reason"
      fi
    fi
    printf 'FT_RUNG_%s=%s\n' "$name" "$verdict"
    printf 'FT_RUNG_%s_REASON=%s\n' "$name" "$reason"
  done

  printf 'FT_FIRST_FAILED_RUNG=%s\n' "$first_failed"
  printf 'FT_FIRST_FAILED_REASON=%s\n' "$first_reason"

  [ "$first_failed" = "none" ]
}

# Allow direct invocation for ad-hoc classification:
#   scripts/gvisor/freeze-thaw-rungs.sh <sentinel-file>
# When sourced, ${BASH_SOURCE[0]} != $0 and this block is skipped (no side effect).
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  ft_classify_rungs "${1:--}"
fi
