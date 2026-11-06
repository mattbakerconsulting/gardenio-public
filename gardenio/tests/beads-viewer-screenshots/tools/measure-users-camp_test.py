#!/usr/bin/env python3
"""Unit tests for measure-users-camp.py's /u/{slug} Activity header invariant.

Regression guard for bug ka-0000006350: the Activity section header renders
"(N)" and the page enumerates N activity rows; the two must agree, EXACTLY, in
a single capture. The audit tool used to record only ``activity_row_count`` for
the detail page and left the header to a human eyeball, so a header/row
disagreement could only be caught by comparing two SEPARATE captures — which is
exactly how a phantom off-by-one got filed (the measure pod's 84-row capture at
T1 vs the judge's 83-row re-capture at T2, one activity event having shifted
between them).

These tests pin ``detail_signals``'s new single-capture invariant:
``activity_header_vs_rows_delta`` = header - enumerable rows, computed from ONE
DOM string, plus the FAIL-FLAG that surfaces a genuine nonzero drift. Hermetic —
no network, no ``kd``, synthetic HTML fixtures.
"""
import os
import unittest
from importlib import util as _imputil

HERE = os.path.dirname(os.path.abspath(__file__))
TOOL_PATH = os.path.join(HERE, "measure-users-camp.py")

_spec = _imputil.spec_from_file_location("measure_users_camp", TOOL_PATH)
_mod = _imputil.module_from_spec(_spec)
_spec.loader.exec_module(_mod)  # type: ignore[union-attr]


def _spine(header_n, n_rows, n_message_rows=0, n_rails=0):
    """Build a minimal #activity spine DOM.

    header_n: the integer the "Activity (N)" header prints (None ⇒ no header,
    the empty-activity shape). n_rows: total <li class="activity-row"> rendered,
    of which n_message_rows carry the is-message variant. n_rails: interleaved
    <li class="timeline-rail"> markers (must NOT count toward the header).
    """
    parts = ['<section id="activity" class="user-home-section is-filled">']
    if header_n is None:
        parts.append("<h2>Activity</h2>")
    else:
        parts.append(f'<h2>Activity <span class="meta">({header_n})</span></h2>')
    parts.append('<ul class="bead-list activity-stream" data-activity-stream>')
    for i in range(n_rails):
        parts.append(f'<li class="timeline-rail" data-activity-rail><span>rail{i}</span></li>')
    for i in range(n_rows):
        msg = " is-message" if i < n_message_rows else ""
        parts.append(
            f'<li class="bead-row activity-row{msg}" data-activity-item '
            f'data-facet="closed">row{i}</li>'
        )
    parts.append("</ul></section>")
    return "".join(parts)


class ActivityHeaderInvariant(unittest.TestCase):
    def test_header_matches_rows_zero_delta(self):
        sig = _mod.detail_signals(_spine(3, 3, n_message_rows=1, n_rails=1))
        self.assertEqual(sig["activity_row_count"], 3)
        self.assertEqual(sig["activity_header_count"], 3)
        self.assertEqual(sig["activity_header_vs_rows_delta"], 0)
        # A date rail must never be counted as a row nor inflate the header.
        self.assertEqual(sig["timeline_rail_count"], 1)

    def test_header_over_by_one_is_positive_delta(self):
        # The exact shape the bug ALLEGED: header says 84, only 83 rows render.
        sig = _mod.detail_signals(_spine(84, 83))
        self.assertEqual(sig["activity_row_count"], 83)
        self.assertEqual(sig["activity_header_count"], 84)
        self.assertEqual(sig["activity_header_vs_rows_delta"], 1)

    def test_header_under_by_one_is_negative_delta(self):
        sig = _mod.detail_signals(_spine(82, 83))
        self.assertEqual(sig["activity_header_vs_rows_delta"], -1)

    def test_empty_activity_has_no_header_and_null_delta(self):
        # The is-empty page renders "<h2>Activity</h2>" with no "(N)". The delta
        # must be None (not 0) so it never flags and never lifts/lowers a score.
        sig = _mod.detail_signals(_spine(None, 0))
        self.assertEqual(sig["activity_row_count"], 0)
        self.assertIsNone(sig["activity_header_count"])
        self.assertIsNone(sig["activity_header_vs_rows_delta"])

    def test_rails_do_not_count_toward_header_delta(self):
        # 5 rows, 3 rails, header 5 ⇒ delta 0 regardless of rail count.
        sig = _mod.detail_signals(_spine(5, 5, n_rails=3))
        self.assertEqual(sig["activity_header_vs_rows_delta"], 0)
        self.assertEqual(sig["timeline_rail_count"], 3)

    def test_script_and_style_stripped_before_counting(self):
        # user_home_activity.html ships an inline <script> mentioning
        # data-activity-item / activity-row in string literals. _body() strips
        # script/style, so those literals must NOT inflate the row count.
        dom = _spine(2, 2) + (
            '<script>var x="<li class=\\"activity-row\\">fake</li>";'
            'document.querySelectorAll("li.activity-row");</script>'
        )
        sig = _mod.detail_signals(dom)
        self.assertEqual(sig["activity_row_count"], 2)
        self.assertEqual(sig["activity_header_vs_rows_delta"], 0)


class PolishPixelBlackoutFlag(unittest.TestCase):
    """Regression guard for bug gd-DQLUhVH4mc: Polish gates this lane's terminal
    goal and is scored from PNG pixels, but under step-role dispatch the capture
    step's PNGs are gitignored and never reach the measure pod — so the whole
    dimension can go silently null while every other signal reads clean. A total
    pixel blackout must FAIL LOUDLY, a partial loss must stay a quiet per-PNG
    warning, and an empty polish map must never manufacture a false flag.
    """

    @staticmethod
    def _polish(people_desktop, people_narrow, home_desktop, home_narrow):
        # html sub-block is DOM-derived and must not affect the pixel verdict —
        # set it non-null everywhere to prove it is ignored.
        return {
            "people": {"html": {"dom_element_count": 1},
                       "desktop": people_desktop, "narrow": people_narrow},
            "user-home": {"html": {"dom_element_count": 1},
                          "desktop": home_desktop, "narrow": home_narrow},
        }

    def test_all_four_pixel_blocks_null_flags(self):
        # The exact gd-DQLUhVH4mc shape: 0 of 4 PNGs reached the measure pod.
        self.assertTrue(_mod.polish_pixels_all_null(
            self._polish(None, None, None, None)))

    def test_all_four_measured_does_not_flag(self):
        m = {"near_white_ratio": 0.3}
        self.assertFalse(_mod.polish_pixels_all_null(self._polish(m, m, m, m)))

    def test_partial_loss_does_not_flag(self):
        # Even ONE measured pixel block means Polish is scorable — the judge
        # skips the individual nulls, so this must stay a warning, not a flag.
        m = {"near_white_ratio": 0.3}
        self.assertFalse(_mod.polish_pixels_all_null(
            self._polish(m, None, None, None)))

    def test_html_block_does_not_rescue_a_pixel_blackout(self):
        # All pixel blocks null but html present ⇒ still a blackout: Polish is
        # scored from pixels, not the DOM sub-block.
        self.assertTrue(_mod.polish_pixels_all_null(
            self._polish(None, None, None, None)))

    def test_empty_polish_map_does_not_flag(self):
        # Nothing was ever asked for ⇒ no blocks ⇒ no false flag.
        self.assertFalse(_mod.polish_pixels_all_null({}))

    def test_surface_missing_viewport_keys_does_not_flag(self):
        # A surface dict with no desktop/narrow keys contributes no blocks; if
        # the only real blocks are measured, no flag.
        m = {"near_white_ratio": 0.3}
        polish = {"people": {"desktop": m, "narrow": m}, "user-home": {}}
        self.assertFalse(_mod.polish_pixels_all_null(polish))


