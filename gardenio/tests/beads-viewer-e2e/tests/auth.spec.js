// Phase T14 (gd-xD79RekEvc) of epic ka-0000004611 — auth middleware
// behavior at the browser layer.
//
// Wire:
//   main.go installs one of two middlewares on the protected mux:
//     - devBypassMiddleware (BEADS_VIEWER_DEV_AUTH_BYPASS=1) — passthrough.
//     - oidcauth.Middleware — verifies a Bearer/X-Auth-Request-Access-Token
//       token; missing → 401, OIDC discovery failure → 503 fail-closed.
//   /login is registered ANONYMOUSLY (outside the auth chain) so the
//   browser can reach the OIDC start picker even when unauthenticated.
//   /healthz and /readyz are also anonymous.
//
// What this suite covers (browser-level):
//   1. Bypass on (the harness mode): a representative set of protected
//      routes returns 2xx — proves the dev passthrough is wired.
//   2. /login is anonymous and 302s into the oauth2-proxy start URL —
//      this is the "OIDC start endpoint reachable" check that callers
//      land on when a real ForwardAuth bounces them.
//   3. /healthz remains anonymous regardless of bypass state.
//
// What this suite does NOT cover (per the plan, kept in Go):
//   - Token validation paths (verifyingMiddleware extractToken / claims /
//     role / email-domain checks). Lives in oidcauth_test.go.
//   - Bypass-off + no-token → 401/503. Booting a second viewer with
//     OIDC discovery against an unreachable issuer eats ~31s of retry
//     backoff before the listener comes up (see oidcauth.go retry
//     constants); not worth the harness time when middleware_test.go +
//     oidcauth_test.go already exercise the matrix at the unit level.
//   - Traefik ForwardAuth's 401-→302 rewrite. That is cluster-edge
//     behavior, not viewer behavior.

import { expect, test } from '@playwright/test';

// Routes that sit behind the auth middleware. Keep this list small —
// T3 already covers full route smoke; T14 just needs enough surface to
// prove the bypass middleware is in place and passing through.
const PROTECTED_ROUTES = [
  '/',
  '/active',
  '/agents',
  '/advice',
  '/decisions',
  '/triggers',
  '/costs',
];

test.describe('auth middleware', () => {
  test.describe('BEADS_VIEWER_DEV_AUTH_BYPASS=1 (harness mode)', () => {
    test('protected routes pass through to handlers', async ({ request }) => {
      for (const path of PROTECTED_ROUTES) {
        const res = await request.get(path);
        expect(res.status(), `${path} status under bypass`).toBeLessThan(400);
      }
    });

    test('readyz returns ok (OIDC bypass implies ready)', async ({ request }) => {
      const res = await request.get('/readyz');
      expect(res.status()).toBe(200);
      const body = await res.json();
      expect(body.status).toBe('ok');
    });
  });

  test.describe('anonymous endpoints (always reachable)', () => {
    test('GET /login redirects to oauth2-proxy /oauth2/start', async ({ request }) => {
      // Picker is disabled in the harness (no LOGIN_PICKER_* env), so
      // /login takes the straight-passthrough branch in handleLogin and
      // 302s to <forwardAuthURL>/oauth2/start with rd= back to the viewer.
      // forwardAuthURL is empty in the harness, so the Location is the
      // relative path /oauth2/start?rd=...
      const res = await request.get('/login', { maxRedirects: 0 });
      expect(res.status(), 'login should be a redirect, not a rendered page').toBe(302);
      const location = res.headers()['location'];
      expect(location, 'Location header').toBeTruthy();
      expect(location).toContain('/oauth2/start');
      // rd= (return-destination) carries the user back to the viewer
      // root after auth completes — the contract Traefik depends on.
      expect(location).toMatch(/[?&]rd=/);
    });

    test('GET /healthz returns ok without auth', async ({ request }) => {
      const res = await request.get('/healthz');
      expect(res.status()).toBe(200);
      const body = await res.json();
      expect(body.status).toBe('ok');
    });
  });
});
