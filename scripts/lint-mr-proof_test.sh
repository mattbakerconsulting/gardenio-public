#!/usr/bin/env bash
# lint-mr-proof_test.sh — Unit tests for lint-mr-proof.sh
#
# Each test writes a synthetic MR description to a temp file and asserts the
# lint's exit status. No external services. Runs entirely in the main shell
# (no pipes into the assert helper) so pass/fail counters are reliable.
#
# Usage:  bash scripts/lint-mr-proof_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/lint-mr-proof.sh"
TEMPLATE="${SCRIPT_DIR}/../.gitlab/merge_request_templates/Default.md"

PASS=0
FAIL=0
TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# assert_exit <expected> <name> <file> [env-assignments...]
# Runs the lint on <file> and compares exit status to <expected>.
assert_exit() {
  local expected="$1" name="$2" file="$3"; shift 3
  local out rc
  out="$(env -u CI_MERGE_REQUEST_DESCRIPTION -u CI_MERGE_REQUEST_TITLE "$@" \
        bash "$SCRIPT_UNDER_TEST" "$file" 2>&1)" && rc=0 || rc=$?
  if [[ "$rc" == "$expected" ]]; then
    pass "$name"
  else
    fail "$name" "expected exit ${expected}, got ${rc}. Output: ${out}"
  fi
}

# Write a description to a fresh temp file and echo its path.
mkdesc() {
  local f
  f="$(mktemp "${TMPDIR_T}/desc.XXXXXX")"
  cat >"$f"
  echo "$f"
}

# A complete, valid proof packet (daemon-api surface — no per-surface
# evidence-shape heuristic, so it isolates the field checks).
complete_block() {
  cat <<'EOF'
## Summary

Adds the foo endpoint.

<!-- PROOF:BEGIN -->
## Proof

- **Claim:** GET /v1/foo returns the bead's foo field
- **Surface:** daemon-api
- **Evidence:** before: `curl /v1/foo` 404; after: 200 with `{"foo":1}` (run against local daemon)
- **Not established:** behavior under concurrent writes; prod-scale datasets
<!-- PROOF:END -->

## Beads

- ka-0000004403
EOF
}

# ── Field / marker checks ───────────────────────────────────────────

# 1. Complete proof block passes.
f="$(complete_block | mkdesc)"
assert_exit 0 "complete proof block passes" "$f"

# 2. No PROOF block, no exemption → fails (unlike trickle, this gates all MRs).
f="$(printf '## Summary\n\nAn ordinary MR with no proof block.\n' | mkdesc)"
assert_exit 1 "MR without proof block fails" "$f"

# 3. Missing a required field fails.
f="$(complete_block | grep -v 'Not established' | mkdesc)"
assert_exit 1 "missing not-established field fails" "$f"

# 4. Unfilled template (placeholder comments) fails.
assert_exit 1 "unfilled template fails" "$TEMPLATE"

# 5. Empty-value field fails.
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** adds foo
- **Surface:** daemon-api
- **Evidence:**
- **Not established:** nothing else
<!-- PROOF:END -->
EOF
)"
assert_exit 1 "empty field value fails" "$f"

# 6. Comment-only value (key present, value is just an HTML comment) fails.
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** adds foo
- **Surface:** daemon-api
- **Evidence:** <!-- link goes here -->
- **Not established:** nothing else
<!-- PROOF:END -->
EOF
)"
assert_exit 1 "comment-only value fails" "$f"

# 7. Prose mention of the marker token (e.g. the MR introducing this lint)
#    does not count as a block — only real HTML-comment markers do.
f="$(mkdesc <<'EOF'
## Summary

Documents the proof-packet norm with `PROOF:BEGIN` / `PROOF:END` markers.
No real block here, so this fails the norm (and must not false-parse).
EOF
)"
assert_exit 1 "prose mention of marker token is not a block" "$f"

# ── Placeholder detection: shape, not substring (ka-0000000685) ──────
# The guard must reject an UNFILLED field, not substantive prose that merely
# contains a common token like "e.g." / "one of:" / "XXXX". Regression for the
# unanchored-substring false positive that reded MR !6687.