def _people_row(email="", identities=(), name="", extra_body=""):
    """Build one minimal <li class="people-row"> matching people.html's shape.

    ``email`` renders the canonical mailto block; ``identities`` is a list of
    (kind, handle) rendered as people-identity chips with the same title= shape
    the template emits ("<kind> identity[ (canonical login)]: <handle>");
    ``name`` is the people-row-name link text; ``extra_body`` is arbitrary
    row-local content (a recent-bead title, a thread-request preview) used to
    prove the classifier scans IDENTIFIERS only, never the whole block.
    """
    parts = ['<li class="bead-row people-row" data-operator-id="1"><div class="bead-row-top"><div class="bead-row-text">']
    parts.append(f'<a class="bead-row-title people-row-name" href="/u/x">{name}</a>')
    if email:
        parts.append(
            f'<div class="people-row-email"><a href="mailto:{email}">{email}</a></div>'
        )
    parts.append('<div class="chip-strip people-row-identities">')
    for kind, handle in identities:
        parts.append(
            f'<span class="chip chip-type people-identity" '
            f'title="{kind} identity: {handle}">'
            f'<span class="people-identity-kind">{kind}</span> {handle} '
            f'<a href="/u/x">/u/x</a></span>'
        )
    parts.append("</div>")
    if extra_body:
        parts.append(f'<div class="people-row-recent">{extra_body}</div>')
    parts.append("</div></div></li>")
    return "".join(parts)


def _people_page(rows):
    return (
        '<h1>Users <span class="meta">(N)</span></h1>'
        '<ul class="bead-list people-list">' + "".join(rows) + "</ul>"
    )


class BotRowAssertion(unittest.TestCase):
    """Regression guard for bug ka-0000001715: the INDEPENDENT humans-only signal.

    row_vs_truth_delta shares the page's own {oidc,slack} classifier, so a bot
    rendered as a human cancels out of the subtraction and the delta stays 0 —
    it proves the page agrees with itself, not that the roster is humans-only.
    bot_rows_rendered is the second, independently-derived measurement that keys
    off objective non-human markers in each rendered row's identifiers, so it
    catches exactly the class the delta is blind to (the four junk rows the
    1151/1352 judge saw and Coverage 9/10 scored straight past).
    """

    def _count(self, rows):
        return _mod.people_bot_rows(_mod._body(_people_page(rows)))

    def test_clean_human_rows_report_zero(self):
        rows = [
            _people_row(email="matthew.baker@acme.example",
                        identities=[("oidc", "matthew.baker@acme.example"),
                                    ("slack", "U0OPERATOR1")],
                        name="Matthew Baker"),
            _people_row(email="devin.hale@contractor-c.example",
                        identities=[("oidc", "devin.hale@contractor-c.example")],
                        name="Devin Hale"),
        ]
        got = self._count(rows)
        self.assertEqual(got["bot_rows_rendered"], 0)
        self.assertEqual(got["bot_rows_sample"], [])

    def test_gitlab_bot_token_flagged(self):
        # The verbatim offender from the 1352 judge: a GitLab bot service account
        # (kind=oidc, email-shaped subject) sails through the {oidc,slack} filter.
        addr = "group_14176856_bot_1dd8a88e50156d4a6dcf9d03e75d8d34@noreply.gitlab.com"
        got = self._count([
            _people_row(email=addr, identities=[("oidc", addr)], name=addr),
        ])
        self.assertEqual(got["bot_rows_rendered"], 1)
        self.assertIn(addr, got["bot_rows_sample"])

    def test_noreply_kbeads_and_fixture_flagged(self):
        rows = [
            _people_row(email="slack-U0PERSON002@noreply.kbeads.local",
                        identities=[("slack", "U0PERSON002")], name=""),
            _people_row(email="e2e-fixture@example.com",
                        identities=[("oidc", "e2e-fixture@example.com")],
                        name="e2e-fixture@example.com"),
        ]
        got = self._count(rows)
        self.assertEqual(got["bot_rows_rendered"], 2)
        self.assertIn("slack-U0PERSON002@noreply.kbeads.local", got["bot_rows_sample"])
        self.assertIn("e2e-fixture@example.com", got["bot_rows_sample"])

    def test_github_noreply_sublabel_flagged(self):
        # noreply as a sub-label (…@users.noreply.github.com) is a bot address too.
        addr = "12345+svc@users.noreply.github.com"
        got = self._count([_people_row(email=addr, identities=[("oidc", addr)])])
        self.assertEqual(got["bot_rows_rendered"], 1)

    def test_mixed_page_counts_only_bot_rows(self):
        # The real /people shape: a majority of humans plus a handful of bots.
        # Result must be the ROW count (7 offenders → 7), never a marker-hit count.
        humans = [
            _people_row(email=f"human{i}@acme.example",
                        identities=[("oidc", f"human{i}@acme.example")],
                        name=f"Human {i}")
            for i in range(39)
        ]
        bots = [
            _people_row(email="e2e-fixture@example.com",
                        identities=[("oidc", "e2e-fixture@example.com")]),
            _people_row(email="group_1_bot_abc@noreply.gitlab.com",
                        identities=[("oidc", "group_1_bot_abc@noreply.gitlab.com")]),
            _people_row(email="slack-U03ALLABCDE@noreply.kbeads.local",
                        identities=[("slack", "U03ALLABCDE")]),
            _people_row(email="slack-U0PERSON008@noreply.kbeads.local",
                        identities=[("slack", "U0PERSON008")]),
            _people_row(email="slack-U0PERSON002@noreply.kbeads.local",
                        identities=[("slack", "U0PERSON002")]),
            _people_row(email="slack-U0PERSON010@noreply.kbeads.local",
                        identities=[("slack", "U0PERSON010")], name="Product AI"),
            _people_row(email="slack-UTRIAGE@noreply.kbeads.local",
                        identities=[("slack", "UTRIAGE")]),
        ]
        got = self._count(humans + bots)
        self.assertEqual(got["bot_rows_rendered"], 7)
        self.assertEqual(len(got["bot_rows_sample"]),
                         len(set(got["bot_rows_sample"])))  # deduped

    def test_multiple_markers_in_one_row_count_once(self):
        # A row whose email AND identity handle both carry markers is ONE bot row.
        addr = "group_9_bot_x@noreply.gitlab.com"
        got = self._count([
            _people_row(email=addr, identities=[("oidc", addr)], name=addr),
        ])
        self.assertEqual(got["bot_rows_rendered"], 1)

    def test_human_content_mentioning_bot_does_not_flag(self):
        # THE false-positive guard: a real human whose recent-bead title or
        # thread-request preview quotes a noreply address / "_bot_" must NOT flag.
        # The classifier scans IDENTIFIERS (email/handle/name), never the block.
        got = self._count([
            _people_row(
                email="sky.bennett@acme.example",
                identities=[("oidc", "sky.bennett@acme.example")],
                name="Sky Bennett",
                extra_body=(
                    '<a class="chip people-recent-bead" '
                    'title="fix bot classifier: reject group_1_bot_x@noreply.gitlab.com">'
                    "gd-abc</a>"
                    '<span class="people-request-text">why is e2e-fixture@example.com '
                    "in the directory?</span>"
                ),
            ),
        ])
        self.assertEqual(got["bot_rows_rendered"], 0)
        self.assertEqual(got["bot_rows_sample"], [])

    def test_service_account_on_real_domain_not_flagged(self):
        # DELIBERATE non-flag: no objective address pattern separates a service
        # account on a REAL domain from a human, and the metric must be able to
        # reach 0 after the address-based viewer fix. Curating these is the
        # judge's issues[] call, not this objective signal.
        rows = [
            _people_row(email="info-density-audit@acme.example",
                        identities=[("oidc", "info-density-audit@acme.example")]),
            _people_row(email="gardenio@acme.example",
                        identities=[("oidc", "gardenio@acme.example")]),
        ]
        got = self._count(rows)
        self.assertEqual(got["bot_rows_rendered"], 0)

    def test_sample_is_capped(self):
        # A pathological page can never emit an unbounded sample blob.
        rows = [
            _people_row(email=f"group_{i}_bot_x@noreply.gitlab.com",
                        identities=[("oidc", f"group_{i}_bot_x@noreply.gitlab.com")])
            for i in range(_mod.BOT_ROW_SAMPLE_CAP + 15)
        ]
        got = self._count(rows)
        self.assertEqual(got["bot_rows_rendered"], _mod.BOT_ROW_SAMPLE_CAP + 15)
        self.assertLessEqual(len(got["bot_rows_sample"]), _mod.BOT_ROW_SAMPLE_CAP)

    def test_script_literals_do_not_create_phantom_bot_rows(self):
        # A <script> mentioning people-row + a noreply address in string literals
        # must not manufacture a row: index_signals runs people_bot_rows over the
        # _body()-stripped DOM, so prove the strip removes the literal.
        page = _people_page([
            _people_row(email="jordan.lake@acme.example",
                        identities=[("oidc", "jordan.lake@acme.example")],
                        name="Jordan Lake"),
        ]) + (
            '<script>var x=\'<li class="people-row">'
            '<a href="mailto:bot_x@noreply.gitlab.com">x</a></li>\';</script>'
        )
        got = _mod.people_bot_rows(_mod._body(page))
        self.assertEqual(got["bot_rows_rendered"], 0)

    def test_index_signals_surfaces_bot_metric(self):
        # The wiring: index_signals must expose bot_rows_rendered/-_sample so
        # main()'s coverage block and FAIL-FLAG can read them.
        sig = _mod.index_signals(_people_page([
            _people_row(email="e2e-fixture@example.com",
                        identities=[("oidc", "e2e-fixture@example.com")]),
            _people_row(email="real.human@acme.example",
                        identities=[("oidc", "real.human@acme.example")],
                        name="Real Human"),
        ]))
        self.assertEqual(sig["rendered_row_count"], 2)
        self.assertEqual(sig["bot_rows_rendered"], 1)
        self.assertIn("e2e-fixture@example.com", sig["bot_rows_sample"])


