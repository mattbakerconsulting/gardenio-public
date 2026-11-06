// Dawn Meadow (shipped, S1h gd-0SYmLWCCM5). Light = sunrise over a garden; dark = the same
// garden under the moon, with fireflies. Tiles are 320 cells wide.
'use strict';
const path = require('path');
const { Grid, rng, wave, mix, writeSvg } = require('./lib');

const W = 320;
// Cells a layer is drawn on; the CSS shows each cell as --gd-px CSS pixels.
const L = {
  // day palette
  far: '#b9c6e6', farHi: '#cdd5ee',
  mid: '#8fc3a6', midHi: '#a8d4b4',
  near: '#6fb56a', nearHi: '#8cc77a', nearLo: '#559a58',
  g1: '#7cc56e', g2: '#4f9e55', g3: '#357a47',
  dirt: '#a2724f', dirtHi: '#b98862', dirtLo: '#835a3e',
  stem: '#3f8f4a', leaf: '#56a84f', leafD: '#2f7340',
  yel: '#ffd23f', yelD: '#f2a30f', brown: '#6b3e1f',
  red: '#e0443e', redD: '#a62b33', pink: '#f58fb0', pinkD: '#c9608a',
  vio: '#8b6bd6', vioD: '#5d46a8', white: '#fffdf4', cream: '#f2e6c6',
  wood: '#d8a468', woodD: '#a8743f',
  cloud: '#fff7ec', cloudD: '#f5c8b8',
  sunA: '#ffdf7a', sunB: '#fff3b8',
};
// Colours that keep their value at night (they glow).
const EMISSIVE = new Set(['#ffe27a', '#fff6c2', '#ffd23f_e']);
const night = (c) => (EMISSIVE.has(c) ? c : mix(mix(c, '#1c2a63', 0.62), '#06081a', 0.12));
const toNight = (g) => g.recolor((c) => (c.length === 9 ? night(c.slice(0, 7)) + c.slice(7) : night(c)));

const S = {
  sunflower: [
    '...yyy...',
    '.yyyyyyy.',
    'yyybbbyyy',
    'yYbBBBbYy',
    'yyybbbyyy',
    '.yyyyyyy.',
    '...yYy...',
    '....s....',
    '.l..s....',
    'll..s..l.',
    'lL..s.ll.',
    '.L..s.Ll.',
    '....s....',
    '....s....',
    '....s....',
  ],
  tulip: [
    '.r.r.',
    'rrRrr',
    'rrrRr',
    '.rrr.',
    '..s..',
    '.ls..',
    '.Lsl.',
    '..sL.',
    '..s..',
    '..s..',
  ],
  daisy: [
    '.www.',
    'wwyww',
    '.www.',
    '..s..',
    '.ls..',
    '..sl.',
    '..s..',
  ],
  lavender: [
    '.v.',
    'vVv',
    '.v.',
    'vVv',
    '.v.',
    'vVv',
    '.s.',
    '.s.',
    'ls.',
    '.sl',
    '.s.',
  ],
  sprout: ['l...l', 'Ll.lL', '.lsl.', '..s..', '..s..'],
  cabbage: ['..lll..', '.lLllL.', 'lllLlll', '.lLlLl.', '..LLL..'],
  bush: [
    '..lllll..',
    '.lllrlll.',
    'lllllllrl',
    'lLlrllLll',
    '.LLlllLL.',
    '..LLLLL..',
  ],
  mushroom: ['.rrr.', 'rwrwr', '.rrr.', '.ccc.', '.ccc.'],
};

function sunflower(stem) {
  const head = ['...yyy...', '.yyyyyyy.', 'yyybbbyyy', 'yYbBBBbYy', 'yyybbbyyy', '.yyyyyyy.', '...yYy...'];
  const rows = [];
  for (let i = 0; i < stem; i++) {
    const leafRow = i === 1 || i === 2 ? '.l..s....' : i === 3 ? 'lL..s....' : i === stem - 6 ? '....s.ll.' : i === stem - 5 ? '....s.Ll.' : '....s....';
    rows.push(leafRow);
  }
  return [...head, ...rows];
}
const SCARECROW = ['....kkk....', '...kkkkk...', '.kkkkkkkkk.', '...ooooo...', '...oEoEo...', '...ooEoo...', '....ooo....',
  'ooRRRRRRRoo', 'o.RRRPRR.o.', '..RRRRRRR..', '...RRRRR...', '....ttt....', '....ttt....', '....ttt....', '....ttt....', '....ttt....', '....ttt....', '....ttt....'];

function plant(g, kind, x, baseY, pal, flip, r) {
  const sp = kind.sprite === 'sunflower' ? sunflower(8 + Math.floor((r ? r() : 0) * 14)) : S[kind.sprite];
  const legend = { ...kind.legend(pal) };
  g.stamp(sp, x - Math.floor(sp[0].length / 2), baseY - sp.length, legend, { flip });
}