# R1. Substantive prose containing "e.g." passes (the live MR !6687 case).
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** adds the foo endpoint
- **Surface:** daemon-api
- **Evidence:** before: `curl /v1/foo` 404; after: 200 (local daemon)
- **Not established:** whether anything currently depends on that parent-child edge (e.g. a `gb crew`/`gb news` lookup), so removing it could regress that path; not tested at prod scale
<!-- PROOF:END -->
EOF
)"
assert_exit 0 "substantive prose containing 'e.g.' passes" "$f"

# R2. Substantive prose containing "one of:" and "XXXX" passes (both were
#     unanchored substring hazards).
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** redaction masks the token as XXXX in logs
- **Surface:** daemon-api
- **Evidence:** log line now reads `token=XXXX`; verified the masker fires on one of: header, query, body
- **Not established:** rotation path
<!-- PROOF:END -->
EOF
)"
assert_exit 0 "substantive prose containing 'one of:' and 'XXXX' passes" "$f"

# R3. A genuinely untouched template value (placeholder sentence, comment
#     markers stripped) still FAILS — the guard is not weakened to a no-op.
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** the user/operator-visible behavior this MR delivers, in one sentence
- **Surface:** daemon-api
- **Evidence:** before/after curl output against local daemon
- **Not established:** prod scale
<!-- PROOF:END -->
EOF
)"
assert_exit 1 "untouched template placeholder sentence still fails" "$f"

# R4. The raw template file (placeholders as HTML comments) still fails — an
#     unfilled field must never pass. (Complements test 4 above, asserting the
#     rewrite kept the empty/comment path intact.)
assert_exit 1 "raw template (comment placeholders) still fails" "$TEMPLATE"

# ── Surface enum + claim/evidence match ─────────────────────────────

# 8. Unknown surface value fails.
f="$(complete_block | sed 's/daemon-api/backend/' | mkdesc)"
assert_exit 1 "unknown surface value fails" "$f"

# 9. viewer-ui claim with unit-test-only evidence fails (the canonical
#    claim=UI / evidence=unit-tests-only mismatch from the norm).
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** plan cards render a task grid
- **Surface:** viewer-ui
- **Evidence:** go test ./cmd/beads-viewer/... passes
- **Not established:** mobile layout
<!-- PROOF:END -->
EOF
)"
assert_exit 1 "viewer-ui claim with test-only evidence fails" "$f"

# 10. viewer-ui claim with an uploaded-image link passes.
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** plan cards render a task grid
- **Surface:** viewer-ui
- **Evidence:** https://gardenio.example.com/image/gardenio%2Fimages%2Fkd-x%2Fgrid.png (after) vs main (before)
- **Not established:** mobile layout
<!-- PROOF:END -->
EOF
)"
assert_exit 0 "viewer-ui claim with image link passes" "$f"

# 11. pipeline-ci claim with no run link fails.
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** release preflight now skips docs-only MRs
- **Surface:** pipeline-ci
- **Evidence:** the rules: block reads correctly
- **Not established:** scheduled pipelines
<!-- PROOF:END -->
EOF
)"
assert_exit 1 "pipeline-ci claim without run link fails" "$f"

# 12. pipeline-ci claim with a pipeline link passes.
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** release preflight now skips docs-only MRs
- **Surface:** pipeline-ci
- **Evidence:** https://gitlab.com/example-org/gardenio/-/pipelines/123456 (docs-only MR, job skipped)
- **Not established:** scheduled pipelines
<!-- PROOF:END -->
EOF
)"
assert_exit 0 "pipeline-ci claim with run link passes" "$f"

# ── Multi-line field values (ka-0000006457) ─────────────────────────
# A field's value is the WHOLE bullet, not just its first line. An Evidence
# bullet that opens with a sentence of framing and puts its run links,
# transcripts and before/after output on the continuation lines is still
# evidence — the old first-line-only read rejected exactly the best-structured
# packets (measured on MR !8103).

# M1. pipeline-ci run link on a continuation line passes (the live !8103 case).
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** the three mux e2e specs no longer flake
- **Surface:** pipeline-ci
- **Evidence:** CI `coop-e2e-playwright` on this branch's HEAD, against the
  bead's own cited baseline run. Same workflow, same runner.

  https://github.com/acme/gardenio/actions/runs/076a87549b69418bbc138b72d42c9d36
