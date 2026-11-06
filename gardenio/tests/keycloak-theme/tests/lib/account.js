// S1e: what "the account console wears the gardenio theme" means, checked on
// the Signing-in page (where a person adds a passkey) in each of the four
// projects. The console is a React app, so the checks read computed styles
// and run axe's colour-contrast rule rather than looking for markup.
const { expect } = require('@playwright/test');
const AxeBuilder = require('@axe-core/playwright').default;

// The tokens in account/resources/css/gardenio-account.css, as the browser
// reports them (rgb()), per colour scheme.
const TOKENS = {
  light: { page: 'rgb(243, 244, 246)', card: 'rgb(255, 255, 255)', onMasthead: 'rgb(243, 244, 246)' },
  dark: { page: 'rgb(11, 15, 20)', card: 'rgb(17, 22, 29)', onMasthead: 'rgb(214, 224, 236)' },
};

// PatternFly's Page drawer mode (narrower than its xl breakpoint): the sidebar
// is an overlay a phone opens with the hamburger.
const PF_XL = 1200;

const bg = (loc) => loc.first().evaluate((el) => getComputedStyle(el).backgroundColor);

async function expectAccountThemed(page, testInfo) {
  const { scheme } = testInfo.project.metadata;
  const want = TOKENS[scheme];

  // theme.properties title/logo reached the page: the tab says gardenio and the
  // masthead brand is our wordmark, loaded (not a broken image).
  await expect(page, 'account: title').toHaveTitle(/gardenio/i);
  const brand = page.locator('img[src$="/img/logo.svg"]').first();
  await expect(brand, 'account: masthead wordmark').toBeVisible();
  expect(await brand.evaluate((img) => img.complete && img.naturalWidth), 'wordmark loaded').toBeGreaterThan(0);

  // Light and dark both resolve to our tokens (not PatternFly's #f0f0f0 / #0f1214).
  expect(await bg(page.locator('.pf-v5-c-page')), `account: page background (${scheme})`).toBe(want.page);
  const cards = page.locator('.pf-v5-c-card');
  if (await cards.count()) {
    expect(await bg(cards), `account: card background (${scheme})`).toBe(want.card);
  }

  // The hamburger is the bar's light ink, not the muted plain-button grey
  // (~2:1 on the dark bar; gd-ROY5CMuLfW). axe does not grade an icon's colour.
  expect(
    await page.locator('#nav-toggle').evaluate((el) => getComputedStyle(el).color),
    `account: hamburger colour (${scheme})`,
  ).toBe(want.onMasthead);

  // On a phone, choosing a page in the drawer closes it (js/gardenio-account.js):
  // it stayed open over the page it had opened (gd-ROY5CMuLfW).
  if ((page.viewportSize()?.width ?? PF_XL) < PF_XL) {
    await expect(page.locator('.pf-v5-c-page__sidebar'), 'account: drawer closed after navigating').not.toHaveClass(/pf-m-expanded/);
  }

  // Legible: every text node on the page passes AA contrast (axe color-contrast),
  // and every button has a name (the kebab menus are icon-only; the theme
  // script names them).
  const results = await new AxeBuilder({ page }).withRules(['color-contrast', 'button-name']).analyze();
  const bad = results.violations.flatMap((v) =>
    v.nodes.slice(0, 5).map((n) => `${n.target.join(' ')}: ${n.any[0]?.message || v.help}`),
  );
  testInfo.annotations.push({
    type: 'axe:account-contrast',
    description: bad.length ? bad.join(' | ') : 'no contrast or button-name violations',
  });
  expect(bad, `account: colour contrast and button names on Signing in (${scheme})`).toEqual([]);
}

module.exports = { expectAccountThemed };
