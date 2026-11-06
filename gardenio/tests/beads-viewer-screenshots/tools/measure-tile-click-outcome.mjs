// Users-CAMP integrity driver: measure the CLICK OUTCOME of every /u/{slug}
// summary stat tile, not just whether a matching chip exists in the markup.
//
// WHY THIS EXISTS (bug gd-XVZNaYe9nZ). The measure step's static
// facet_match_ratio was blind to a whole defect class because it (1) EXCUSED
// zero-count tiles ("nothing to filter to, so a missing chip is fine") and
// (2) only asked "does a chip with this tile's facet key exist" — never "what
// does clicking the tile actually DO to the stream". Both real defects the
// lane was built to catch render DOM-well-formed AND pixel-perfect, so only an
// OUTCOME measurement sees them:
//
//   * ka-0000003914 — clicking a tile that reads 0 hits activate()'s !target
//     branch and RESETS the filter to '' (shows the ENTIRE stream) instead of
//     an empty/no-match state. 139/170 tiles across all 34 users behave this
//     way — a click that promises "these 0 things" and delivers "everything".
//   * ka-0000008262 — "Merged MRs" and "Open MRs" share facet key `mrs`, so
//     clicking either filters to the identical rows; one tile's label
//     contradicts what its own click surfaces.
//
// A STATIC simulation of the CURRENT JS cannot be the detector, because the
// two candidate product fixes for ka-0000003914 change the JS in DIFFERENT
// directions (make zero tiles inert vs. make activate() show the no-match
// state). A detector hard-coded to today's activate() would false-FAIL a valid
// fix. So this driver clicks the REAL (possibly-fixed) tile in a real browser
// and records what the stream actually does — fix-independent by construction.
//
// HEALTHY-SAFE INVARIANT. tile_count (the section count printed on the tile) is
// NOT the stream's facet population: the `mrs` facet folds two sections
// (Merged + Open MRs), dedup drops lower-precedence duplicates, and
// un-timestamped rows are dropped from the stream but still counted on the tile
// (user_home_activity.go assembleActivityStream). So "visible != tile_count" is
// NOT a defect on a healthy page — the original facet_integrity comment warns
// exactly this. The robust invariant a correct page satisfies is:
//
//     clicking a tile filters the stream to EXACTLY that facet's chip
//     population — visible_after_click == facet_chip_count(facet_link).
//
// The silent-unfilter defect breaks it unmistakably: a facet whose chip
// population is 0 (or absent) shows the FULL stream after the click
// (visible == total > 0) with no no-match state. That is the objective,
// fix-independent, false-positive-free signal this driver emits.
//
// Usage:
//   BEADS_VIEWER_URL=http://localhost:8095 \
//   SLUGS=ken,matthew.baker \            # comma list, OR
//   ALL_USERS=1 \                        # scan every /people row's /u/{slug}
//   OUT=docs/users-camp/<iter>/captures/tile-click-outcomes.json \
//   [BEARER=<jwt>] [SELF_EMAIL_FOR=<slug>=<email>,...] \
//   node tools/measure-tile-click-outcome.mjs
//
// Output JSON shape (consumed by measure-users-camp.py --tile-outcomes):
//   {
//     "slugs": ["ken", ...],
//     "tiles": [ {slug,label,tile_count,facet_link,clickable,
//                 visible_after_click,total,nomatch,
//                 facet_population,outcome_ok,silently_unfilters}, ... ],
//     "duplicate_tile_facet_keys": [ {slug, facet, labels:[...]} , ... ],
//     "errors": [ ... ]
//   }
import { chromium } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';

const baseURL = process.env.BEADS_VIEWER_URL || 'http://localhost:8095';
const bearer = process.env.BEARER || '';
const outPath = process.env.OUT || 'tile-click-outcomes.json';
const allUsers = ['1', 'true', 'yes'].includes(
  String(process.env.ALL_USERS || '').toLowerCase(),
);
const slugsEnv = (process.env.SLUGS || '')
  .split(',')
  .map((s) => s.trim())
  .filter(Boolean);

// Optional owner-email overrides so a specific slug can be driven as the OWNER
// (IsViewerSelf=true) render under dev-auth-bypass (X-Auth-Request-Email). Not
// required — the peer render carries the same summary tiles.
const selfEmailFor = {};
for (const pair of (process.env.SELF_EMAIL_FOR || '').split(',')) {
  const [slug, email] = pair.split('=');
  if (slug && email) selfEmailFor[slug.trim()] = email.trim();
}

const headers = bearer ? { Authorization: `Bearer ${bearer}` } : {};

async function newContext(browser, extraHeaders) {
  return browser.newContext({
    viewport: { width: 1280, height: 900 },
    deviceScaleFactor: 1,
    ignoreHTTPSErrors: true,
    colorScheme: 'light',
    locale: 'en-US',
    timezoneId: 'UTC',
    extraHTTPHeaders: { ...headers, ...(extraHeaders || {}) },
  });
}