- **Not established:** behavior on other runners
<!-- PROOF:END -->
EOF
)"
assert_exit 0 "pipeline-ci run link on a continuation line passes" "$f"

# M2. viewer-ui screenshot on a continuation line passes.
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** plan cards render a task grid
- **Surface:** viewer-ui
- **Evidence:** captured against a local viewer with the seeded fixture plan:

  ![after](https://gardenio.example.com/image/gardenio%2Fimages%2Fkd-x%2Fgrid.png)
- **Not established:** mobile layout
<!-- PROOF:END -->
EOF
)"
assert_exit 0 "viewer-ui screenshot on a continuation line passes" "$f"

# M3. The value stops at the next sibling field — a link that lives in
#     *Not established* does not satisfy the Evidence heuristic. (The fix must
#     widen the read to the bullet, not to the whole PROOF block.)
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** release preflight now skips docs-only MRs
- **Surface:** pipeline-ci
- **Evidence:** the rules: block reads correctly
- **Not established:** scheduled pipelines — see https://gitlab.com/example-org/gardenio/-/pipelines/123456
<!-- PROOF:END -->
EOF
)"
assert_exit 1 "link in a sibling field does not satisfy Evidence" "$f"

# M4. A fenced code block inside Evidence cannot end the value: a fence whose
#     content starts with "- " / "# " must not be read as a sibling bullet or
#     heading, so the link *after* the fence still counts.
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** the reaper logs each skipped namespace
- **Surface:** pipeline-ci
- **Evidence:** before/after job output:

```
- ns/foo skipped
# summary: 1 skipped
```

  Run: https://github.com/acme/gardenio/actions/runs/deadbeefdeadbeefdeadbeefdeadbeef
- **Not established:** multi-cluster
<!-- PROOF:END -->
EOF
)"
assert_exit 0 "fenced block inside Evidence does not truncate the value" "$f"

# M5. Nested sub-bullets under Evidence count as part of the value.
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** the e2e smoke suite gates prod deploy
- **Surface:** pipeline-ci
- **Evidence:**
  - before: deploy ran with the suite red
  - after: https://github.com/acme/gardenio/actions/runs/1b1ec49007904985b93676e118910e5f blocked the deploy
- **Not established:** partial-failure semantics
<!-- PROOF:END -->
EOF
)"
assert_exit 0 "nested sub-bullets under Evidence count" "$f"

# M6. A bullet whose only continuation is blank lines is still an empty value.
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** adds foo
- **Surface:** daemon-api
- **Evidence:**

- **Not established:** nothing else
<!-- PROOF:END -->
EOF
)"
assert_exit 1 "blank-only continuation is still an empty value" "$f"

# M7. Conversely, a field whose content lives entirely on continuation lines
#     is FILLED — the placeholder check reads the bullet, not the first line.
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** adds the foo endpoint
- **Surface:** daemon-api
- **Evidence:**
  before: `curl /v1/foo` 404
  after: 200 with `{"foo":1}` (local daemon)
- **Not established:** prod scale
<!-- PROOF:END -->
EOF
)"
assert_exit 0 "content only on continuation lines counts as filled" "$f"

# M8. The placeholder guard is not weakened by the wider read: a field that
#     still carries the template's placeholder sentence fails even when real
#     continuation prose follows it.
f="$(mkdesc <<'EOF'
<!-- PROOF:BEGIN -->
- **Claim:** the user/operator-visible behavior this MR delivers, in one sentence
  and some follow-up prose the author did write
- **Surface:** daemon-api
- **Evidence:** before: `curl /v1/foo` 404; after: 200 (local daemon)
- **Not established:** prod scale
<!-- PROOF:END -->
EOF
)"
assert_exit 1 "template placeholder still fails with continuation prose" "$f"

# M9. CRLF line endings (GitLab sends CRLF in CI_MERGE_REQUEST_DESCRIPTION)
#     do not break the bullet walk: a link on a continuation line still counts,
#     and a bullet with no link still fails.
f="$(mktemp "${TMPDIR_T}/desc.XXXXXX")"
printf '<!-- PROOF:BEGIN -->\r\n- **Claim:** the reaper skips live namespaces\r\n- **Surface:** pipeline-ci\r\n- **Evidence:** framing sentence, link below\r\n\r\n  https://github.com/acme/gardenio/actions/runs/deadbeefdeadbeefdeadbeefdeadbeef\r\n- **Not established:** other clusters\r\n<!-- PROOF:END -->\r\n' >"$f"
assert_exit 0 "CRLF description with link on a continuation line passes" "$f"