class ServiceRowAssertion(unittest.TestCase):
    """Regression guard for bug ka-0000004673: the INDEPENDENT service-identity signal.

    The companion to bot_rows_rendered for the class its address markers cannot
    reach — service/CI identities provisioned on a REAL domain
    (gardenio@acme.example, info-density-audit@acme.example, …). BOTH of the other
    Coverage correctness signals are blind to them: bot_rows_rendered deliberately
    does not widen its markers to real domains (it would false-positive real
    employees), and row_vs_truth_delta cancels a deny-listed row out because the
    page and its ground-truth query share the {oidc,slack} classifier. So the
    deny-list fix (MR 6172) shipped with server tests but no page-level assertion
    until service_rows_rendered: exact set-membership in SERVICE_ROW_SUBJECTS,
    kept in lockstep with serviceOperatorSubjects server-side, reproducing the
    four junk rows pre-fix and reading exactly 0 post-fix.
    """

    def _count(self, rows):
        return _mod.people_service_rows(_mod._body(_people_page(rows)))

    def test_clean_human_rows_report_zero(self):
        rows = [
            _people_row(email="matthew.baker@acme.example",
                        identities=[("oidc", "matthew.baker@acme.example"),
                                    ("slack", "U0OPERATOR1")],
                        name="Matthew Baker"),
            _people_row(email="devin.hale@contractor-c.example",
                        identities=[("oidc", "devin.hale@contractor-c.example")],
                        name="Devin Hale"),
        ]
        got = self._count(rows)
        self.assertEqual(got["service_rows_rendered"], 0)
        self.assertEqual(got["service_rows_sample"], [])

    def test_all_four_deny_list_subjects_flagged(self):
        # The exact pre-fix roster: the four marker-invisible service/CI identities
        # the deny-list removes. Each is kind=oidc on a real domain, so it sails
        # through {oidc,slack} AND carries no BOT_ROW_MARKER_RE marker.
        rows = [
            _people_row(email=s, identities=[("oidc", s)], name=s)
            for s in sorted(_mod.SERVICE_ROW_SUBJECTS)
        ]
        got = self._count(rows)
        self.assertEqual(got["service_rows_rendered"], 4)
        self.assertEqual(sorted(got["service_rows_sample"]),
                         sorted(_mod.SERVICE_ROW_SUBJECTS))

    def test_lockstep_with_server_deny_list(self):
        # LOCKSTEP guard: SERVICE_ROW_SUBJECTS must mirror the server's
        # serviceOperatorSubjects verbatim. A drift here re-opens the delta-blind
        # gap (a subject on one side but not the other renders while the signal
        # reads 0). Pins the exact set so a one-sided edit reds this test.
        self.assertEqual(_mod.SERVICE_ROW_SUBJECTS, frozenset({
            "gardenio@acme.example",
            "gardenio2@acme.example",
            "info-density-audit@acme.example",
            "joel.approver@acme.example",
        }))

    def test_service_subject_in_identity_chip_flagged(self):
        # A row whose canonical email is absent but whose identity chip carries the
        # service subject must still flag — the scan checks all identifier fields.
        got = self._count([
            _people_row(identities=[("oidc", "gardenio2@acme.example")]),
        ])
        self.assertEqual(got["service_rows_rendered"], 1)
        self.assertIn("gardenio2@acme.example", got["service_rows_sample"])

    def test_case_folded_membership(self):
        # Email domains are case-insensitive (RFC 1035 §2.3.3); the server lowercases
        # subject/email before the map lookup, so a mixed-case rendering of a listed
        # subject must screen identically.
        got = self._count([
            _people_row(email="Gardenio@acme.example",
                        identities=[("oidc", "Gardenio@acme.example")]),
        ])
        self.assertEqual(got["service_rows_rendered"], 1)

    def test_real_employee_on_same_domain_not_flagged(self):
        # THE false-positive guard the exact-match design exists for: jordan.lake@ is
        # a distinct human on the SAME domain as the listed joel.approver@ fixture.
        # A substring match (like the bot marker regex) would be wrong here; exact
        # set-membership leaves the real employee untouched.
        rows = [
            _people_row(email="jordan.lake@acme.example",
                        identities=[("oidc", "jordan.lake@acme.example")],
                        name="Jordan Lake"),
            _people_row(email="gardenio.support@acme.example",
                        identities=[("oidc", "gardenio.support@acme.example")],
                        name="Gardenio Support Person"),
        ]
        got = self._count(rows)
        self.assertEqual(got["service_rows_rendered"], 0)
        self.assertEqual(got["service_rows_sample"], [])

    def test_mixed_page_counts_only_service_rows(self):
        # The real /people shape: a majority of humans plus the service identities.
        # Result is the ROW count, and human rows (incl. one quoting a service
        # subject in a bead title) are untouched.
        humans = [
            _people_row(email=f"human{i}@acme.example",
                        identities=[("oidc", f"human{i}@acme.example")],
                        name=f"Human {i}")
            for i in range(20)
        ]
        humans.append(
            _people_row(
                email="ada.lovelace@acme.example",
                identities=[("oidc", "ada.lovelace@acme.example")],
                name="Ada Lovelace",
                extra_body='<span class="people-request-text">'
                           "why is gardenio@acme.example in the directory?</span>",
            )
        )
        service = [
            _people_row(email="gardenio@acme.example",
                        identities=[("oidc", "gardenio@acme.example")]),
            _people_row(email="info-density-audit@acme.example",
                        identities=[("oidc", "info-density-audit@acme.example")]),
        ]
        got = self._count(humans + service)
        self.assertEqual(got["service_rows_rendered"], 2)
        self.assertEqual(len(got["service_rows_sample"]),
                         len(set(got["service_rows_sample"])))  # deduped

    def test_human_content_mentioning_service_does_not_flag(self):
        # A real human whose recent-bead title / request preview quotes a service
        # subject must NOT flag: the classifier scans IDENTIFIERS, never the block.
        got = self._count([
            _people_row(
                email="sky.bennett@acme.example",
                identities=[("oidc", "sky.bennett@acme.example")],
                name="Sky Bennett",
                extra_body=(
                    '<a class="chip people-recent-bead" '
                    'title="deny-list gardenio@acme.example from /people">gd-abc</a>'
                    '<span class="people-request-text">is joel.approver@acme.example '
                    "still needed?</span>"
                ),
            ),
        ])
        self.assertEqual(got["service_rows_rendered"], 0)
        self.assertEqual(got["service_rows_sample"], [])

    def test_script_literals_do_not_create_phantom_service_rows(self):
        # A <script> quoting a people-row + a service subject in string literals
        # must not manufacture a row: index_signals runs people_service_rows over
        # the _body()-stripped DOM.
        page = _people_page([
            _people_row(email="jordan.lake@acme.example",
                        identities=[("oidc", "jordan.lake@acme.example")],
                        name="Jordan Lake"),
        ]) + (
            '<script>var x=\'<li class="people-row">'
            '<a href="mailto:gardenio@acme.example">x</a></li>\';</script>'
        )
        got = _mod.people_service_rows(_mod._body(page))
        self.assertEqual(got["service_rows_rendered"], 0)

    def test_sample_is_capped(self):
        # The sample is capped at BOT_ROW_SAMPLE_CAP even if (implausibly) every
        # deny-list subject renders many times — a page can never emit an unbounded
        # blob. Duplicate the four subjects across many rows to exceed the cap.
        subs = sorted(_mod.SERVICE_ROW_SUBJECTS)
        rows = [
            _people_row(email=subs[i % len(subs)],
                        identities=[("oidc", subs[i % len(subs)])])
            for i in range(_mod.BOT_ROW_SAMPLE_CAP + 15)
        ]
        got = self._count(rows)
        self.assertEqual(got["service_rows_rendered"], _mod.BOT_ROW_SAMPLE_CAP + 15)
        # only 4 distinct subjects exist, so the sample stays small AND capped
        self.assertLessEqual(len(got["service_rows_sample"]), _mod.BOT_ROW_SAMPLE_CAP)
        self.assertEqual(sorted(got["service_rows_sample"]), subs)

    def test_index_signals_surfaces_service_metric(self):
        # The wiring: index_signals must expose service_rows_rendered/-_sample so
        # main()'s coverage block and FAIL-FLAG can read them.
        sig = _mod.index_signals(_people_page([
            _people_row(email="gardenio@acme.example",
                        identities=[("oidc", "gardenio@acme.example")]),
            _people_row(email="real.human@acme.example",
                        identities=[("oidc", "real.human@acme.example")],
                        name="Real Human"),
        ]))
        self.assertEqual(sig["rendered_row_count"], 2)
        self.assertEqual(sig["service_rows_rendered"], 1)
        self.assertIn("gardenio@acme.example", sig["service_rows_sample"])
        # bot_rows_rendered is deliberately blind to the same row — proving the two
        # signals are independent (bot markers cannot reach a real-domain service
        # account, which is exactly why this companion signal exists).
        self.assertEqual(sig["bot_rows_rendered"], 0)