// Discover every /u/{slug} the /people directory links to. Deduped, order
// preserved. Uses the SAME same-origin authenticated context as the tile clicks.
async function discoverSlugs(browser) {
  const context = await newContext(browser);
  const page = await context.newPage();
  const res = await page.goto(`${baseURL}/people`, { waitUntil: 'networkidle' });
  if (!res || res.status() >= 400) {
    await context.close();
    throw new Error(`/people returned HTTP ${res ? res.status() : 'null'}`);
  }
  const slugs = await page.evaluate(() => {
    const out = [];
    const seen = new Set();
    document.querySelectorAll('a.people-row-name[href]').forEach((a) => {
      const m = /\/u\/([^/#?"]+)/.exec(a.getAttribute('href') || '');
      if (m && !seen.has(m[1])) {
        seen.add(m[1]);
        out.push(decodeURIComponent(m[1]));
      }
    });
    return out;
  });
  await context.close();
  return slugs;
}

// Drive every summary stat tile on ONE /u/{slug} page and record the outcome.
// Returns { tiles: [...], duplicates: [...] } for this slug.
async function measureSlug(browser, slug) {
  const extra = selfEmailFor[slug]
    ? { 'X-Auth-Request-Email': selfEmailFor[slug] }
    : undefined;
  const context = await newContext(browser, extra);
  const page = await context.newPage();
  const url = `${baseURL}/u/${encodeURIComponent(slug)}`;
  const res = await page.goto(url, { waitUntil: 'networkidle' });
  if (!res || res.status() >= 400) {
    await context.close();
    throw new Error(`/u/${slug} returned HTTP ${res ? res.status() : 'null'}`);
  }

  // Enumerate the tiles up front: for each summary stat tile capture its label,
  // printed count, whether it is CLICKABLE (has an <a> — a valid product fix may
  // make zero tiles inert, and an inert tile can never silently unfilter), and
  // its data-facet-link. Also record the facet-chip populations so the driver
  // can score each click against the facet it targets, not against the tile's
  // own (differently-scoped) count.
  const meta = await page.evaluate(() => {
    const parseNum = (t) => {
      const m = /([0-9][0-9,]*)/.exec(t || '');
      return m ? parseInt(m[1].replace(/,/g, ''), 10) : 0;
    };
    const tiles = [];
    document.querySelectorAll('li.user-home-stat').forEach((li) => {
      const numEl = li.querySelector('.user-home-stat-num');
      const labelEl = li.querySelector('.user-home-stat-label');
      // A tile is "clickable as a facet link" iff it carries an <a
      // data-facet-link>. A fix that drops the link on zero tiles (candidate
      // (a) in ka-0000003914) makes clickable=false, which the scorer treats as
      // safe (an inert tile cannot lie about a filter).
      const link = li.querySelector('a[data-facet-link]');
      tiles.push({
        label: labelEl ? labelEl.textContent.replace(/\s+/g, ' ').trim() : '',
        tile_count: numEl ? parseNum(numEl.textContent) : 0,
        facet_link: link ? link.getAttribute('data-facet-link') || '' : null,
        clickable: !!link,
      });
    });
    // Facet chip populations (data-facet on the chip strip, with its printed N).
    const facetPop = {};
    document
      .querySelectorAll('[data-activity-facets] [data-facet]')
      .forEach((c) => {
        const key = c.getAttribute('data-facet') || '';
        if (key === '') return; // the "All" chip is not a facet population
        const nEl = c.querySelector('.activity-facet-n');
        facetPop[key] = nEl ? parseNum(nEl.textContent) : 0;
      });
    const total = document.querySelectorAll('[data-activity-item]').length;
    return { tiles, facetPop, total };
  });

  // duplicate_tile_facet_keys: two tiles on ONE page sharing a facet key
  // (ka-0000008262: "Merged MRs" and "Open MRs" both map to `mrs`). Purely
  // static — no click needed — but recorded per slug so the metric can dedupe.
  const byFacet = {};
  for (const t of meta.tiles) {
    if (t.facet_link) (byFacet[t.facet_link] ||= []).push(t.label);
  }
  const duplicates = Object.entries(byFacet)
    .filter(([, labels]) => labels.length > 1)
    .map(([facet, labels]) => ({ slug, facet, labels }));

  const results = [];
  for (let i = 0; i < meta.tiles.length; i++) {
    const t = meta.tiles[i];
    // Reset to the unfiltered "All" view before each tile so one click's state
    // never bleeds into the next measurement.
    await page.evaluate(() => {
      const all = document.querySelector('[data-activity-facets] [data-facet=""]');
      if (all) all.click();
    });

    let visible = null;
    let nomatch = null;
    let clicked = false;
    if (t.clickable) {
      // Click the REAL tile link. preventDefault on the fragment nav keeps the
      // test from scrolling, but the click handler (activate) still runs — we
      // measure exactly the JS the product ships.
      clicked = await page.evaluate((idx) => {
        const li = document.querySelectorAll('li.user-home-stat')[idx];
        const a = li && li.querySelector('a[data-facet-link]');
        if (!a) return false;
        a.addEventListener('click', (e) => e.preventDefault(), { once: true });
        a.click();
        return true;
      }, i);
      const observed = await page.evaluate(() => {
        const items = document.querySelectorAll('[data-activity-item]');
        let vis = 0;
        items.forEach((el) => {
          if (getComputedStyle(el).display !== 'none') vis++;
        });
        const nm = document.querySelector('[data-activity-no-match]');
        return {
          visible: vis,
          nomatch: nm ? !nm.hidden : false,
        };
      });
      visible = observed.visible;
      nomatch = observed.nomatch;
    }

    // The facet this tile targets, and the stream population that facet holds.
    // A facet with no chip has population 0 (nothing to filter to).
    const facetPopulation = t.facet_link
      ? meta.facetPop[t.facet_link] || 0
      : null;

    // outcome_ok — the healthy-safe invariant. An inert (non-clickable) tile is
    // always OK (it makes no filter promise). A clickable tile is OK iff its
    // post-click visible count equals the population of the facet it targets:
    //   * facet has rows  -> filtered to exactly those rows;
    //   * facet is empty   -> visible 0 (with the no-match state), NOT the whole
    //                         stream. This is the exact silent-unfilter defect.
    // Comparing against facet_population (the chip's own N), not tile_count,
    // is what keeps a legitimate mrs-fold / dedup / dropped-untimestamped-row
    // page from false-failing.
    let outcomeOk;
    let silentlyUnfilters = false;
    if (!t.clickable) {
      outcomeOk = true;
    } else if (facetPopulation === 0) {
      // Empty facet: a correct click shows nothing (no-match), a broken one
      // shows the whole stream.
      silentlyUnfilters = visible > 0 && !(nomatch && visible === 0);
      outcomeOk = visible === 0;
    } else {
      outcomeOk = visible === facetPopulation;
    }

    results.push({
      slug,
      label: t.label,
      tile_count: t.tile_count,
      facet_link: t.facet_link,
      clickable: t.clickable,
      clicked,
      visible_after_click: visible,
      total: meta.total,
      nomatch,
      facet_population: facetPopulation,
      outcome_ok: outcomeOk,
      silently_unfilters: silentlyUnfilters,
    });
  }

  await context.close();
  return { tiles: results, duplicates };
}

const browser = await chromium.launch();
const out = {
  target_url: baseURL,
  slugs: [],
  tiles: [],
  duplicate_tile_facet_keys: [],
  errors: [],
};
try {
  let slugs = slugsEnv;
  if (allUsers || slugs.length === 0) {
    try {
      const discovered = await discoverSlugs(browser);
      slugs = allUsers ? discovered : slugs.length ? slugs : discovered;
    } catch (e) {
      out.errors.push(`slug discovery failed: ${e.message}`);
    }
  }
  out.slugs = slugs;
  for (const slug of slugs) {
    try {
      const { tiles, duplicates } = await measureSlug(browser, slug);
      out.tiles.push(...tiles);
      out.duplicate_tile_facet_keys.push(...duplicates);
    } catch (e) {
      out.errors.push(`${slug}: ${e.message}`);
    }
  }
} finally {
  await browser.close();
}

fs.mkdirSync(path.dirname(path.resolve(outPath)), { recursive: true });
fs.writeFileSync(outPath, JSON.stringify(out, null, 2));

// Headline summary to stdout (stderr carries the fail line for the step comment).
const clickable = out.tiles.filter((t) => t.clickable);
const bad = out.tiles.filter((t) => t.clickable && !t.outcome_ok);
const silent = out.tiles.filter((t) => t.silently_unfilters);
console.log(
  `wrote ${outPath}: ${out.slugs.length} slug(s), ${out.tiles.length} tiles ` +
    `(${clickable.length} clickable), ${bad.length} outcome-mismatch, ` +
    `${silent.length} silently-unfiltering, ` +
    `${out.duplicate_tile_facet_keys.length} duplicate-facet-key row(s)`,
);
if (out.errors.length) {
  console.error(`TILE-CLICK ERRORS: ${out.errors.join('; ')}`);
}
if (bad.length || silent.length) {
  const sample = bad
    .slice(0, 5)
    .map((t) => `${t.slug}/${t.label}[${t.facet_link}]=${t.visible_after_click}vs${t.facet_population}`)
    .join(', ');
  console.error(`TILE-CLICK-FAIL: ${bad.length} tiles mismatch outcome (${sample})`);
}
// Never nonzero-exit on a detected DEFECT — the measure step reads the JSON and
// decides. Only a hard driver failure (no slugs, browser crash) is an error.
process.exit(out.slugs.length === 0 && out.errors.length ? 1 : 0);
