// Plan composer clear-on-send regression coverage (ka-0000002657 — operator
// report: "the text I send doesn't always clear from the text input field").
//
// Root cause: the merged plan-agent card swaps as a UNIT on every
// `plan-agent-updated` SSE frame, and plan_events.js onAgentEvent snapshots the
// compose textarea BEFORE the swap and restores it after so a background frame
// never wipes an in-progress draft (ka-0000006168). On a SEND, the operator's
// turn is minted at send time, which fires a plan-agent-updated frame; if that
// frame's onAgentEvent runs while the composer still holds the just-sent text
// (before the POST response's own swap lands — and when it wins the race the
// POST swaps a now-detached node, so its clear is invisible), the restore
// re-fills the sent text and it "doesn't clear". The fix tracks the in-flight
// send and (a) never restores a draft equal to the in-flight text, (b) clears a
// resurrected value on success, and (c) re-fills on failure so nothing is lost.
//
// These tests reproduce the race deterministically at the browser layer:
//   - a FAKE EventSource (installed via addInitScript) lets the test emit a
//     plan-agent-updated frame at a precise moment, and
//   - a HELD POST route keeps the send in flight while that frame is emitted,
// so the exact SSE-vs-POST interleaving that caused the bug is forced, not
// hoped for. No daemon writes — the shared read fixture is safe.

import { test, expect } from '@playwright/test';

import { fixtureId, waitForHtmxIdle } from './_helpers.js';

// Operator identity — any non-empty email flips the viewer into operator mode
// under dev-bypass, which is required for the composer to render at all.
const OPERATOR_EMAIL = 'e2e-plan-clear@example.test';

// installFakeEventSource replaces window.EventSource with a controllable stub
// BEFORE any page script runs (defer'd plan_events.js constructs it on load,
// after a same-origin auth probe that returns 200 under dev-bypass). The stub
// records every instance so the test can emit events onto the plan stream
// (url matches /plans/{id}/events) regardless of other streams on the page.
async function installFakeEventSource(page) {
  await page.addInitScript(() => {
    window.__planESInstances = [];
    function FakeEventSource(url) {
      this.url = url;
      this._listeners = {};
      this.withCredentials = true;
      window.__planESInstances.push(this);
    }
    FakeEventSource.prototype.addEventListener = function (type, fn) {
      (this._listeners[type] = this._listeners[type] || []).push(fn);
    };
    FakeEventSource.prototype.removeEventListener = function () {};
    FakeEventSource.prototype.close = function () { this.closed = true; };
    FakeEventSource.prototype.__emit = function (type, data) {
      var ls = this._listeners[type] || [];
      for (var i = 0; i < ls.length; i++) ls[i]({ data: data });
    };
    window.__emitPlanFrame = function (type, data) {
      var es = window.__planESInstances.filter((e) => /\/plans\/.*\/events/.test(e.url)).pop();
      if (!es) throw new Error('no plan EventSource constructed yet');
      es.__emit(type, data);
      return true;
    };
    window.EventSource = FakeEventSource;
  });
}

// Wait until plan_events.js has finished its auth probe and constructed the
// plan EventSource, so __emitPlanFrame has an instance to emit onto.
async function waitForPlanStream(page) {
  await page.waitForFunction(
    () => (window.__planESInstances || []).some((e) => /\/plans\/.*\/events/.test(e.url)),
    null,
    { timeout: 5000 },
  );
}