f="$(mktemp "${TMPDIR_T}/desc.XXXXXX")"
printf '<!-- PROOF:BEGIN -->\r\n- **Claim:** the reaper skips live namespaces\r\n- **Surface:** pipeline-ci\r\n- **Evidence:** the rules: block reads correctly\r\n- **Not established:** other clusters\r\n<!-- PROOF:END -->\r\n' >"$f"
assert_exit 1 "CRLF description without a run link still fails" "$f"

# ── Off-ramps ───────────────────────────────────────────────────────

# 13. Draft MR (CI title env) is skipped.
f="$(printf 'no proof yet\n' | mkdesc)"
assert_exit 0 "draft MR is skipped" "$f" CI_MERGE_REQUEST_TITLE="Draft: wip thing"

# 14. Trickle MR (real PROVENANCE markers) defers to the trickle lint.
f="$(mkdesc <<'EOF'
## Summary

A trickle peel.

<!-- PROVENANCE:BEGIN -->
- **Source dark commits/MRs:** dark!1
<!-- PROVENANCE:END -->
EOF
)"
assert_exit 0 "trickle MR defers to trickle lint" "$f"

# 15. Proof-Exempt with a real reason passes.
f="$(printf 'Proof-Exempt: docs typo fix, diff is the evidence\n' | mkdesc)"
assert_exit 0 "proof-exempt with reason passes" "$f"

# 16. Proof-Exempt with no reason fails.
f="$(printf 'Proof-Exempt:\n' | mkdesc)"
assert_exit 1 "proof-exempt without reason fails" "$f"

# 17. Truncated CI snapshot whose PROOF:END fell past the cut is now validated
#     on its VISIBLE part, not skipped: this block shows only Claim, so the
#     other three required fields are absent and the job FAILS (ka-0000001603).
#     Before this fix it exited 0 and the packet went unenforced.
f="$(printf '## Summary\n\nlong description…\n<!-- PROOF:BEGIN -->\n- **Claim:** cut off\n' | mkdesc)"
assert_exit 1 "truncated cut-off block with missing fields fails (no silent skip)" "$f" \
  CI_MERGE_REQUEST_DESCRIPTION_IS_TRUNCATED=true

# 17b. ...and the failure says truncation is why a field may look absent, so the
#      author moves the block up instead of re-adding a field that is there.
f="$(printf '## Summary\n\nlong description…\n<!-- PROOF:BEGIN -->\n- **Claim:** cut off\n' | mkdesc)"
out="$(env -u CI_MERGE_REQUEST_DESCRIPTION -u CI_MERGE_REQUEST_TITLE \
       CI_MERGE_REQUEST_DESCRIPTION_IS_TRUNCATED=true \
       bash "$SCRIPT_UNDER_TEST" "$f" 2>&1)" && rc=0 || rc=$?
if grep -qi 'TRUNCATED snapshot' <<<"$out" && grep -qi 'near the TOP' <<<"$out"; then
  pass "partial-validation failure explains the truncation"
else
  fail "partial-validation failure explains the truncation" "got: ${out}"
fi

# 17c. A cut-off block whose four required fields are ALL visible before the cut
#      still passes — partial validation must not red an MR whose packet is
#      complete and merely lost its END marker.
f="$(complete_block | grep -v 'PROOF:END' | mkdesc)"
assert_exit 0 "truncated cut-off block with all fields visible passes" "$f" \
  CI_MERGE_REQUEST_DESCRIPTION_IS_TRUNCATED=true

# 17d. ...and it says so, rather than claiming a fully validated packet.
f="$(complete_block | grep -v 'PROOF:END' | mkdesc)"
out="$(env -u CI_MERGE_REQUEST_DESCRIPTION -u CI_MERGE_REQUEST_TITLE \
       CI_MERGE_REQUEST_DESCRIPTION_IS_TRUNCATED=true \
       bash "$SCRIPT_UNDER_TEST" "$f" 2>&1)" && rc=0 || rc=$?