# --- self/owner variant (bug gd-zFNvLriG4D) -----------------------------------
def _detail_self(*, you_badge, status_button):
    """Minimal /u/{slug} DOM carrying the IsViewerSelf-gated owner affordances.

    ``you_badge`` renders the "(you)" self-badge in the <h1>; ``status_button``
    is None (no editor — peer view), "set" (empty-status owner editor), or
    "edit" (present-status owner editor). Mirrors the real markup in
    user_home.html / user_status_message_block.html so the regexes exercise the
    shapes they will actually see.
    """
    h1 = '<h1>Matt Baker'
    if you_badge:
        h1 += ' <span class="meta">(you)</span>'
    h1 += "</h1>"
    block = '<div id="user-status-message" class="user-status-message">'
    if status_button == "set":
        block += (
            '<button type="button" class="user-status-message-set-button" '
            'hx-get="/u/matt/status-message/edit">Set a status message</button>'
        )
    elif status_button == "edit":
        block += (
            '<span class="user-status-message-value">hi</span>'
            '<button type="button" class="user-status-message-edit-button" '
            'hx-get="/u/matt/status-message/edit">Edit</button>'
        )
    block += "</div>"
    return (
        h1 + block + '<section id="activity" class="user-home-section is-filled">'
        '<h2>Activity <span class="meta">(1)</span></h2>'
        '<ul class="activity-stream"><li class="bead-row activity-row">x</li></ul>'
        "</section>"
    )


class SelfVariantSignals(unittest.TestCase):
    """Regression guard for bug gd-zFNvLriG4D: the owner (IsViewerSelf=true)
    variant of /u/{slug} was never graded — every CAMP iteration captured under
    dev-auth-bypass with no viewer identity, so the "(you)" badge + self-only
    status editor never rendered. The self-variant capture ('user-home-self',
    owner email injected on X-Auth-Request-Email) plus these signals close it.
    """

    def test_healthy_self_vs_peer(self):
        # Owner affordances render in self, none leak to peer ⇒ variant OK.
        self_html = _detail_self(you_badge=True, status_button="set")
        peer_html = _detail_self(you_badge=False, status_button=None)
        sig = _mod.self_variant_signals(self_html, peer_html)
        self.assertEqual(sig["self_you_badge"], 1)
        self.assertEqual(sig["self_status_edit_button"], 1)
        self.assertEqual(sig["self_affordance_count"], 2)
        self.assertEqual(sig["self_affordances_leaked_to_peer"], 0)
        self.assertTrue(sig["self_variant_ok"])

    def test_present_status_uses_edit_button(self):
        # A profile with an existing status renders the "edit" (not "set")
        # button; both variants must count as an owner affordance.
        self_html = _detail_self(you_badge=True, status_button="edit")
        peer_html = _detail_self(you_badge=False, status_button=None)
        sig = _mod.self_variant_signals(self_html, peer_html)
        self.assertEqual(sig["self_status_edit_button"], 1)
        self.assertTrue(sig["self_variant_ok"])

    def test_missing_self_capture_is_null_not_fail(self):
        # No self capture ⇒ None (variant ungraded this run), never a
        # zero-affordance fail. This is what keeps a missing capture a WARN.
        self.assertIsNone(_mod.self_variant_signals("", "<h1>peer</h1>"))

    def test_self_capture_with_no_affordances_flags(self):
        # The self capture rendered but carries NO owner affordance — the owner
        # path never resolved (IsViewerSelf never went true). Count is 0 and the
        # variant is NOT ok, so main()'s FAIL-FLAG fires.
        self_html = _detail_self(you_badge=False, status_button=None)
        peer_html = _detail_self(you_badge=False, status_button=None)
        sig = _mod.self_variant_signals(self_html, peer_html)
        self.assertEqual(sig["self_affordance_count"], 0)
        self.assertFalse(sig["self_variant_ok"])

    def test_owner_affordance_leaked_to_peer_flags(self):
        # An owner-only control renders for a NON-owner — a UI authorization
        # leak. self_variant_ok must be False even though the self capture is
        # fine, because the peer capture carries the affordance.
        self_html = _detail_self(you_badge=True, status_button="set")
        peer_html = _detail_self(you_badge=True, status_button="edit")
        sig = _mod.self_variant_signals(self_html, peer_html)
        self.assertEqual(sig["self_affordances_leaked_to_peer"], 2)
        self.assertFalse(sig["self_variant_ok"])

    def test_you_badge_in_script_literal_not_counted(self):
        # A JS string literal mentioning "(you)" must not count as a rendered
        # badge — _body() strips <script> before the regex runs.
        self_html = (
            _detail_self(you_badge=False, status_button=None)
            + '<script>var t="<span class=\\"meta\\">(you)</span>";</script>'
        )
        sig = _mod.self_variant_signals(self_html, "")
        self.assertEqual(sig["self_you_badge"], 0)


