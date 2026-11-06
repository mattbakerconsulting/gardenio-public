// The shipped pixel-art scene (Dawn Meadow, design doc section 13; built by S1h,
// gd-0SYmLWCCM5). Checks the constraints the scene has to meet that axe and the
// screenshots cannot show:
//
//   * same-origin only: no request leaves the harness origin, none is a data: URI
//   * the scene's assets (images + font) stay under 300 KB per colour scheme
//   * the pixel font is on the heading only; fields and labels keep the system font
//   * motion stops under prefers-reduced-motion, and exists otherwise
//   * the kc* markup and the passkey hooks are the production ones
const { test, expect } = require('@playwright/test');
const { realmUrl, BASE } = require('./lib/kc');

const LAYERS = [
  ['html', '::before'], ['html', '::after'], ['body', '::before'], ['body', '::after'],
  ['.pf-v5-c-login', '::before'], ['.pf-v5-c-login', '::after'],
];

async function openLogin(page) {
  await page.goto(realmUrl('/account'));
  await expect(page.locator('#username')).toBeVisible();
  await page.waitForLoadState('networkidle').catch(() => {});
}

test.describe('scene', () => {
  test('same-origin assets, under 300 KB, and the markup is the production markup', async ({ page }, testInfo) => {
    const bodies = [];
    const foreign = [];
    page.on('response', async (res) => {
      const url = res.url();
      if (url.startsWith('data:')) foreign.push(url.slice(0, 40));
      else if (new URL(url).origin !== new URL(BASE).origin) foreign.push(url);
      if (/\/resources\/.*\/login\/gardenio\/(img|fonts)\/.*\.(svg|png|woff2?)(\?|$)/.test(url)) {
        bodies.push({ url, bytes: (await res.body().catch(() => Buffer.alloc(0))).length });
      }
    });
    await openLogin(page);
    expect(foreign, 'requests outside the Keycloak origin').toEqual([]);
    const total = bodies.reduce((s, b) => s + b.bytes, 0);
    testInfo.annotations.push({
      type: 'scene-assets',
      description: `${testInfo.project.name}: ${bodies.length} files, ${total} bytes: ${bodies.map((b) => `${b.url.split('/').pop()}=${b.bytes}`).join(' ')}`,
    });
    expect(bodies.length, 'scene images and the font were fetched').toBeGreaterThan(3);
    expect(total, 'scene assets for one colour scheme').toBeLessThan(300 * 1024);

    // Production markup and hooks untouched.
    await expect(page.locator('#username')).toHaveAttribute('autocomplete', /username webauthn/);
    await expect(page.locator('#kc-login')).toBeVisible();
    await expect(page.locator('#kc-form-login.pf-v5-c-form')).toHaveCount(1);
    await expect(page.locator('#kc-page-title')).toHaveText(/^\s*Sign in/);
    // The pixel font is on headings only; the field and the body text are the system font.
    const fonts = await page.evaluate(() => ({
      title: getComputedStyle(document.querySelector('#kc-page-title')).fontFamily,
      input: getComputedStyle(document.querySelector('#username')).fontFamily,
      label: getComputedStyle(document.querySelector('label')).fontFamily,
    }));
    expect(fonts.title).toMatch(/Pixelify Sans/);
    expect(fonts.input).not.toMatch(/Pixelify/);
    expect(fonts.label).not.toMatch(/Pixelify/);
  });

  test('motion exists, and stops under prefers-reduced-motion', async ({ browser }) => {
    const names = async (reducedMotion) => {
      const ctx = await browser.newContext({ reducedMotion });
      const page = await ctx.newPage();
      await openLogin(page);
      const out = await page.evaluate((layers) => layers.map(([sel, pseudo]) => {
        const el = sel === 'html' ? document.documentElement : document.querySelector(sel);
        return `${sel}${pseudo}=${getComputedStyle(el, pseudo).animationName}`;
      }), LAYERS);
      await ctx.close();
      return out;
    };
    const moving = await names('no-preference');
    const still = await names('reduce');
    expect(moving.some((n) => !n.endsWith('=none')), `animated layers: ${moving.join(' ')}`).toBeTruthy();
    expect(still.every((n) => n.endsWith('=none')), `reduced motion: ${still.join(' ')}`).toBeTruthy();
  });
});