const KINDS = [
  { sprite: 'sunflower', w: 3, legend: (p) => ({ y: p.yel, Y: p.yelD, b: p.brown, B: '#4a2a14', s: p.stem, l: p.leaf, L: p.leafD }) },
  { sprite: 'tulip', w: 3, legend: (p) => ({ r: p.red, R: p.redD, s: p.stem, l: p.leaf, L: p.leafD }) },
  { sprite: 'tulip', w: 3, legend: (p) => ({ r: p.pink, R: p.pinkD, s: p.stem, l: p.leaf, L: p.leafD }) },
  { sprite: 'daisy', w: 3, legend: (p) => ({ w: p.white, y: p.yel, s: p.stem, l: p.leaf }) },
  { sprite: 'lavender', w: 2, legend: (p) => ({ v: p.vio, V: p.vioD, s: p.stem, l: p.leaf }) },
  { sprite: 'cabbage', w: 2, legend: (p) => ({ l: '#8fd08c', L: '#5eab63' }) },
  { sprite: 'sprout', w: 2, legend: (p) => ({ l: p.leaf, L: p.leafD, s: p.stem }) },
  { sprite: 'bush', w: 1, legend: (p) => ({ l: p.leaf, L: p.leafD, r: p.red }) },
];

// ---- hills: far, mid, near meadow (static) --------------------------------

function hills() {
  const H = 130;
  const g = new Grid(W, H);
  const r = rng(11);
  const wf = wave(W, r, 4);
  const wm = wave(W, r, 5);
  const wn = wave(W, r, 4);
  for (let x = 0; x < W; x++) {
    const far = 62 + wf(x) * 16;
    const mid = 40 + wm(x) * 13;
    const near = 22 + wn(x) * 7;
    g.col(x, H - far, L.far);
    g.col(x, H - mid, L.mid);
    g.col(x, H - near, L.near);
    // dithered rims: a lit row on top, then a checker row
    for (const [top, hi, c] of [[H - far, L.farHi, L.far], [H - mid, L.midHi, L.mid], [H - near, L.nearHi, L.near]]) {
      g.set(x, Math.round(top), hi);
      if ((x + Math.round(top)) % 2 === 0) g.set(x, Math.round(top) + 1, hi);
      if ((x * 7 + Math.round(top)) % 5 === 0) g.set(x, Math.round(top) + 2, c);
    }
  }
  // far hills: a few tiny trees on the ridge
  for (let i = 0; i < 16; i++) {
    const x = Math.floor(r() * W);
    const top = Math.round(H - (62 + wf(x) * 16));
    for (let k = 0; k < 3; k++) for (let j = 0; j <= k + 1; j++) g.rect(x - j, top - 8 + k * 2 + j, 1 + 2 * j, 1, '#98aad2');
    g.rect(x, top - 1, 1, 2, '#98aad2');
  }
  // near meadow: grass flecks and wild flowers
  for (let i = 0; i < 380; i++) {
    const x = Math.floor(r() * W);
    const top = Math.round(H - (22 + wn(x) * 7));
    const y = top + 2 + Math.floor(r() * (H - top - 4));
    const t = r();
    g.set(x, y, t < 0.5 ? L.nearLo : L.nearHi);
    if (r() < 0.25) g.set(x + 1, y - 1, t < 0.5 ? L.nearLo : L.nearHi);
  }
  const fl = [L.white, L.yel, L.pink, L.white, '#ffffff'];
  for (let i = 0; i < 70; i++) {
    const x = Math.floor(r() * W);
    const top = Math.round(H - (22 + wn(x) * 7));
    const y = top + 3 + Math.floor(r() * 14);
    g.set(x, y, fl[Math.floor(r() * fl.length)]);
  }
  // a path of lighter ground fading into the bed
  return g;
}

// ---- the garden bed: soil, fence, plants (grow in) ---------------------------