def _card(name, slug, email=""):
    """One /people card whose name anchor heads with ``name`` and links /u/{slug}.

    Mirrors people.html's ``people-row`` shape: the ``people-row-name`` anchor
    carries BOTH the class and the ``/u/{slug}`` href on one opening tag (the
    exact tag ``people_card_identities`` splits the heading + slug out of).
    """
    parts = [
        '<li class="bead-row people-row" data-operator-id="1">'
        '<div class="bead-row-top"><div class="bead-row-text">',
        f'<a class="bead-row-title people-row-name" href="/u/{slug}" '
        f'hx-get="/x" hx-trigger="mouseover">{name}</a>',
    ]
    if email:
        parts.append(
            f'<div class="people-row-email"><a href="mailto:{email}">{email}</a></div>'
        )
    parts.append("</div></div></li>")
    return "".join(parts)


def _index(cards):
    return (
        '<h1>Users <span class="meta">(N)</span></h1>'
        '<ul class="bead-list people-list">' + "".join(cards) + "</ul>"
    )


def _detail(h1_inner):
    """A minimal /u/{slug} page whose <h1> renders ``h1_inner`` verbatim.

    ``h1_inner`` may include the self ``(you)`` badge span; ``detail_h1_text``
    must strip it so a self-render never reads as a mismatch.
    """
    return f'<html><body><h1>{h1_inner}</h1><section id="activity"></section></body></html>'


class CrossSurfaceIdentity(unittest.TestCase):
    """Regression guard for bug ka-0000006552 / gd-VJy99tfmnF: the two endpoints
    of a /people→/u/{slug} link must AGREE about who they describe.

    The other three integrity ratios ask "does the link resolve / is the route
    reachable / do the facet counts match" — never "do the card heading and the
    detail <h1> name the SAME person". Iteration 2026-07-28T0226 scored all three
    1.0-clean and STILL rendered `Matthew.Baker@acme.example` as the card heading
    where /u/matthew.baker rendered `Matt Baker`. These pin the fourth ratio +
    its FAIL-FLAG. Hermetic — the detail fetch is an injected dict lookup, no net.
    """

    @staticmethod
    def _fetcher(by_slug):
        """A fetch(href)->html that resolves /u/{slug} against ``by_slug``.

        A slug absent from the map returns None ⇒ the real fetcher's "detail page
        unfetchable" path (UNVERIFIABLE, excluded from the ratio).
        """
        def fetch(href):
            m = _mod.SLUG_IN_HREF_RE.search(href)
            if not m:
                return None
            return by_slug.get(m.group(1))
        return fetch

    def test_agreeing_names_score_1(self):
        idx = _index([_card("Matt Baker", "matthew.baker"),
                      _card("Devin Hale", "devin.hale")])
        by_slug = {"matthew.baker": _detail("Matt Baker"),
                   "devin.hale": _detail("Devin Hale")}
        w = []
        r = _mod.cross_surface_identity(idx, self._fetcher(by_slug), 60, w)
        self.assertEqual(r["cards_with_slug_link"], 2)
        self.assertEqual(r["cards_compared"], 2)
        self.assertEqual(r["matching_cards"], 2)
        self.assertEqual(r["identity_mismatches"], [])
        self.assertEqual(r["cross_surface_identity_match_ratio"], 1.0)

    def test_email_heading_vs_resolved_name_is_a_mismatch(self):
        # THE motivating defect: card heads with a raw email, detail heads with
        # the resolved display name. Must NOT normalize the email to its local
        # part (that would make it match "Matt Baker" — going blind to the defect).
        idx = _index([_card("Matthew.Baker@acme.example", "matthew.baker")])
        by_slug = {"matthew.baker": _detail("Matt Baker")}
        w = []
        r = _mod.cross_surface_identity(idx, self._fetcher(by_slug), 60, w)
        self.assertEqual(r["cards_compared"], 1)
        self.assertEqual(r["matching_cards"], 0)
        self.assertEqual(r["cross_surface_identity_match_ratio"], 0.0)
        self.assertEqual(len(r["identity_mismatches"]), 1)
        mm = r["identity_mismatches"][0]
        self.assertEqual(mm["card_heading"], "Matthew.Baker@acme.example")
        self.assertEqual(mm["slug"], "matthew.baker")
        self.assertEqual(mm["detail_h1"], "Matt Baker")

    def test_ratio_over_compared_set_with_a_mismatch(self):
        idx = _index([_card("Matt Baker", "matthew.baker"),
                      _card("devin.hale@contractor-c.example", "devin.hale"),
                      _card("Ada Lovelace", "ada.lovelace")])
        by_slug = {"matthew.baker": _detail("Matt Baker"),
                   "devin.hale": _detail("Devin Hale"),
                   "ada.lovelace": _detail("Ada Lovelace")}
        w = []
        r = _mod.cross_surface_identity(idx, self._fetcher(by_slug), 60, w)
        self.assertEqual(r["cards_compared"], 3)
        self.assertEqual(r["matching_cards"], 2)
        self.assertEqual(r["cross_surface_identity_match_ratio"], round(2 / 3, 4))
        self.assertEqual([m["slug"] for m in r["identity_mismatches"]], ["devin.hale"])

    def test_casefold_and_whitespace_normalized_not_a_mismatch(self):
        # Trim + collapse-whitespace + casefold is the sanctioned normalization:
        # a heading that differs only in case/spacing is NOT a real divergence.
        idx = _index([_card("Matt   Baker", "matthew.baker")])
        by_slug = {"matthew.baker": _detail("matt baker")}
        w = []
        r = _mod.cross_surface_identity(idx, self._fetcher(by_slug), 60, w)
        self.assertEqual(r["matching_cards"], 1)
        self.assertEqual(r["cross_surface_identity_match_ratio"], 1.0)

    def test_self_you_badge_stripped_before_compare(self):
        # The detail <h1> of a self-render carries the "(you)" meta badge; it must
        # be stripped so a self capture never manufactures a phantom mismatch.
        idx = _index([_card("Matt Baker", "matthew.baker")])
        by_slug = {"matthew.baker": _detail(
            'Matt Baker <span class="meta">(you)</span>')}
        w = []
        r = _mod.cross_surface_identity(idx, self._fetcher(by_slug), 60, w)
        self.assertEqual(r["matching_cards"], 1)
        self.assertEqual(r["cross_surface_identity_match_ratio"], 1.0)

    def test_unfetchable_detail_is_unverifiable_not_a_mismatch(self):
        # A slug whose detail page can't be fetched/parsed is excluded from the
        # ratio (UNVERIFIABLE), never counted as a mismatch — matches the other
        # ratios' "null == unmeasurable, not a zero-population fail" convention.
        idx = _index([_card("Matt Baker", "matthew.baker"),
                      _card("Gone Person", "gone.person")])
        by_slug = {"matthew.baker": _detail("Matt Baker")}  # gone.person absent
        w = []
        r = _mod.cross_surface_identity(idx, self._fetcher(by_slug), 60, w)
        self.assertEqual(r["cards_compared"], 1)
        self.assertEqual(r["matching_cards"], 1)
        self.assertEqual(r["unverifiable_slugs"], ["gone.person"])
        self.assertEqual(r["cross_surface_identity_match_ratio"], 1.0)
        self.assertTrue(any("unfetchable" in x.lower() for x in w))

    def test_total_fetch_outage_is_null_not_zero(self):
        # Every detail fetch fails ⇒ nothing compared ⇒ ratio is null, NOT 0.0.
        idx = _index([_card("Matt Baker", "matthew.baker")])
        w = []
        r = _mod.cross_surface_identity(idx, self._fetcher({}), 60, w)
        self.assertEqual(r["cards_compared"], 0)
        self.assertIsNone(r["cross_surface_identity_match_ratio"])
        self.assertEqual(r["unverifiable_slugs"], ["matthew.baker"])

    def test_h1_missing_is_unverifiable(self):
        # A detail page with no <h1> (auth wall / non-render) is UNVERIFIABLE.
        idx = _index([_card("Matt Baker", "matthew.baker")])
        by_slug = {"matthew.baker": "<html><body><p>login required</p></body></html>"}
        w = []
        r = _mod.cross_surface_identity(idx, self._fetcher(by_slug), 60, w)
        self.assertEqual(r["cards_compared"], 0)
        self.assertIsNone(r["cross_surface_identity_match_ratio"])
        self.assertEqual(r["unverifiable_slugs"], ["matthew.baker"])

    def test_cards_without_slug_link_are_skipped(self):
        # A name anchor that is not a /u/{slug} has no detail page to agree with,
        # so it is not counted in the denominator.
        no_slug = (
            '<li class="bead-row people-row"><div class="bead-row-top">'
            '<div class="bead-row-text">'
            '<a class="bead-row-title people-row-name" href="/search?q=x">Ghost</a>'
            "</div></div></li>"
        )
        idx = _index([_card("Matt Baker", "matthew.baker"), no_slug])
        by_slug = {"matthew.baker": _detail("Matt Baker")}
        w = []
        r = _mod.cross_surface_identity(idx, self._fetcher(by_slug), 60, w)
        self.assertEqual(r["cards_with_slug_link"], 1)
        self.assertEqual(r["cards_compared"], 1)

    def test_fan_out_cap_records_overflow(self):
        cards = [_card(f"Human {i}", f"human.{i}") for i in range(5)]
        idx = _index(cards)
        by_slug = {f"human.{i}": _detail(f"Human {i}") for i in range(5)}
        w = []
        r = _mod.cross_surface_identity(idx, self._fetcher(by_slug), 2, w)
        self.assertEqual(r["cards_with_slug_link"], 5)
        self.assertEqual(r["cards_compared"], 2)  # only the first 2 fetched
        self.assertEqual(r["cards_capped"], 3)
        self.assertEqual(r["cross_surface_identity_match_ratio"], 1.0)
        self.assertTrue(any("cap" in x.lower() for x in w))

    def test_duplicate_slug_fetched_once(self):
        # Two cards linking the same slug reuse one detail fetch (the per-slug
        # cache), and both are compared against that one <h1>.
        fetches = {"n": 0}
        def counting_fetch(href):
            fetches["n"] += 1
            return _detail("Matt Baker")
        idx = _index([_card("Matt Baker", "matthew.baker"),
                      _card("Matt Baker", "matthew.baker")])
        w = []
        r = _mod.cross_surface_identity(idx, counting_fetch, 60, w)
        self.assertEqual(fetches["n"], 1)
        self.assertEqual(r["cards_compared"], 2)
        self.assertEqual(r["matching_cards"], 2)

    def test_empty_roster_is_null_not_zero(self):
        r = _mod.cross_surface_identity(_index([]), self._fetcher({}), 60, [])
        self.assertEqual(r["cards_with_slug_link"], 0)
        self.assertIsNone(r["cross_surface_identity_match_ratio"])

    def test_script_literal_slug_link_not_a_phantom_card(self):
        # A /u/{slug} inside a <script> string must not manufacture a card —
        # _body() strips <script> before the row split runs.
        idx = (
            _index([_card("Matt Baker", "matthew.baker")])
            + '<script>var h="<a class=\\"people-row-name\\" href=\\"/u/ghost\\">G</a>";</script>'
        )
        by_slug = {"matthew.baker": _detail("Matt Baker")}
        r = _mod.cross_surface_identity(idx, self._fetcher(by_slug), 60, [])
        self.assertEqual(r["cards_with_slug_link"], 1)