if grep -qi 'PARTIAL validation' <<<"$out"; then
  pass "partial pass is labelled PARTIAL"
else
  fail "partial pass is labelled PARTIAL" "got: ${out}"
fi

# 17e. A shape heuristic a mid-value cut could false-fail (the pipeline-ci run
#      link) softens to a warning under partial validation — the field is there,
#      the link may simply be below the cut.
f="$(printf '## Summary\n\nlong description…\n<!-- PROOF:BEGIN -->\n- **Claim:** the lint reds a bad packet\n- **Surface:** pipeline-ci\n- **Evidence:** see the job linked at the bottom of this description\n- **Not established:** behaviour on other runners\n' | mkdesc)"
assert_exit 0 "partial validation softens the evidence heuristic to a warning" "$f" \
  CI_MERGE_REQUEST_DESCRIPTION_IS_TRUNCATED=true

# 17f. The same packet is a HARD fail when the description is NOT truncated —
#      softening is scoped to the truncated snapshot, not the surface check.
f="$(printf '## Summary\n\n<!-- PROOF:BEGIN -->\n- **Claim:** the lint reds a bad packet\n- **Surface:** pipeline-ci\n- **Evidence:** see the job linked at the bottom of this description\n- **Not established:** behaviour on other runners\n<!-- PROOF:END -->\n' | mkdesc)"
assert_exit 1 "evidence heuristic stays hard when not truncated" "$f"

# 18. Truncated flag but block fully visible still enforces normally.
f="$(complete_block | grep -v 'Not established' | mkdesc)"
assert_exit 1 "truncated flag with visible incomplete block still fails" "$f" \
  CI_MERGE_REQUEST_DESCRIPTION_IS_TRUNCATED=true

# 18b. Truncated flag with NO PROOF block at all still FAILS (gd-J26tmwa9lp).
#      Truncation must not make an absent block indistinguishable from a
#      cut-off one: with no BEGIN marker, the block is missing, not truncated,
#      so a long (>2700-char) description with no proof packet must still red.
f="$(printf '## Summary\n\nA long portal IAM change with no proof block at all.\n' | mkdesc)"
assert_exit 1 "truncated flag with no PROOF block still fails" "$f" \
  CI_MERGE_REQUEST_DESCRIPTION_IS_TRUNCATED=true

# 18c. ...and that failure NAMES truncation as the likely cause (ka-0000004000).
#      A correct PROOF block that sits below the ~2700-char cut is invisible to
#      CI while passing locally, so the error must point at the snapshot rather
#      than leave the author re-verifying a block that is genuinely there.
f="$(printf '## Summary\n\nA long change whose PROOF block fell below the CI snapshot cut.\n' | mkdesc)"
out="$(env -u CI_MERGE_REQUEST_DESCRIPTION -u CI_MERGE_REQUEST_TITLE \
       CI_MERGE_REQUEST_DESCRIPTION_IS_TRUNCATED=true \
       bash "$SCRIPT_UNDER_TEST" "$f" 2>&1)" && rc=0 || rc=$?
if grep -qi 'truncated this description' <<<"$out" && grep -qi 'near the TOP' <<<"$out"; then
  pass "no-PROOF failure names truncation as likely cause when truncated"
else
  fail "no-PROOF failure names truncation as likely cause when truncated" \
    "expected a truncation hint, got: ${out}"
fi

# 18d. The hint is NOT emitted when the description was not truncated — an
#      author who simply forgot the block must not be sent chasing a phantom.
f="$(printf '## Summary\n\nA short change that forgot the proof block.\n' | mkdesc)"
out="$(env -u CI_MERGE_REQUEST_DESCRIPTION -u CI_MERGE_REQUEST_TITLE \
       bash "$SCRIPT_UNDER_TEST" "$f" 2>&1)" && rc=0 || rc=$?
if grep -qi 'truncated' <<<"$out"; then
  fail "no truncation hint when description is not truncated" \
    "unexpected truncation hint: ${out}"
else
  pass "no truncation hint when description is not truncated"
fi