function bed() {
  const H = 50;
  const g = new Grid(W, H);
  const r = rng(23);
  const baseY = H - 6;
  // fence segments, behind the plants
  const post = (x, y0) => {
    g.rect(x, y0 + 2, 3, 11, L.wood);
    g.rect(x + 2, y0 + 2, 1, 11, L.woodD);
    g.set(x + 1, y0, L.wood);
    g.rect(x, y0 + 1, 3, 1, L.wood);
  };
  for (const [x0, n] of [[6, 7], [150, 6], [262, 4]]) {
    for (let i = 0; i < n; i++) post(x0 + i * 6, baseY - 22);
    g.rect(x0, baseY - 19, n * 6 - 3, 2, L.woodD);
    g.rect(x0, baseY - 13, n * 6 - 3, 2, L.woodD);
  }
  g.stamp(SCARECROW, 212, baseY + 1 - SCARECROW.length, { k: '#6b4a2b', o: '#e8c46a', E: '#3a2a14', R: '#d2574a', P: '#f2d36b', t: L.woodD });
  // soil band along the bottom with furrows
  for (let x = 0; x < W; x++) {
    g.col(x, baseY + 1, L.dirt);
    g.set(x, baseY + 1, L.dirtHi);
    if ((x + 3) % 4 < 2) g.set(x, baseY + 3, L.dirtLo);
    if ((x * 5 + 1) % 9 === 0) g.set(x, baseY + 2, L.dirtLo);
    if ((x * 3 + 2) % 11 === 0) g.set(x, baseY + 4, L.dirtHi);
  }
  // plants in groups of a kind, with gaps
  let x = 3;
  while (x < W - 3) {
    const kind = KINDS[Math.floor(r() * KINDS.length)];
    const n = 1 + Math.floor(r() * 3);
    for (let i = 0; i < n && x < W - 3; i++) {
      plant(g, kind, x, baseY + 1, L, r() < 0.5, r);
      x += kind.sprite === 'sunflower' || kind.sprite === 'bush' ? 11 : 8;
    }
    x += 3 + Math.floor(r() * 12);
  }
  return g;
}

// ---- foreground grass blades (sways one cell) ---------------------------------

function grass() {
  const H = 26;
  const g = new Grid(W, H);
  const r = rng(37);
  const cols = [L.g1, L.g2, L.g3];
  for (let x = 0; x < W; x += 1) {
    if (r() < 0.55) continue;
    const h = 3 + Math.floor(r() * 9);
    const c = cols[Math.floor(r() * cols.length)];
    const lean = r() < 0.5 ? -1 : 1;
    for (let y = 0; y < h; y++) g.set(x + (y > h * 0.6 ? lean : 0), H - 1 - y, c);
  }
  g.rect(0, H - 3, W, 3, L.g3);
  for (let x = 0; x < W; x++) if ((x * 3) % 7 < 2) g.set(x, H - 3, L.g2);
  // a few tall wild flowers poking up
  for (let i = 0; i < 9; i++) {
    const x = Math.floor(r() * W);
    const h = 8 + Math.floor(r() * 8);
    g.rect(x, H - h, 1, h, L.g2);
    const c = [L.white, L.pink, L.yel, L.vio][i % 4];
    g.set(x - 1, H - h, c); g.set(x + 1, H - h, c); g.set(x, H - h - 1, c); g.set(x, H - h + 1, L.yelD);
  }
  return g;
}

// ---- clouds (drift) ------------------------------------------------------------

function blob(g, cx, cy, circles, c, cShade) {
  const tmp = new Grid(g.w, g.h);
  for (const [dx, dy, rx, ry] of circles) tmp.disc(cx + dx, cy + dy, rx, ry, c);
  for (let y = 0; y < g.h; y++) {
    for (let x = 0; x < g.w; x++) {
      if (tmp.get(x, y)) g.set(x, y, tmp.get(x, y + 2) ? c : cShade);
    }
  }
}
function clouds(top, bottom, seed) {
  const H = 70;
  const g = new Grid(W, H);
  const r = rng(seed);
  const spots = [[30, 30, 1], [118, 14, 0.7], [190, 44, 1.2], [262, 22, 0.8]];
  for (const [cx, cy, s] of spots) {
    blob(g, cx, cy, [[-9 * s, 2, 7 * s, 4 * s], [0, -2, 9 * s, 6 * s], [10 * s, 2, 8 * s, 4.5 * s], [2 * s, 3, 15 * s, 3.5 * s]], top, bottom);
    if (r() < 0.5) blob(g, cx + 24, cy + 9, [[0, 0, 6, 3], [6, 1, 8, 3]], top, bottom);
  }
  return g;
}

// ---- sun / moon / stars ------------------------------------------------------