class DetailH1Text(unittest.TestCase):
    """Pin the detail <h1> extraction the cross-surface ratio compares against."""

    def test_plain_h1(self):
        self.assertEqual(_mod.detail_h1_text(_detail("Matt Baker")), "Matt Baker")

    def test_you_badge_stripped(self):
        self.assertEqual(
            _mod.detail_h1_text(_detail('Ada <span class="meta">(you)</span>')),
            "Ada",
        )

    def test_no_h1_is_none(self):
        self.assertIsNone(_mod.detail_h1_text("<html><body>no heading</body></html>"))

    def test_whitespace_collapsed(self):
        self.assertEqual(
            _mod.detail_h1_text("<h1>  Matt   Baker  </h1>"), "Matt Baker"
        )


# --- tile facet integrity (static) + click outcome (bug gd-XVZNaYe9nZ) --------
def _stat_tile(count, label, facet=None, *, link=True):
    """One <li class="user-home-stat"> mirroring user_home.html.

    ``facet`` is the data-facet-link value; when ``link`` is False the tile
    renders WITHOUT the <a data-facet-link> (an inert tile — a candidate product
    fix for ka-0000003914 drops the link on zero tiles). ``count`` is the printed
    figure; a 0 count also stamps the .is-zero class the template emits.
    """
    zero = " is-zero" if count == 0 else ""
    inner = (
        f'<span class="user-home-stat-num">{count}</span>'
        f'<span class="user-home-stat-label">{label}</span>'
    )
    if link:
        fa = f' data-facet-link="{facet}"' if facet else ""
        body = f'<a href="#activity"{fa}>{inner}</a>'
    else:
        body = f"<span>{inner}</span>"
    return f'<li class="user-home-stat{zero}">{body}</li>'


def _detail_with_tiles(tiles, chip_facets=()):
    """A /u/{slug} DOM: a summary band of ``tiles`` + a facet chip strip.

    ``chip_facets`` is an iterable of (slug, count) chips the activity spine
    renders (the "All" chip is always present). Enough shape for the static
    facet_integrity scan; the click OUTCOME is measured by the browser driver,
    not from this DOM.
    """
    chips = ['<button class="activity-facet" data-facet="">All</button>']
    for slug, n in chip_facets:
        chips.append(
            f'<button class="activity-facet" data-facet="{slug}">'
            f'X <span class="activity-facet-n">{n}</span></button>'
        )
    return (
        '<div class="user-home-summary"><ul class="user-home-stats">'
        + "".join(tiles)
        + "</ul></div>"
        + '<section id="activity"><div class="activity-facets" '
        + 'data-activity-facets>'
        + "".join(chips)
        + "</div></section>"
    )


class FacetIntegrityStatic(unittest.TestCase):
    """The STATIC tile scan: descriptive counts + the duplicate-facet-key
    detector (ka-0000008262), and the deliberate absence of a false 1.0 ratio.

    The old facet_match_ratio lived here and excused zero tiles + checked only
    chip existence, so it scored 1.0 on a page broken two ways. This function is
    now descriptive only; the gate moved to the click-outcome driver.
    """

    def test_no_facet_match_ratio_key_here(self):
        # The false-1.0 ratio must be GONE from the static block — it now lives
        # on the click-outcome reader, where it means the real outcome.
        sig = _mod.facet_integrity(_detail_with_tiles(
            [_stat_tile(0, "Merged MRs", "mrs")], chip_facets=[]))
        self.assertNotIn("facet_match_ratio", sig)
        self.assertNotIn("unmatched_tile_facets", sig)

    def test_duplicate_facet_key_detected(self):
        # THE ka-0000008262 shape: two tiles share the `mrs` key. Static, no
        # browser — flags on the DOM alone, naming both tiles a human sees.
        sig = _mod.facet_integrity(_detail_with_tiles(
            [_stat_tile(0, "Merged MRs", "mrs"),
             _stat_tile(2, "Open MRs", "mrs")],
            chip_facets=[("mrs", 2)]))
        self.assertEqual(len(sig["duplicate_tile_facet_keys"]), 1)
        dk = sig["duplicate_tile_facet_keys"][0]
        self.assertEqual(dk["facet"], "mrs")
        self.assertEqual(sorted(dk["labels"]), ["Merged MRs", "Open MRs"])

    def test_distinct_facets_no_duplicate(self):
        sig = _mod.facet_integrity(_detail_with_tiles(
            [_stat_tile(2, "Open MRs", "mrs"),
             _stat_tile(1, "Needs you", "decisions")],
            chip_facets=[("mrs", 2), ("decisions", 1)]))
        self.assertEqual(sig["duplicate_tile_facet_keys"], [])

    def test_zero_count_tile_is_still_a_facet_link(self):
        # A zero tile that STILL carries the facet link is in scope (it is
        # clickable and can silently unfilter). The count is recorded so a fix
        # that makes it inert (link=False) is visible as a drop here.
        sig = _mod.facet_integrity(_detail_with_tiles(
            [_stat_tile(0, "Active agents", "agents")], chip_facets=[]))
        self.assertEqual(sig["tile_facet_links"], 1)
        self.assertEqual(sig["zero_count_tile_facet_links"], 1)
        self.assertEqual(sig["inert_tiles"], 0)

    def test_inert_zero_tile_counted_as_inert(self):
        # Candidate fix (a): a zero tile renders WITHOUT the <a data-facet-link>.
        # It is inert (cannot lie about a filter) and counts as such.
        sig = _mod.facet_integrity(_detail_with_tiles(
            [_stat_tile(0, "Active agents", "agents", link=False)],
            chip_facets=[]))
        self.assertEqual(sig["tile_count"], 1)
        self.assertEqual(sig["tile_facet_links"], 0)
        self.assertEqual(sig["inert_tiles"], 1)

    def test_missing_chip_is_descriptive_not_a_gate(self):
        # A zero tile whose facet renders no chip is the EXPECTED healthy DOM —
        # recorded for context, but there is no ratio penalty here (the defect is
        # what the CLICK does, which this static scan cannot see).
        sig = _mod.facet_integrity(_detail_with_tiles(
            [_stat_tile(0, "Active agents", "agents")], chip_facets=[]))
        self.assertIn("agents", sig["tiles_missing_facet_chip"])
        self.assertNotIn("facet_match_ratio", sig)

    def test_script_literal_tile_not_counted(self):
        # A <script> literal mentioning user-home-stat / data-facet-link must not
        # manufacture a tile — _body() strips <script> first.
        dom = _detail_with_tiles(
            [_stat_tile(2, "Open MRs", "mrs")], chip_facets=[("mrs", 2)]
        ) + (
            '<script>var t=\'<li class="user-home-stat">'
            '<a data-facet-link="ghost">x</a></li>\';</script>'
        )
        sig = _mod.facet_integrity(dom)
        self.assertEqual(sig["tile_count"], 1)