test.describe('plan composer clear-on-send (ka-0000002657)', () => {
  test.use({ extraHTTPHeaders: { 'X-Auth-Request-Email': OPERATOR_EMAIL } });

  test('a racing plan-agent-updated frame during an in-flight send does not resurrect the sent text', async ({
    page,
  }) => {
    await installFakeEventSource(page);
    const planId = fixtureId('plan_epic');
    await page.goto(`/plans/${planId}`);
    await waitForHtmxIdle(page);
    await waitForPlanStream(page);

    // Capture the server's empty-composer render of #plan-agent — this IS what a
    // plan-agent-updated frame carries (the server never echoes an in-progress
    // draft), and it doubles as the POST response body below.
    const frameHTML = await page.locator('section#plan-agent').evaluate((el) => el.outerHTML);

    const input = page.locator('section#plan-agent form.plan-assistant-form textarea[name="message"]');
    await expect(input).toBeVisible();
    const message = 'e2e clear-on-send: this must vanish after Send';
    await input.fill(message);

    // Hold the POST so the send stays IN FLIGHT while we emit the racing frame.
    let releasePost;
    const postGate = new Promise((resolve) => {
      releasePost = resolve;
    });
    await page.route('**/assistant/prompt', async (route) => {
      if (route.request().method() !== 'POST') return route.fallback();
      await postGate;
      await route.fulfill({ status: 200, contentType: 'text/html; charset=utf-8', body: frameHTML });
    });

    // Fire the send; wait until the request is actually issued so htmx's
    // beforeRequest (which records the in-flight text) has run. waitForRequest's
    // predicate receives a Request, so call r.method() directly (not r.request()).
    const reqPromise = page.waitForRequest(
      (r) => r.url().includes('/assistant/prompt') && r.method() === 'POST',
    );
    await page.locator('section#plan-agent form.plan-assistant-form button[type="submit"]').click();
    await reqPromise;

    // Emit the mint-driven frame WHILE the send is in flight. Pre-fix, this
    // snapshotted the still-present sent text and restored it into the swapped-in
    // empty composer — the bug. Post-fix, the in-flight guard suppresses it.
    await page.evaluate(
      ([type, data]) => window.__emitPlanFrame(type, data),
      ['plan-agent-updated', frameHTML],
    );

    // The composer must be empty immediately after the racing frame, even before
    // the POST resolves — the restore was suppressed.
    await expect(input).toHaveValue('');

    // Release the POST; the composer must STAY empty on success.
    releasePost();
    await page.waitForResponse(
      (r) => r.url().includes('/assistant/prompt') && r.request().method() === 'POST',
    );
    await expect(
      page.locator('section#plan-agent form.plan-assistant-form textarea[name="message"]'),
    ).toHaveValue('');
  });

  test('a failed send preserves the operator message even if a racing frame emptied the composer', async ({
    page,
  }) => {
    await installFakeEventSource(page);
    const planId = fixtureId('plan_epic');
    await page.goto(`/plans/${planId}`);
    await waitForHtmxIdle(page);
    await waitForPlanStream(page);

    const frameHTML = await page.locator('section#plan-agent').evaluate((el) => el.outerHTML);
    const input = page.locator('section#plan-agent form.plan-assistant-form textarea[name="message"]');
    await expect(input).toBeVisible();
    const message = 'e2e failed-send: must be preserved, never lost';
    await input.fill(message);

    // Hold the POST, then fail it with a non-2xx (htmx does not swap 4xx/5xx —
    // the textarea-preservation contract, R1).
    let releasePost;
    const postGate = new Promise((resolve) => {
      releasePost = resolve;
    });
    await page.route('**/assistant/prompt', async (route) => {
      if (route.request().method() !== 'POST') return route.fallback();
      await postGate;
      await route.fulfill({
        status: 502,
        contentType: 'text/plain; charset=utf-8',
        body: 'Failed to start the plan assistant.',
      });
    });

    const reqPromise = page.waitForRequest(
      (r) => r.url().includes('/assistant/prompt') && r.method() === 'POST',
    );
    await page.locator('section#plan-agent form.plan-assistant-form button[type="submit"]').click();
    await reqPromise;

    // A racing frame during flight swaps the card and (with restore suppressed)
    // leaves the composer empty — the message is momentarily only in the
    // in-flight buffer.
    await page.evaluate(
      ([type, data]) => window.__emitPlanFrame(type, data),
      ['plan-agent-updated', frameHTML],
    );
    await expect(input).toHaveValue('');

    // On failure, afterRequest must put the message back so it is never lost.
    releasePost();
    await page.waitForResponse(
      (r) => r.url().includes('/assistant/prompt') && r.request().method() === 'POST',
    );
    await expect(
      page.locator('section#plan-agent form.plan-assistant-form textarea[name="message"]'),
    ).toHaveValue(message);
  });

  test('a background frame still restores an unsent draft (no regression of ka-0000006168)', async ({
    page,
  }) => {
    await installFakeEventSource(page);
    const planId = fixtureId('plan_epic');
    await page.goto(`/plans/${planId}`);
    await waitForHtmxIdle(page);
    await waitForPlanStream(page);

    const frameHTML = await page.locator('section#plan-agent').evaluate((el) => el.outerHTML);
    const input = page.locator('section#plan-agent form.plan-assistant-form textarea[name="message"]');
    await expect(input).toBeVisible();

    // Type a draft but do NOT send. A background plan-agent-updated frame (a new
    // transcript turn, a typing-bubble flip) must preserve the in-progress draft.
    const draft = 'e2e in-progress draft — must survive a background swap';
    await input.fill(draft);

    await page.evaluate(
      ([type, data]) => window.__emitPlanFrame(type, data),
      ['plan-agent-updated', frameHTML],
    );

    // No send was in flight, so the guard does not apply — the draft is restored
    // across the whole-card swap exactly as before.
    await expect(
      page.locator('section#plan-agent form.plan-assistant-form textarea[name="message"]'),
    ).toHaveValue(draft);
  });
});