function sun() {
  const g = new Grid(80, 80, { wrapX: false });
  // dithered halo rings, then disc
  for (let y = 0; y < 80; y++) {
    for (let x = 0; x < 80; x++) {
      const d = Math.hypot(x - 39.5, y - 39.5);
      if (d < 14) g.set(x, y, d < 9 ? L.sunB : L.sunA);
      else if (d < 22 && (x + y) % 2 === 0) g.set(x, y, '#ffe9a066');
      else if (d < 32 && (x + y) % 4 === 0) g.set(x, y, '#ffe9a055');
    }
  }
  return g;
}
function moon() {
  const g = new Grid(80, 80, { wrapX: false });
  for (let y = 0; y < 80; y++) {
    for (let x = 0; x < 80; x++) {
      const d = Math.hypot(x - 39.5, y - 39.5);
      if (d < 11) g.set(x, y, d < 7 ? '#fff6c2' : '#ffe27a');
      else if (d < 20 && (x + y) % 2 === 0) g.set(x, y, '#ffe27a44');
      else if (d < 28 && (x + y) % 4 === 0) g.set(x, y, '#ffe27a33');
    }
  }
  // craters
  for (const [cx, cy, rr] of [[37, 37, 2], [43, 43, 2], [41, 35, 1]]) {
    for (let y = -rr; y <= rr; y++) for (let x = -rr; x <= rr; x++) if (x * x + y * y <= rr * rr) g.set(cx + x, cy + y, '#e9cf6c');
  }
  return g;
}
function stars() {
  const H = 130;
  const g = new Grid(W, H);
  const r = rng(51);
  for (let i = 0; i < 70; i++) {
    const x = Math.floor(r() * W);
    const y = Math.floor(r() * r() * H);
    g.set(x, y, r() < 0.8 ? '#e8ecff' : '#ffe9a0');
  }
  for (let i = 0; i < 7; i++) {
    const x = Math.floor(r() * W);
    const y = 4 + Math.floor(r() * 60);
    for (const [dx, dy] of [[0, 0], [1, 0], [-1, 0], [0, 1], [0, -1]]) g.set(x + dx, y + dy, dx || dy ? '#c9d2ff' : '#ffffff');
  }
  return g;
}

// ---- fireflies (night) and butterflies (dawn) ----------------------------------

function fireflies() {
  const H = 110;
  const g = new Grid(W, H);
  const r = rng(77);
  for (let i = 0; i < 26; i++) {
    const x = Math.floor(r() * W);
    const y = 20 + Math.floor(r() * (H - 26));
    g.set(x, y, '#fff6c2');
    for (const [dx, dy] of [[1, 0], [-1, 0], [0, 1], [0, -1]]) g.set(x + dx, y + dy, '#ffe27a77');
  }
  return g;
}
// Butterflies (3.8, gd-wyucyyLuAz): eleven per tile, so about 11 per 1280 px
// of laptop meadow, 5 and 6 cells across (20-24 px at 4 px a cell, 15-18 at 3).
// Six fly low, 13 to 34 cells above the foot of the scene: on a phone the
// plate covers everything higher, and the strip below it is about 37 cells
// tall. They are spaced evenly round the tile, so any 130 cells of it (390 px
// at 3 px a cell) hold at least one. The other five fly 58 to 87 cells up,
// in the side gutters and over the hills a wide window shows.
const BUTTERFLY = {
  small: ['.c.c.', 'aacaa', 'aAcAa', '.BcB.', '.B.B.'],
  large: ['.c..c.', 'aaccaa', 'aAccAa', 'aBccBa', '.B..B.'],
};
function butterflies() {
  const H = 110;
  const g = new Grid(W, H);
  const r = rng(91);
  const cols = [['#f58fb0', '#c9608a'], ['#ffd23f', '#f2a30f'], ['#8bb7f0', '#5b86c9'], ['#f59a3c', '#c0611f'], ['#b39ae8', '#7d63c4']];
  const fly = (x, y, i) => {
    const [a, b] = cols[i % cols.length];
    const sprite = i % 2 ? BUTTERFLY.large : BUTTERFLY.small;
    g.stamp(sprite, x, y, { a, A: mix(a, '#ffffff', 0.55), B: b, c: '#3b2a24' });
  };
  const low = 6, high = 5;
  for (let i = 0; i < low; i++) fly(Math.floor((i * W) / low + r() * 12), H - 34 + Math.floor(r() * 18), i);
  for (let i = 0; i < high; i++) fly(Math.floor(((i + 0.5) * W) / high + r() * 16), H - 87 + Math.floor(r() * 30), low + i);
  return g;
}

function build(outDir) {
  const img = (n) => path.join(outDir, 'img', n);
  const sizes = {};
  const w = (n, g) => { sizes[n] = writeSvg(img(n), g); };
  const h = hills(), b = bed(), gr = grass();
  const dayCl = clouds(L.cloud, L.cloudD, 5);
  const nightCl = clouds('#3a4a86', '#2b3870', 5);
  w('hills-day.svg', h); w('hills-night.svg', toNight(h));
  w('bed-day.svg', b); w('bed-night.svg', toNight(b));
  w('grass-day.svg', gr); w('grass-night.svg', toNight(gr));
  w('clouds-day.svg', dayCl); w('clouds-night.svg', nightCl);
  w('sun.svg', sun()); w('moon.svg', moon()); w('stars.svg', stars());
  w('fireflies.svg', fireflies()); w('butterflies.svg', butterflies());
  return sizes;
}

module.exports = { build, clouds, sun, moon, stars, fireflies, blob };
