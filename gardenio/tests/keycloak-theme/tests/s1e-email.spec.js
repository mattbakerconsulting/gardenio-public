// S1e: the emails Keycloak sends wear the gardenio email theme. Keycloak sends
// to the SMTP sink in tests/lib/mail.js (the realm's smtpServer); each message
// is read back, checked, rendered in the browser and screenshotted:
//
//   email-password-reset   the "forgot password" link a person asks for
//   email-execute-actions  what an administrator triggers ("update your account")
//
// Same four projects as the other specs, so the emailed HTML is shot in light and
// dark (the render emulates the project's colour scheme, which is what the
// message's prefers-color-scheme rules answer) and at phone width.
const { test, expect } = require('@playwright/test');
const AxeBuilder = require('@axe-core/playwright').default;
const { realmUrl, CREDS, screenshot } = require('./lib/kc');
const { latestMail } = require('./lib/mail');
const { sendExecuteActionsEmail } = require('./lib/admin');

const TEST_USER_MAIL = 'test-user@example.invalid';

// Plain-text alternative: readable on its own, the link in full, no markup.
function expectPlainText(text, what) {
  expect(text, `${what}: text/plain part`).toBeTruthy();
  expect(text, `${what}: no markup in the text part`).not.toMatch(/<\/?[a-z][^>]*>/i);
  expect(text, `${what}: link in the text part`).toMatch(/https?:\/\/\S+action-token\?\S+/);
  expect(text, `${what}: text part is gardenio's`).toMatch(/gardenio/i);
  expect(text, `${what}: text part never says Keycloak`).not.toMatch(/keycloak/i);
}

// Render the emailed HTML the way a client would (the project's colour scheme),
// prove it fetches nothing, shoot it, and hold its text to AA contrast.
async function renderEmail(page, html, screen, testInfo) {
  const remote = [];
  page.on('request', (req) => {
    if (!/^(data|about|blob):/.test(req.url())) remote.push(req.url());
  });
  await page.setContent(html, { waitUntil: 'load' });

  expect(html, `${screen}: inline styles, not a stylesheet`).toContain('style="');
  expect(html, `${screen}: no remote images or stylesheets`).not.toMatch(/<img|<link|@import|url\(/i);
  expect(remote, `${screen}: requests made while rendering`).toEqual([]);
  await expect(page.locator('.gd-mark'), `${screen}: wordmark is text`).toHaveText('gardenio');
  await expect(page.locator('.gd-btn'), `${screen}: call to action`).toBeVisible();
  await expect(page.locator('body'), `${screen}: never says Keycloak`).not.toContainText(/keycloak/i);
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow, `${screen}: horizontal overflow in px`).toBeLessThanOrEqual(0);

  await screenshot(page, screen, testInfo);
  const results = await new AxeBuilder({ page }).withRules(['color-contrast']).analyze();
  const bad = results.violations.flatMap((v) => v.nodes.map((n) => n.target.join(' ')));
  expect(bad, `${screen}: colour contrast`).toEqual([]);
}

test.describe('email theme', () => {
  test('password reset', async ({ page, browser, contextOptions }, testInfo) => {
    // Ask for the reset link through the real forgot-password page.
    await page.goto(realmUrl('/account'));
    await page.getByRole('link', { name: /forgot/i }).click();
    await page.locator('#username').fill(CREDS.user.username);
    await page.locator('input[type="submit"], button[type="submit"]').first().click();
    await expect(page.locator('body')).toContainText(/check your email/i);

    const mail = await latestMail(process.env.KC_MAIL_DIR, TEST_USER_MAIL, /reset your gardenio password/i);
    expectPlainText(mail.text, 'password reset');
    expect(mail.text).toMatch(/expires in \d+ /i);
    expect(mail.html, 'password reset: text/html part').toBeTruthy();
    expect(mail.html).toContain('Choose a new password');

    // Render in a clean page (not the Keycloak one) with the project's scheme.
    const ctx = await browser.newContext(contextOptions);
    try {
      await renderEmail(await ctx.newPage(), mail.html, 'email-password-reset', testInfo);
    } finally {
      await ctx.close();
    }
  });

  test('administrator-requested account actions', async ({ browser, contextOptions }, testInfo) => {
    await sendExecuteActionsEmail(CREDS.user.username, ['UPDATE_PASSWORD']);
    const mail = await latestMail(process.env.KC_MAIL_DIR, TEST_USER_MAIL, /finish setting up your gardenio account/i);
    expectPlainText(mail.text, 'execute actions');
    expect(mail.text, 'execute actions: names the action').toMatch(/update password/i);
    expect(mail.html, 'execute actions: text/html part').toBeTruthy();
    expect(mail.html).toContain('Update my account');

    const ctx = await browser.newContext(contextOptions);
    try {
      await renderEmail(await ctx.newPage(), mail.html, 'email-execute-actions', testInfo);
    } finally {
      await ctx.close();
    }
  });
});
