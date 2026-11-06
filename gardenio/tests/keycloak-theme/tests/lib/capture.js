// Shared by the screen specs: the "is this page themed" checks, the screenshot
// and the axe run (a hard gate since S1c, see screens.spec.js).
const fs = require('fs');
const path = require('path');
const { expect } = require('@playwright/test');
const AxeBuilder = require('@axe-core/playwright').default;
const { AXE_DIR, screenshot, shotName } = require('./kc');

const ENFORCE = process.env.AXE_ENFORCE !== '0';

// A screen rendered by the gardenio login theme (everything except the account
// console, which is a separate React app with its own theme) must show no
// Keycloak branding and must not scroll sideways at 360px.
async function expectThemed(page, screen) {
  await expect(page, `${screen}: title`).not.toHaveTitle(/keycloak/i);
  await expect(page.locator('body'), `${screen}: visible text`).not.toContainText(/keycloak/i);
  // S1d: nobody is shown the word WebAuthn (it is "passkey" / "security key").
  await expect(page.locator('body'), `${screen}: visible text`).not.toContainText(/webauthn/i);
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow, `${screen}: horizontal overflow in px`).toBeLessThanOrEqual(0);
  // The wordmark is gardenio's logo.svg painted by CSS; the header text stays as its name.
  await expect(page.locator('#kc-header-wrapper'), `${screen}: wordmark name`).toHaveText(/^\s*gardenio\s*$/);
  await expect(page.locator('html'), `${screen}: lang`).toHaveAttribute('lang', /^[a-z]{2}/i);
}

const SETTLE_MS = 10_000;

async function capture(page, screen, testInfo, { themed = true, enforce = true } = {}) {
  // Let fonts, the SPA and any redirect settle before the shot. Bounded: on the
  // account console networkidle sometimes never fires with nothing in flight,
  // and an unbounded wait spends the whole test timeout (gd-ROY5CMuLfW).
  await page.waitForLoadState('networkidle', { timeout: SETTLE_MS }).catch(() => {});
  if (themed) await expectThemed(page, screen);
  await screenshot(page, screen, testInfo);

  const results = await new AxeBuilder({ page }).analyze();
  fs.mkdirSync(AXE_DIR, { recursive: true });
  const slim = results.violations.map((v) => ({
    id: v.id,
    impact: v.impact,
    help: v.help,
    nodes: v.nodes.length,
    targets: v.nodes.slice(0, 5).map((n) => n.target.join(' ')),
  }));
  fs.writeFileSync(
    path.join(AXE_DIR, `${shotName(screen, testInfo)}.json`),
    JSON.stringify({ screen, url: page.url(), title: await page.title(), violations: slim }, null, 2),
  );
  testInfo.annotations.push({
    type: `axe:${screen}`,
    description: `${slim.length} violation(s): ${slim.map((v) => `${v.id}(${v.impact})`).join(', ') || 'none'}`,
  });
  if (ENFORCE && enforce) {
    const bad = slim.filter((v) => v.impact === 'serious' || v.impact === 'critical');
    expect(bad, `serious/critical axe violations on ${screen}`).toEqual([]);
  }
}

module.exports = { capture, expectThemed, SETTLE_MS };