# 19. CI_MERGE_REQUEST_DESCRIPTION env path works for a complete block.
out="$(CI_MERGE_REQUEST_DESCRIPTION="$(complete_block)" bash "$SCRIPT_UNDER_TEST" 2>&1)" && rc=0 || rc=$?
if [[ "$rc" == "0" ]]; then pass "reads CI_MERGE_REQUEST_DESCRIPTION env"; else
  fail "reads CI_MERGE_REQUEST_DESCRIPTION env" "expected 0, got ${rc}: ${out}"; fi

# ── API description read (ka-0000001603) ────────────────────────────
#
# The real fix is to stop trusting the truncated variable at all: with an MR
# context the lint reads the FULL description from the API. These tests stub
# `curl` (serves the description) and `jq` (pass-through), so they exercise the
# script's plumbing — env guards, header choice, source labelling, and the fact
# that a truncated-flag snapshot is ignored once we hold the whole text — not
# GitLab itself.
STUB_BIN="${TMPDIR_T}/stubbin"
mkdir -p "$STUB_BIN"
cat >"${STUB_BIN}/jq" <<'EOF'
#!/usr/bin/env bash
cat
EOF
chmod +x "${STUB_BIN}/jq"

# The stub honours the real call shape: body to the `-o` file, HTTP code on
# stdout (that is how the script learns WHY a read failed).
#   ok            -> 200 + the description in API_DESC_FILE
#   fail          -> curl-level failure (exit 22), no code printed
#   <3-digit code> -> that HTTP code with an empty body
make_curl_stub() {  # make_curl_stub <mode: ok|fail|NNN>
  if [[ "$1" == "fail" ]]; then
    printf '#!/usr/bin/env bash\nexit 22\n' >"${STUB_BIN}/curl"
  else
    cat >"${STUB_BIN}/curl" <<EOF
#!/usr/bin/env bash
out=""
while (( \$# )); do
  case "\$1" in
    -o) out="\$2"; shift 2 ;;
    *) shift ;;
  esac
done
[[ -n "\$out" ]] || out=/dev/null
if [[ "$1" == "ok" ]]; then
  cat "${API_DESC_FILE}" >"\$out"
  printf '200'
else
  : >"\$out"
  printf '%s' "$1"
fi
EOF
  fi
  chmod +x "${STUB_BIN}/curl"
}

# 22. A complete packet served by the API passes even though the CI snapshot is
#     flagged truncated and carries no PROOF block at all.
API_DESC_FILE="$(complete_block | mkdesc)"
make_curl_stub ok
out="$(env -u CI_MERGE_REQUEST_TITLE \
       PATH="${STUB_BIN}:${PATH}" \
       CI_API_V4_URL=https://gitlab.example/api/v4 CI_PROJECT_ID=1 CI_MERGE_REQUEST_IID=7 \
       GITLAB_CLONE_TOKEN=stub-token \
       CI_MERGE_REQUEST_DESCRIPTION='## Summary

a long description whose proof block is past the cut' \
       CI_MERGE_REQUEST_DESCRIPTION_IS_TRUNCATED=true \
       bash "$SCRIPT_UNDER_TEST" 2>&1)" && rc=0 || rc=$?
if [[ "$rc" == "0" ]] && grep -qi 'read the full description from the MR API' <<<"$out"; then
  pass "API read beats a truncated snapshot"
else
  fail "API read beats a truncated snapshot" "expected 0 + API source line, got ${rc}: ${out}"
fi

# 23. An incomplete packet served by the API FAILS — the API path enforces for
#     real; it is not a new way to pass.
API_DESC_FILE="$(complete_block | grep -v 'Not established' | mkdesc)"
make_curl_stub ok
out="$(env -u CI_MERGE_REQUEST_TITLE \
       PATH="${STUB_BIN}:${PATH}" \
       CI_API_V4_URL=https://gitlab.example/api/v4 CI_PROJECT_ID=1 CI_MERGE_REQUEST_IID=7 \
       GITLAB_CLONE_TOKEN=stub-token \
       CI_MERGE_REQUEST_DESCRIPTION='short' \
       CI_MERGE_REQUEST_DESCRIPTION_IS_TRUNCATED=true \
       bash "$SCRIPT_UNDER_TEST" 2>&1)" && rc=0 || rc=$?
if [[ "$rc" == "1" ]]; then pass "API read still enforces missing fields"; else
  fail "API read still enforces missing fields" "expected 1, got ${rc}: ${out}"; fi

