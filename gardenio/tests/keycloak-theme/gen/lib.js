// Tiny pixel-art toolkit for the login scene (drawn for the S1g mockups, shipped by S1h). No dependencies.
//
// Every scene is drawn on an integer grid and written out as an SVG of
// run-length rectangles with shape-rendering="crispEdges", so it scales to any
// whole multiple of its size without blur (the theme displays one grid cell as
// 3 or 4 CSS pixels). All art here is original to gardenio: shapes are
// generated from seeded noise and hand-written sprite maps in this repo.
'use strict';
const fs = require('fs');
const path = require('path');

// ---- random and colour helpers -------------------------------------------

function rng(seed) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const hex2rgb = (h) => {
  const s = h.replace('#', '');
  return [0, 2, 4].map((i) => parseInt(s.slice(i, i + 2), 16));
};
const rgb2hex = (r) => '#' + r.map((v) => Math.max(0, Math.min(255, Math.round(v))).toString(16).padStart(2, '0')).join('');
const mix = (a, b, t) => {
  const x = hex2rgb(a);
  const y = hex2rgb(b);
  return rgb2hex(x.map((v, i) => v + (y[i] - v) * t));
};

// ---- the grid -------------------------------------------------------------

class Grid {
  // wrapX: x wraps around, so tiles repeat without a seam.
  constructor(w, h, { wrapX = true } = {}) {
    this.w = w;
    this.h = h;
    this.wrapX = wrapX;
    this.a = new Array(w * h).fill(null);
  }
  set(x, y, c) {
    x = Math.round(x);
    y = Math.round(y);
    if (y < 0 || y >= this.h) return;
    if (this.wrapX) x = ((x % this.w) + this.w) % this.w;
    else if (x < 0 || x >= this.w) return;
    this.a[y * this.w + x] = c;
  }
  get(x, y) {
    if (y < 0 || y >= this.h) return null;
    if (this.wrapX) x = ((x % this.w) + this.w) % this.w;
    else if (x < 0 || x >= this.w) return null;
    return this.a[y * this.w + x];
  }
  rect(x, y, w, h, c) {
    for (let j = 0; j < h; j++) for (let i = 0; i < w; i++) this.set(x + i, y + j, c);
  }
  // Fill column x from y0 down to the bottom edge.
  col(x, y0, c, y1 = this.h) {
    for (let y = Math.max(0, Math.round(y0)); y < y1; y++) this.set(x, y, c);
  }
  // Filled ellipse.
  disc(cx, cy, rx, ry, c) {
    for (let y = Math.floor(cy - ry); y <= Math.ceil(cy + ry); y++) {
      for (let x = Math.floor(cx - rx); x <= Math.ceil(cx + rx); x++) {
        const dx = (x + 0.5 - cx) / rx;
        const dy = (y + 0.5 - cy) / ry;
        if (dx * dx + dy * dy <= 1) this.set(x, y, c);
      }
    }
  }
  // sprite: array of strings; '.' and ' ' are transparent. legend: char -> colour.
  stamp(sprite, x, y, legend, { flip = false } = {}) {
    sprite.forEach((row, j) => {
      const cells = flip ? [...row].reverse() : [...row];
      cells.forEach((ch, i) => {
        if (ch === '.' || ch === ' ') return;
        const c = legend[ch];
        if (c) this.set(x + i, y + j, c);
      });
    });
  }
  // Recolour cells: fn(colour, x, y) -> colour | null (null erases).
  map(fn) {
    for (let y = 0; y < this.h; y++) {
      for (let x = 0; x < this.w; x++) {
        const c = this.a[y * this.w + x];
        if (c) this.a[y * this.w + x] = fn(c, x, y);
      }
    }
  }
  // Pixels of colour `c` whose upper neighbour is empty (the lit rim of a shape).
  rim(c, rimColour) {
    const edges = [];
    for (let y = 0; y < this.h; y++) {
      for (let x = 0; x < this.w; x++) {
        if (this.get(x, y) === c && !this.get(x, y - 1)) edges.push([x, y]);
      }
    }
    edges.forEach(([x, y]) => this.set(x, y, rimColour));
  }
  // Transform every colour (day -> night, etc.), returning a new grid.
  recolor(fn) {
    const g = new Grid(this.w, this.h, { wrapX: this.wrapX });
    g.a = this.a.map((c) => (c ? fn(c) : c));
    return g;
  }
  // Pixels in a w x h block of the sky/colour bands of the scene, merged.
  svg() {
    // colour -> rows -> runs, then merge identical runs on consecutive rows.
    const byColour = new Map();
    for (let y = 0; y < this.h; y++) {
      let x = 0;
      while (x < this.w) {
        const c = this.a[y * this.w + x];
        if (!c) {
          x++;
          continue;
        }
        let x1 = x + 1;
        while (x1 < this.w && this.a[y * this.w + x1] === c) x1++;
        if (!byColour.has(c)) byColour.set(c, []);
        byColour.get(c).push([x, y, x1 - x]);
        x = x1;
      }
    }
    const parts = [];
    for (const [c, runs] of byColour) {
      const open = new Map(); // "x:len" -> [x, y0, len, y1]
      const done = [];
      let lastY = -2;
      const flushBefore = (y) => {
        for (const [k, r] of open) {
          if (r[3] < y - 1) {
            done.push(r);
            open.delete(k);
          }
        }
      };
      for (const [x, y, len] of runs) {
        if (y !== lastY) {
          flushBefore(y);
          lastY = y;
        }
        const k = `${x}:${len}`;
        const o = open.get(k);
        if (o && o[3] === y - 1) o[3] = y;
        else {
          if (o) done.push(o);
          open.set(k, [x, y, len, y]);
        }
      }
      done.push(...open.values());
      const d = done.map(([x, y0, len, y1]) => `M${x} ${y0}h${len}v${y1 - y0 + 1}h-${len}z`).join('');
      const rgba = c.length === 9;
      const fill = rgba ? c.slice(0, 7) : c;
      const op = rgba ? (parseInt(c.slice(7, 9), 16) / 255).toFixed(2).replace(/0+$/, '') : null;
      parts.push(`<path fill="${fill}"${op ? ` fill-opacity="${op}"` : ''} d="${d}"/>`);
    }
    return (
      `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${this.w} ${this.h}" width="${this.w}" height="${this.h}" ` +
      `shape-rendering="crispEdges">${parts.join('')}</svg>\n`
    );
  }
}

// Periodic height field: sum of sines whose periods divide the tile width, so
// the tile joins itself. Returns a function x -> value in roughly [-1, 1].
function wave(w, rand, terms = 4) {
  const parts = [];
  for (let k = 1; k <= terms; k++) {
    parts.push({ f: k + Math.floor(rand() * 2), a: 1 / k ** 0.8, p: rand() * Math.PI * 2 });
  }
  const norm = parts.reduce((s, p) => s + p.a, 0);
  return (x) => parts.reduce((s, p) => s + p.a * Math.sin((2 * Math.PI * p.f * x) / w + p.p), 0) / norm;
}

function writeSvg(file, grid) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  const svg = grid.svg();
  fs.writeFileSync(file, svg);
  return svg.length;
}

module.exports = { Grid, rng, wave, mix, hex2rgb, rgb2hex, writeSvg };