class TileClickOutcomes(unittest.TestCase):
    """The CLICK-OUTCOME reader (bug gd-XVZNaYe9nZ): folds the Playwright
    driver's JSON into tile_click_outcome_ratio + the silent-unfilter signal.

    The ratio is over ALL rendered tiles (zero-count IN scope), scored against
    the FACET population the click targets — never the tile's own count — so a
    legitimate mrs-fold / dedup page reads clean while the silent-unfilter defect
    reds it. Hermetic: writes a temp JSON, no browser.
    """

    def setUp(self):
        import tempfile
        self._dir = tempfile.mkdtemp()

    def tearDown(self):
        import shutil
        shutil.rmtree(self._dir, ignore_errors=True)

    def _write(self, obj):
        from pathlib import Path as _P
        p = _P(self._dir) / "tile-click-outcomes.json"
        p.write_text(__import__("json").dumps(obj))
        return p

    def _tile(self, **kw):
        base = {
            "slug": "ken", "label": "Open MRs", "tile_count": 2,
            "facet_link": "mrs", "clickable": True, "clicked": True,
            "visible_after_click": 2, "total": 3, "nomatch": False,
            "facet_population": 2, "outcome_ok": True,
            "silently_unfilters": False,
        }
        base.update(kw)
        return base

    def test_missing_file_is_null_warn_not_fail(self):
        from pathlib import Path as _P
        w = []
        r = _mod.tile_click_outcomes(_P(self._dir) / "nope.json", w)
        self.assertIsNone(r)
        self.assertTrue(any("UNMEASURED" in x for x in w))

    def test_all_tiles_ok_ratio_1(self):
        p = self._write({"slugs": ["ken"], "tiles": [
            self._tile(label="Open MRs", facet_population=2,
                       visible_after_click=2),
            # A zero tile whose click correctly shows NOTHING (facet empty).
            self._tile(label="Active agents", facet_link="agents",
                       tile_count=0, facet_population=0,
                       visible_after_click=0, nomatch=True),
        ]})
        w = []
        r = _mod.tile_click_outcomes(p, w)
        self.assertEqual(r["tile_click_outcome_ratio"], 1.0)
        self.assertEqual(r["facet_match_ratio"], 1.0)  # backward-compat alias
        self.assertEqual(r["tiles_silently_unfiltering"], 0)
        self.assertEqual(r["tiles_measured"], 2)

    def test_silent_unfilter_reds_the_ratio(self):
        # THE ka-0000003914 shape: a zero tile's click reveals the WHOLE stream.
        p = self._write({"slugs": ["ken"], "tiles": [
            self._tile(label="Open MRs", facet_population=2,
                       visible_after_click=2, outcome_ok=True),
            self._tile(label="Active agents", facet_link="agents",
                       tile_count=0, facet_population=0, total=3,
                       visible_after_click=3, nomatch=False,
                       outcome_ok=False, silently_unfilters=True),
        ]})
        w = []
        r = _mod.tile_click_outcomes(p, w)
        self.assertEqual(r["tile_click_outcome_ratio"], 0.5)
        self.assertEqual(r["tiles_silently_unfiltering"], 1)
        self.assertEqual(r["tiles_outcome_mismatch"], 1)
        self.assertEqual(r["unmatched_tile_facets"], ["agents"])
        self.assertEqual(len(r["tiles_silently_unfiltering_sample"]), 1)
        self.assertEqual(
            r["tiles_silently_unfiltering_sample"][0]["label"], "Active agents")

    def test_inert_zero_tile_counts_as_ok(self):
        # A fix that made the zero tile inert (not clickable) is OK — it makes no
        # filter promise. The ratio stays 1.0.
        p = self._write({"slugs": ["ken"], "tiles": [
            self._tile(label="Active agents", facet_link=None,
                       tile_count=0, clickable=False, clicked=False,
                       visible_after_click=None, facet_population=None,
                       outcome_ok=True, silently_unfilters=False),
        ]})
        r = _mod.tile_click_outcomes(p, [])
        self.assertEqual(r["tile_click_outcome_ratio"], 1.0)
        self.assertEqual(r["clickable_tiles"], 0)
        self.assertEqual(r["tiles_silently_unfiltering"], 0)

    def test_wrong_nonzero_count_is_a_mismatch_not_silent(self):
        # A click that filters to the WRONG non-zero count (not the whole
        # stream) is an outcome mismatch but not a "silent unfilter".
        p = self._write({"slugs": ["ken"], "tiles": [
            self._tile(label="Open MRs", facet_population=2,
                       visible_after_click=1, outcome_ok=False,
                       silently_unfilters=False),
        ]})
        r = _mod.tile_click_outcomes(p, [])
        self.assertEqual(r["tile_click_outcome_ratio"], 0.0)
        self.assertEqual(r["tiles_silently_unfiltering"], 0)
        self.assertEqual(r["tiles_outcome_mismatch"], 1)

    def test_empty_tiles_ratio_null(self):
        # No tiles measured ⇒ null ratio (unmeasurable), never 0.0.
        p = self._write({"slugs": [], "tiles": []})
        r = _mod.tile_click_outcomes(p, [])
        self.assertIsNone(r["tile_click_outcome_ratio"])

    def test_duplicate_keys_and_errors_passed_through(self):
        p = self._write({
            "slugs": ["ken"],
            "tiles": [self._tile()],
            "duplicate_tile_facet_keys": [
                {"slug": "ken", "facet": "mrs",
                 "labels": ["Merged MRs", "Open MRs"]}],
            "errors": ["boom"],
        })
        r = _mod.tile_click_outcomes(p, [])
        self.assertEqual(len(r["duplicate_tile_facet_keys"]), 1)
        self.assertEqual(r["driver_errors"], ["boom"])

    def test_unreadable_json_is_null_warn(self):
        from pathlib import Path as _P
        p = _P(self._dir) / "tile-click-outcomes.json"
        p.write_text("{ not json")
        w = []
        r = _mod.tile_click_outcomes(p, w)
        self.assertIsNone(r)
        self.assertTrue(any("unreadable" in x for x in w))