# 24. A failed API read degrades to the truncated variable — and, being
#     truncated with no visible block, still fails rather than skipping.
make_curl_stub fail
out="$(env -u CI_MERGE_REQUEST_TITLE \
       PATH="${STUB_BIN}:${PATH}" \
       CI_API_V4_URL=https://gitlab.example/api/v4 CI_PROJECT_ID=1 CI_MERGE_REQUEST_IID=7 \
       GITLAB_CLONE_TOKEN=stub-token \
       CI_MERGE_REQUEST_DESCRIPTION='## Summary

a long description with no proof block' \
       CI_MERGE_REQUEST_DESCRIPTION_IS_TRUNCATED=true \
       bash "$SCRIPT_UNDER_TEST" 2>&1)" && rc=0 || rc=$?
if [[ "$rc" == "1" ]] && grep -qi 'falling back to the TRUNCATED CI snapshot' <<<"$out"; then
  pass "failed API read degrades to the variable and still enforces"
else
  fail "failed API read degrades to the variable and still enforces" "expected 1 + fallback warning, got ${rc}: ${out}"
fi

# 26. A refused API read names the credential and its HTTP code, so the log
#     says WHY it fell back instead of leaving a silent mystery.
make_curl_stub 401
out="$(env -u CI_MERGE_REQUEST_TITLE \
       PATH="${STUB_BIN}:${PATH}" \
       CI_API_V4_URL=https://gitlab.example/api/v4 CI_PROJECT_ID=1 CI_MERGE_REQUEST_IID=7 \
       GITLAB_CLONE_TOKEN=stub-token CI_JOB_TOKEN=stub-job-token \
       CI_MERGE_REQUEST_DESCRIPTION='## Summary

a long description with no proof block' \
       CI_MERGE_REQUEST_DESCRIPTION_IS_TRUNCATED=true \
       bash "$SCRIPT_UNDER_TEST" 2>&1)" && rc=0 || rc=$?
if [[ "$rc" == "1" ]] \
   && grep -q 'GITLAB_CLONE_TOKEN→HTTP 401' <<<"$out" \
   && grep -q 'CI_JOB_TOKEN→HTTP 401' <<<"$out"; then
  pass "failed API read reports each credential and its HTTP code"
else
  fail "failed API read reports each credential and its HTTP code" "expected 1 + per-credential codes, got ${rc}: ${out}"
fi

# 27. The "GitLab truncated your description" hint keys off the description we
#     actually judged, not the raw CI flag: an API-sourced description with a
#     genuinely absent block must not send the author hunting for a block they
#     never wrote.
API_DESC_FILE="$(printf '## Summary\n\nno proof block anywhere in the FULL description.\n' | mkdesc)"
make_curl_stub ok
out="$(env -u CI_MERGE_REQUEST_TITLE \
       PATH="${STUB_BIN}:${PATH}" \
       CI_API_V4_URL=https://gitlab.example/api/v4 CI_PROJECT_ID=1 CI_MERGE_REQUEST_IID=7 \
       GITLAB_CLONE_TOKEN=stub-token \
       CI_MERGE_REQUEST_DESCRIPTION='## Summary' \
       CI_MERGE_REQUEST_DESCRIPTION_IS_TRUNCATED=true \
       bash "$SCRIPT_UNDER_TEST" 2>&1)" && rc=0 || rc=$?
if [[ "$rc" == "1" ]] \
   && grep -qi 'no PROOF block found' <<<"$out" \
   && ! grep -qi 'LIKELY CAUSE' <<<"$out"; then
  pass "no phantom truncation hint on an API-sourced description"
else
  fail "no phantom truncation hint on an API-sourced description" "expected 1 without LIKELY CAUSE, got ${rc}: ${out}"
fi

# 25. With no MR context (no IID) the API is not attempted at all — the plain
#     file/stdin path must stay usable locally with no network.
f="$(complete_block | mkdesc)"
assert_exit 0 "local file path needs no API" "$f"

# ── Summary ─────────────────────────────────────────────────────────
echo "----"
echo "lint-mr-proof_test: ${PASS} passed, ${FAIL} failed"
(( FAIL == 0 ))