def _request_snippet(text):
    """One .people-request-text snippet as people.html renders it.

    ``text`` is inserted verbatim, so callers pass the HTML-ESCAPED mention form
    (``&lt;@U…&gt;``) exactly as the captured DOM carries it — proving the scanner
    unescapes before matching, matching what the browser shows.
    """
    return (
        '<li class="people-request">'
        '<a class="people-request-link" href="/threads/gd-x">'
        f'<span class="people-request-text">{text}</span>'
        '<span class="meta people-request-ago">4h</span>'
        "</a></li>"
    )


def _people_page_with_requests(snippets):
    return _people_page([]) + '<ul class="people-requests">' + "".join(snippets) + "</ul>"


class RawSlackMentionLeak(unittest.TestCase):
    """Regression guard for bug ka-0000003119 (T1722): raw <@U…> mention leak.

    T1722 shipped raw Slack mention tokens as literal text in the /people
    "recent requests" snippets; MR 6264 routed those snippets through
    resolveSlackMentions. The gardener had been re-verifying the fix by a hand
    grep every iteration — these pins make it an INHERITED signal.

    The load-bearing subtlety: the viewer's resolver "never invents a name", so a
    token whose id is NOT in the alias feed (an external Slack user) is left raw
    BY DESIGN. Only a token whose id IS resolvable-yet-raw is the regression. A
    naive "any <@U…> ⇒ fail" would false-red on the external case; this classifier
    must split the two.
    """

    def test_resolved_snippet_reports_nothing(self):
        # A snippet the viewer already resolved to "@Name" carries no raw token.
        got = _mod.raw_slack_mentions(
            _people_page_with_requests([_request_snippet("@Lee Morgan welcome to the team!")]),
            {"U0PERSON001"},
        )
        self.assertEqual(got["raw_mention_tokens_total"], 0)
        self.assertEqual(got["raw_mentions_unresolved_resolvable"], 0)
        self.assertEqual(got["raw_mentions_unresolved_unknown"], 0)

    def test_resolvable_id_left_raw_is_the_regression(self):
        # The T1722 shape: an id the resolver KNOWS still rendered raw ⇒ flag.
        got = _mod.raw_slack_mentions(
            _people_page_with_requests(
                [_request_snippet("&lt;@U0PERSON001&gt; welcome to the team!")]
            ),
            {"U0PERSON001"},
        )
        self.assertEqual(got["raw_mention_tokens_total"], 1)
        self.assertEqual(got["raw_mentions_unresolved_resolvable"], 1)
        self.assertIn("U0PERSON001", got["raw_mentions_unresolved_resolvable_sample"])
        self.assertEqual(got["raw_mentions_unresolved_unknown"], 0)

    def test_unknown_external_id_left_raw_is_by_design(self):
        # An external Slack user not in the alias feed — the resolver correctly
        # leaves it raw. Counted as unknown (informational), NEVER as resolvable.
        got = _mod.raw_slack_mentions(
            _people_page_with_requests(
                [_request_snippet("&lt;@U0PERSON006&gt; @Lee Morgan I set these")]
            ),
            {"U0PERSON001", "U0OPERATOR2"},  # resolver knows other ids, not this one
        )
        self.assertEqual(got["raw_mention_tokens_total"], 1)
        self.assertEqual(got["raw_mentions_unresolved_resolvable"], 0)
        self.assertEqual(got["raw_mentions_unresolved_unknown"], 1)
        self.assertIn("U0PERSON006", got["raw_mentions_unresolved_unknown_sample"])

    def test_labeled_mention_form_uses_id_not_label(self):
        # Slack's `<@U123|display>` form: the id in group 1 decides resolvability,
        # the |label is ignored (mirrors slackMentionRe).
        got = _mod.raw_slack_mentions(
            _people_page_with_requests(
                [_request_snippet("&lt;@U0PERSON001|graham&gt; hi")]
            ),
            {"U0PERSON001"},
        )
        self.assertEqual(got["raw_mentions_unresolved_resolvable"], 1)

    def test_lowercase_placeholder_junk_is_left_untouched(self):
        # `<@Uxxx>` / `<@Uxxxx>` are templated placeholders, not real ids; the
        # UPPERCASE-only matcher must never touch them (mirrors the viewer).
        got = _mod.raw_slack_mentions(
            _people_page_with_requests(
                [_request_snippet("ping &lt;@Uxxxx&gt; about &lt;@Uxxx&gt;")]
            ),
            {"U0PERSON001"},
        )
        self.assertEqual(got["raw_mention_tokens_total"], 0)

    def test_only_request_snippets_are_scanned(self):
        # A raw token OUTSIDE a .people-request-text span (e.g. quoted in some
        # other field) is not the resolver's surface and must not be scanned.
        page = _people_page([]) + (
            '<div class="people-row-other">&lt;@U0PERSON001&gt; not a request</div>'
        )
        got = _mod.raw_slack_mentions(page, {"U0PERSON001"})
        self.assertEqual(got["raw_mention_tokens_total"], 0)

    def test_null_resolver_degrades_to_informational(self):
        # Alias feed unreachable ⇒ resolver_ids None ⇒ the split is unmeasurable;
        # every id buckets as unknown so no phantom regression is manufactured.
        got = _mod.raw_slack_mentions(
            _people_page_with_requests(
                [_request_snippet("&lt;@U0PERSON001&gt; welcome")]
            ),
            None,
        )
        self.assertFalse(got["resolver_available"])
        self.assertIsNone(got["resolver_id_count"])
        self.assertEqual(got["raw_mentions_unresolved_resolvable"], 0)
        self.assertEqual(got["raw_mentions_unresolved_unknown"], 1)

    def test_resolver_ids_skips_empty_display_sentinels(self):
        # slack_mention_resolver_ids mirrors buildSlackMentionResolver: only rows
        # with a non-empty display are resolvable. A sentinel row (empty display)
        # is skipped, so a token for it renders raw on the page too and must not
        # be judged a resolvable regression. This test pins the projection shape
        # directly (no network) by exercising the same keep/skip rule.
        aliases = [
            {"subject": "U0AA", "display": "Real Person", "email": "r@x.com"},
            {"subject": "U0BB", "display": "", "email": "slack-U0BB@noreply.kbeads.local"},
            {"subject": "  ", "display": "Nameless", "email": ""},
        ]
        # Reproduce the projection the fetcher applies to the feed body.
        ids = {
            (a.get("subject") or "").strip()
            for a in aliases
            if (a.get("subject") or "").strip() and (a.get("display") or "").strip()
        }
        self.assertEqual(ids, {"U0AA"})

    def test_dedup_and_sample_cap(self):
        # Repeated ids dedup (count is DISTINCT ids), and samples are capped.
        snippets = [
            _request_snippet(f"&lt;@U0RES{i:04d}&gt; hi") for i in range(_mod.BOT_ROW_SAMPLE_CAP + 5)
        ]
        # Same resolvable id twice must count once.
        snippets.append(_request_snippet("&lt;@U0RES0000&gt; again"))
        resolver = {f"U0RES{i:04d}" for i in range(_mod.BOT_ROW_SAMPLE_CAP + 5)}
        got = _mod.raw_slack_mentions(_people_page_with_requests(snippets), resolver)
        self.assertEqual(
            got["raw_mentions_unresolved_resolvable"], _mod.BOT_ROW_SAMPLE_CAP + 5
        )
        self.assertLessEqual(
            len(got["raw_mentions_unresolved_resolvable_sample"]), _mod.BOT_ROW_SAMPLE_CAP
        )

    def test_script_literals_do_not_create_phantom_tokens(self):
        # A <script> mentioning a request-text span with a raw token in a string
        # literal must not count — the scan runs over _body()-stripped DOM.
        page = _people_page_with_requests(
            [_request_snippet("@Real Name hi")]
        ) + (
            '<script>var x=\'<span class="people-request-text">'
            "&lt;@U0PERSON001&gt;</span>';</script>"
        )
        got = _mod.raw_slack_mentions(page, {"U0PERSON001"})
        self.assertEqual(got["raw_mention_tokens_total"], 0)


if __name__ == "__main__":
    unittest.main()
