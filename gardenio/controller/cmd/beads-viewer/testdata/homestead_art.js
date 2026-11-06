/* The Homestead's generated pixel art (plan gd-JaMxRKsc8z, steps 3.0 gd-2skiMOyS7f, 3.1 gd-b2hzcYUTji and 3.4
   gd-WP5vygf7Ly, the season's vines and the mask sheets drawn from the meadow's own art): pixel maps in
   this file become the crispEdges SVGs under templates/static/homestead/img. The output is checked in; after editing a
   map, rerun from gardenio/controller/cmd/beads-viewer:
     node testdata/homestead_art.js templates/static/homestead/img
   Night files take /garden's night() formula (gardenhub/web/art.js: cooler, dimmer, bluer) except the stem, which is
   pewter at night, and the blossom, which stays lit. Textures, gems and figures are one file for both skins: textures
   are shade (light and dark ink at low alpha), gems are lit, figures are objects with their own colours. leaf.svg and
   flower.svg are hand-made masks, not generated here. TestHomesteadArt_UnderBudget keeps the folder under 60 KB. */
var fs = require('fs'), path = require('path');
var out = process.argv[2];
if (!out) { console.error('usage: node homestead_art.js <img dir>'); process.exit(2); }

function hex(c) { return [parseInt(c.substr(1, 2), 16), parseInt(c.substr(3, 2), 16), parseInt(c.substr(5, 2), 16)]; }
function toHex(r) { return '#' + r.map(function (v) { v = Math.max(0, Math.min(255, Math.round(v))); return (v < 16 ? '0' : '') + v.toString(16); }).join(''); }
function night(c) { var r = hex(c), l = (r[0] + r[1] + r[2]) / 3; return toHex([r[0] * .42 + l * .1 + 6, r[1] * .5 + l * .1 + 14, r[2] * .72 + l * .12 + 46]); }

/* Colours from /garden's palette (art.js DAY): g leaf, d deep leaf, n new leaf, s stem (bark), F blossom, Y its eye. */
var DAY = { g: '#5fae3e', d: '#458f36', n: '#a6ee7a', s: '#7e4c26', F: '#ff9ec4', Y: '#f2d44a' };
var NIGHT = { g: night(DAY.g), d: night(DAY.d), n: night(DAY.n), s: '#8e93a6', F: DAY.F, Y: DAY.Y };

/* One <path> per colour (paths). A run of a colour along a row is a rectangle, grown down while the rows below repeat it
   exactly (a stem is one tall rectangle), and every rectangle after a path's first moves relative to the one before
   (m dx dy), which keeps the numbers short; the season art (3.4) needed the bytes. width/height are set so
   border-image-slice numbers are image cells. A palette value may carry an alpha ("#000000/0.06": a texture's shade);
   views name the parts of a sprite sheet (url("sheet.svg#name") draws just that part). */
function paths(rows, pal) {
  var h = rows.length, w = rows[0].length, rects = {}, taken = {};
  function runAt(row, x, e, ch) {
    for (var i = x; i < e; i++) if (row[i] !== ch) return false;
    return row[x - 1] !== ch && row[e] !== ch;
  }
  rows.forEach(function (row, y) {
    if (row.length !== w) throw new Error('ragged map at row ' + y);
    for (var x = 0; x < w; x++) {
      var ch = row[x];
      if (ch === '.') continue;
      var e = x + 1;
      while (e < w && row[e] === ch) e++;
      if (!pal[ch]) throw new Error('no colour for ' + ch);
      if (!taken[y + ',' + x]) {
        var b = y + 1;
        while (b < h && runAt(rows[b], x, e, ch)) taken[(b++) + ',' + x] = true;
        (rects[pal[ch]] = rects[pal[ch]] || []).push([x, y, e - x, b - y]);
      }
      x = e - 1;
    }
  });
  function pair(a, b) { return a + (b < 0 ? '' : ' ') + b; }
  return Object.keys(rects).map(function (c) {
    var p = c.split('/'), d = rects[c].map(function (r, i, all) {
      var abs = 'M' + pair(r[0], r[1]), rel = i ? 'm' + pair(r[0] - all[i - 1][0], r[1] - all[i - 1][1]) : abs;
      var back = 'H' + r[0], left = 'h-' + r[2];
      return (rel.length < abs.length ? rel : abs) + 'h' + r[2] + 'v' + r[3] + (left.length < back.length ? left : back) + 'z';
    });
    return '<path fill="' + p[0] + '"' + (p[1] ? ' fill-opacity="' + p[1] + '"' : '') + ' d="' + d.join('') + '"/>';
  }).join('');
}
function open(w, h) {
  return '<svg xmlns="http://www.w3.org/2000/svg" width="' + w + '" height="' + h + '" viewBox="0 0 ' + w + ' ' + h + '" shape-rendering="crispEdges">';
}
function svg(rows, pal, views) {
  var v = Object.keys(views || {}).map(function (id) { return '<view id="' + id + '" viewBox="' + views[id].join(' ') + '"/>'; }).join('');
  return open(rows[0].length, rows.length) + v + paths(rows, pal) + '</svg>\n';
}
function blank(w, h) { var g = []; for (var y = 0; y < h; y++) g.push(new Array(w + 1).join('.').split('')); return g; }
function stamp(g, sprite, x0, y0) { sprite.forEach(function (row, y) { row.split('').forEach(function (ch, x) { if (ch !== '.') g[y0 + y][x0 + x] = ch; }); }); }
function rows(g) { return g.map(function (r) { return r.join(''); }); }
function flipX(r) { return r.map(function (s) { return s.split('').reverse().join(''); }); }
function flipY(r) { return r.slice().reverse(); }
function transpose(r) { var o = []; for (var x = 0; x < r[0].length; x++) { var s = ''; for (var y = 0; y < r.length; y++) s += r[y][x]; o.push(s); } return o; }

/* ---- The vine frame: a 9-slice for border-image -----------------------------------------------------------------
   56x56 cells, slice 12: four 12x12 grown-over corners and four 32-cell edge runs. The stem runs three cells in from
   the outer edge (so with a one-cell outset it sits just inside the plate's frame line), leaves and the tendril reach at
   most eight cells in, so the plate's padding clears them. The top run is drawn once; the left run is its
   transpose, the right and bottom are mirrors, so every stem meets its corner at the same cell. */
var LEAF_UP = ['.gn', 'ggg', 'dg.'];
var LEAF_DOWN = ['dg.', 'ggg', '.gn'];
var LEAF_UP_R = ['ng.', 'ggg', '.gd'];
var BLOSSOM = ['.F.', 'FYF', '.F.'];
var TENDRIL = ['s..', '.ss', '..s'];

function topRun() {
  var g = blank(32, 12), x;
  for (x = 0; x <= 13; x++) g[3][x] = 's';
  for (x = 13; x <= 22; x++) g[4][x] = 's';
  for (x = 22; x < 32; x++) g[3][x] = 's';
  stamp(g, LEAF_UP, 4, 0);
  stamp(g, LEAF_DOWN, 9, 4);
  stamp(g, ['.s.', '.s.'], 16, 2);   // the blossom's stalk
  stamp(g, BLOSSOM, 16, 0);
  stamp(g, TENDRIL, 19, 5);
  stamp(g, LEAF_UP_R, 25, 0);
  stamp(g, LEAF_DOWN, 28, 4);
  return rows(g);
}
/* The top-left corner: the stem comes in along row 3 from the right and turns down column 3, with a knot of leaves,
   a blossom and a curl overgrowing the turn and spilling one cell over the frame (the outset). */
var CORNER = [
  '.gn..ng.....',
  'gggggggg....',
  'dgFgdgd.....',
  'nFYFssssssss',
  'gdFs.s......',
  '.gss.s.gn...',
  'gnds.ss.....',
  '.g.s........',
  '..gs........',
  '.ggs........',
  '.dgs........',
  '...s........',
];

function frame() {
  var top = topRun(), left = transpose(top), g = blank(56, 56);
  stamp(g, CORNER, 0, 0);
  stamp(g, flipX(CORNER), 44, 0);
  stamp(g, flipY(CORNER), 0, 44);
  stamp(g, flipY(flipX(CORNER)), 44, 44);
  stamp(g, top, 12, 0);
  stamp(g, flipY(top), 12, 44);
  stamp(g, left, 0, 12);
  stamp(g, flipX(left), 44, 12);
  return rows(g);
}

/* The frame as the file draws it: the corner and the top run once each, and every other side placed by a mirror or a
   transpose (<use>), which is how frame() builds them. The same pixels as frame(), a third of the bytes; the season
   palettes (3.4) are four of these per skin. */
var FRAME_USES = { c: ['-1 0 0 1 56 0', '1 0 0 -1 0 56', '-1 0 0 -1 56 56'], t: ['1 0 0 -1 0 56', '0 1 1 0 0 0', '0 1 -1 0 56 0'] };
function frameParts() { var t = blank(44, 12); stamp(t, topRun(), 12, 0); return { c: CORNER, t: rows(t) }; }
function frameSvg(pal) {
  var parts = frameParts(), uses = '';
  Object.keys(FRAME_USES).forEach(function (id) {
    FRAME_USES[id].forEach(function (m) { uses += '<use href="#' + id + '" transform="matrix(' + m + ')"/>'; });
  });
  return open(56, 56) + '<g id="c">' + paths(parts.c, pal) + '</g><g id="t">' + paths(parts.t, pal) + '</g>' + uses + '</svg>\n';
}
/* The uses must land every cell where frame() puts it: place each part's cells through its matrices and compare. */
(function checkFrame() {
  var parts = frameParts(), g = blank(56, 56);
  Object.keys(parts).forEach(function (id) {
    ['1 0 0 1 0 0'].concat(FRAME_USES[id]).forEach(function (m) {
      var k = m.split(' ').map(Number);
      parts[id].forEach(function (row, y) {
        row.split('').forEach(function (ch, x) {
          if (ch === '.') return;
          var xs = [x, x + 1].map(function (a) { return [y, y + 1].map(function (b) { return k[0] * a + k[2] * b + k[4]; }); });
          var ys = [x, x + 1].map(function (a) { return [y, y + 1].map(function (b) { return k[1] * a + k[3] * b + k[5]; }); });
          g[Math.min.apply(null, ys[0].concat(ys[1]))][Math.min.apply(null, xs[0].concat(xs[1]))] = ch;
        });
      });
    });
  });
  if (rows(g).join('\n') !== frame().join('\n')) throw new Error('frameSvg places the vine differently from frame()');
})();

/* ---- The h1 vine rule: a 24x4 run under the page title (repeat-x) ------------------------------------------------ */
var RULE = [
  '..gn...........ng.......',
  '.ggg..F.......ggg.......',
  'ssdssFYFsssssssdssssssss',
  '......s..dg.............',
];


/* ---- Textures (3.1): three tiles of shade, one file each for both skins ------------------------------------------
   An SVG drawn as a CSS image cannot see the page's currentColor, so each tile paints in two inks at low alpha: a dark
   one that reads on day paper and a light one, a cell below or beside it like an emboss, that reads on night slate.
   TestHomesteadArt_TexturesStayFaint holds every ink to a swing of 6 in CIELAB lightness over both skins' grounds.
   Stone sits under section heads and the stats strip, earth under cards and the bead row, water under code and
   anything editable. */
var STONE = [
  '......d.........',
  '......dl........',
  '.......d........',
  '.......dl.......',
  'ddddddddddd.....',
  'lllllll.lldddddd',
  '..........dl.lll',
  '...........d....',
  '...........dl...',
  '.d..........d...',
  '.dl.........dl..',
  'ddddd........d..',
  'lllldddddddddddd',
  '...........lllll',
  '..l.............',
  '.....d......d...',
];
var EARTH = [
  '.......l........',
  '..ll...d.....d..',
  '..dd.........l..',
  '..........l.....',
  '.d.......ldd....',
  '.l..............',
  '.....ll.......d.',
  '....dddd......l.',
  '................',
  '.l........d.....',
  '.d.......ld.....',
  '...........l..d.',
  '......d.......l.',
  '..ll..l.........',
  '..dd.......ll...',
  '..........dd....',
];
var WATER = [
  '..lll...........',
  '.l...l..........',
  '.d...d....lll...',
  '..ddd....l...l..',
  '.........d...d..',
  '..........ddd...',
  '................',
  '................',
];
var TEXTURES = {
  stone: { map: STONE, pal: { d: '#000000/0.06', l: '#ffffff/0.05' } },
  earth: { map: EARTH, pal: { d: '#5a3a1a/0.085', l: '#fff3d6/0.05' } },
  water: { map: WATER, pal: { d: '#1d4f9a/0.09', l: '#ffffff/0.05' } },
};

/* ---- Moss and tendrils (3.1): one sheet, four named views, drawn at half a cell -----------------------------------
   tr creeps along a card's top edge from its right corner and drops a tendril down the side; bl grows along the foot
   from the left corner; end-l and end-r sit on the two ends of an h2's divider. They stay inside the padding they
   are drawn in (tr is 4 rows deep and 3 columns wide at the side), so no text sits on a leaf. */
var MOSS_TR = [
  '....g..ggngggdgg',
  '.......dg.ggdgng',
  '..........g..gds',
  '.............g.s',
  '..............s.',
  '.............gs.',
  '..............n.',
  '................',
];
var MOSS_BL = [
  '...g............',
  '.gdgg..g........',
  'gdggngdgg.g.....',
  'ggdgggggdggn.g..',
];
var MOSS_END_R = [
  '.........s.s..',
  '..........ss.n',
  '.....n...gs.gg',
  '...ggdg.ggdggd',
  'gggdggggdgggdg',
];
var MOSS_END_L = [
  '.n......',
  'ggd.g...',
  'dgggdgg.',
];
function sheet(parts) {
  var w = 0, h = 0, at = {}, g;
  parts.forEach(function (p) { at[p[0]] = [w, 0, p[1][0].length, p[1].length]; w += p[1][0].length; h = Math.max(h, p[1].length); });
  g = blank(w, h);
  parts.forEach(function (p) { stamp(g, p[1], at[p[0]][0], 0); });
  return { rows: rows(g), views: at };
}
var MOSS = sheet([['tr', MOSS_TR], ['bl', MOSS_BL], ['end-r', MOSS_END_R], ['end-l', MOSS_END_L]]);

/* ---- Gems (3.1): three hues, three frames each, in one tall strip -----------------------------------------------
   Each frame is a 5x5 slot (a 3x3 stone with room for the flare) at the top of a 12-row band, so any box up to 12
   cells tall shows exactly one frame and background-position-y picks it. Band k is hue * 3 + frame: ruby 0-2,
   emerald 3-5, sapphire 6-8; frame 0 at rest, 1 a glint, 2 the sparkle (gold arms, so it reads on day paper too). The gleam runs glint, sparkle, glint. The
   corner stones sit where the vine frame's blossom grows, so a gem covers that knot exactly. */
var GEM_FRAMES = [
  ['.....', '.LLM.', '.LMD.', '.MDD.', '.....'],
  ['.....', '.WLM.', '.LMD.', '.MDD.', '.....'],
  ['.S...', 'SWWM.', '.WMD.', '.MDD.', '.....'],
];
var GEM_HUES = [['a', 'b', 'c'], ['e', 'f', 'h'], ['i', 'j', 'k']];
var GEM_PAL = {
  a: '#ff9aa8', b: '#e2304c', c: '#8c1530',
  e: '#9af2b8', f: '#2fbf6a', h: '#15703a',
  i: '#a8d0ff', j: '#3a78e0', k: '#1c3c8c',
  W: '#ffffff', S: '#ffd23f',
};
function gems() {
  var out = [];
  GEM_HUES.forEach(function (hue) {
    GEM_FRAMES.forEach(function (f) {
      f.forEach(function (r) { out.push(r.replace(/L/g, hue[0]).replace(/M/g, hue[1]).replace(/D/g, hue[2])); });
      for (var i = 0; i < 7; i++) out.push('.....');
    });
  });
  return out;
}

/* ---- Figures (3.1): pewter and clay, 12x12 each, side by side --------------------------------------------------
   0 the gardener (clay, straw hat, a pewter trowel) at the foot of the plate, 1 the cat (pewter) by the blossom on a
   "nothing matched" empty state, 2 the watering can (pewter) by the sprout on a "nothing yet" one, 3 the robin
   (clay) on the sign. A pseudo-element one figure wide picks one with background-position-x; a wider band names it
   by its view (figures.svg#gardener). */
var GARDENER = [
  '....kkkk....',
  '...kcccck...',
  '.kkkkkkkkkk.',
  '....cccc....',
  '....KccK....',
  '....cccc....',
  '...gggggg...',
  '..cggggggco.',
  '...gdggdg.P.',
  '...gggggg.P.',
  '...CC..CC...',
  '..kkk..kkk..',
];
var CAT = [
  '..o....o....',
  '..oo..oo....',
  '..oPPPPo....',
  '..PKPPKP....',
  '..PPppPP....',
  '...PPPP.....',
  '..oPPPPo....',
  '.oPPppPPo...',
  '.PPPppPPP.o.',
  '.PPPPPPPP..o',
  '.oPPPPPPo.o.',
  '..oooooooo..',
];
var CAN = [
  '............',
  '...oooo.....',
  '..o....o....',
  '..o....o..oo',
  '.oPPPPPPo.oo',
  '.PppPPPPPPo.',
  '.PpPPPPPPo.w',
  '.PpPPPPPP...',
  '.PPPPPPPP.w.',
  '.PPPPPPPP...',
  '.oooooooo...',
  '............',
];
var ROBIN = [
  '............',
  '............',
  '............',
  '............',
  '....CCC.....',
  '...CcKCC....',
  '..YCcccC....',
  '...eecCCC...',
  '...eeeCCCC..',
  '....eeCCCkk.',
  '.....CCC...k',
  '.....k.k....',
];
var FIGURE_PAL = {
  p: '#d6d9e2', P: '#9ba0b3', o: '#5d6275',
  c: '#d99a62', C: '#b06a3a', k: '#6e3f22',
  K: '#2a1a14', g: '#5fae3e', d: '#458f36', Y: '#f2d44a', e: '#d94a3a', w: '#78c0f4',
};
function figures() {
  var g = blank(48, 12);
  [GARDENER, CAT, CAN, ROBIN].forEach(function (f, i) { stamp(g, f, i * 12, 0); });
  return rows(g);
}

/* ---- Season (3.4): the vine's four palettes ---------------------------------------------------------------------
   Spring is the vine as 3.0 drew it (DAY/NIGHT, vine-frame-day.svg); the other three recolour the same maps. g and d are
   the season's --gd-vine and --gd-vine-dark in homestead.css (TestHomesteadSeason_VineTokensMatchArt). Summer is fuller
   and deeper with a buttercup; autumn is amber and russet with a rosehip; winter is evergreen ivy with frost on the new
   leaves and a holly berry. Night is the same formula as the rest, stem pewter, the blossom lit. */
var SEASON_VINES = {
  summer: { g: '#3f9a3a', d: '#2c7a2e', n: '#8fd85e', s: DAY.s, F: '#ffd23f', Y: '#e07a1f' },
  autumn: { g: '#d98a2b', d: '#a8541e', n: '#f2c14e', s: DAY.s, F: '#d94a3a', Y: '#f2d44a' },
  winter: { g: '#4f8a6a', d: '#2f6a52', n: '#e4f0f4', s: DAY.s, F: '#d94a3a', Y: '#ffffff' },
};
function nightOf(p) { return { g: night(p.g), d: night(p.d), n: night(p.n), s: NIGHT.s, F: p.F, Y: p.Y }; }

/* ---- Season (3.4): the meadow's accents, one mask sheet per season ----------------------------------------------
   Drawn from the meadow's own pixels (../meadow/img, the chart theme's copies), so a petal sits on a leaf, snow on a
   ridge and an amber leaf where a green one was. The scene's layers are 320 cells wide and bottom-anchored: the hills
   130 tall, the bed 50 (its rows are the hills' rows 80-129, painted over them), the grass 26. A sheet is mask only
   (the page paints each band in the skin's ink, so day and night share one file), its bands stacked top to bottom,
   each as tall as the layer it lies on; a pseudo-element that tall shows one band by mask-position-y. */
function readArt(name) {
  var s = fs.readFileSync(path.join(__dirname, '../templates/static/meadow/img', name + '.svg'), 'utf8');
  var vb = s.match(/viewBox="0 0 (\d+) (\d+)"/), w = +vb[1], h = +vb[2], g = [], re = /<path fill="(#[0-9a-f]{6})" d="([^"]+)"/g, m;
  for (var y = 0; y < h; y++) g.push(new Array(w).fill(null));
  while ((m = re.exec(s))) m[2].split('z').forEach(function (sub) {
    if (!sub) return;
    var r = sub.match(/^M(\d+) (\d+)h(\d+)v(\d+)/);
    if (!r) throw new Error(name + '.svg: unexpected path ' + sub);
    for (var yy = +r[2]; yy < +r[2] + +r[4]; yy++) for (var xx = +r[1]; xx < +r[1] + +r[3]; xx++) g[yy][xx] = m[1];
  });
  return g;
}
var HILLS = readArt('hills-day'), BED = readArt('bed-day'), GRASS = readArt('grass-day'), SCENE_W = 320, BED_TOP = 80;
/* What each meadow colour is. A colour the maps do not name stops the run, so a change to the scene art is looked at. */
var KIND = {
  hills: { pine: ['#98aad2'], far: ['#cdd5ee', '#b9c6e6'], ridge: ['#a8d4b4'], mid: ['#8fc3a6'], edge: ['#8cc77a'],
    near: ['#6fb56a', '#559a58'], flower: ['#fffdf4', '#ffd23f', '#f58fb0', '#ffffff'] },
  bed: { leaf: ['#56a84f', '#2f7340', '#8fd08c', '#5eab63'], stalk: ['#3f8f4a'],
    flower: ['#ffd23f', '#f2a30f', '#6b3e1f', '#4a2a14', '#8b6bd6', '#5d46a8', '#e0443e', '#a62b33', '#fffdf4'],
    wood: ['#d8a468', '#a8743f', '#6b4a2b', '#e8c46a', '#3a2a14', '#d2574a', '#f2d36b'], soil: ['#b98862', '#a2724f', '#835a3e'] },
  grass: { blade: ['#4f9e55', '#7cc56e', '#357a47'], flower: ['#fffdf4', '#ffd23f', '#8b6bd6', '#f2a30f', '#f58fb0'] },
};
function kindOf(layer, c) {
  if (!c) return null;
  for (var k in KIND[layer]) if (KIND[layer][k].indexOf(c) >= 0) return k;
  throw new Error('season art: ' + layer + ' colour ' + c + ' has no kind');
}
/* At hills row y: what the bed shows there, else what the hills show (null where neither paints: the sky). */
function bedAt(x, y) { return y >= BED_TOP ? kindOf('bed', BED[y - BED_TOP][x]) : null; }
function hillAt(x, y) { return bedAt(x, y) ? null : kindOf('hills', HILLS[y][x]); }
function skyAt(x, y) { return y < 0 || (!bedAt(x, y) && !HILLS[y][x]); }
/* A fixed scatter: the same cells every run, so regenerating changes nothing. */
function chance(x, y, salt) {
  var h = Math.imul(x + 1, 374761393) ^ Math.imul(y + 1, 668265263) ^ Math.imul(salt, 362437);
  h = Math.imul(h ^ (h >>> 13), 1274126177);
  return ((h ^ (h >>> 16)) >>> 0) / 4294967296;
}
function band(h, paint) {
  var g = blank(SCENE_W, h);
  for (var y = 0; y < h; y++) for (var x = 0; x < SCENE_W; x++) if (paint(x, y)) g[y][x] = 'x';
  return rows(g);
}
/* Spring (on the bed layer): blossom on three leaf cells in ten, petals fallen on the soil and in the meadow grass, and
   a few on the air. */
function spring(x, y) {
  var b = bedAt(x, y), k = hillAt(x, y);
  if (b === 'leaf') return chance(x, y, 1) < 0.3;
  if (b === 'soil') return y === BED_TOP + 45 && chance(x, y, 2) < 0.1;
  if (k === 'near') return chance(x, y, 3) < 0.012;
  return !b && y >= 60 && y < 100 && chance(x, y, 4) < 0.0015;
}
/* Summer (on the grass layer): fuller grass, a third of the columns growing a blade four to ten cells tall from the
   sward, some leaning at the tip; the grass's own flowers are left showing. The sun climbs in CSS. */
function summerGrass() {
  var g = blank(SCENE_W, 26);
  for (var x = 0; x < SCENE_W; x++) {
    if (chance(x, 0, 8) >= 0.32) continue;
    var tall = 4 + Math.floor(chance(x, 1, 8) * 7), lean = chance(x, 2, 8) < 0.4 ? (chance(x, 3, 8) < 0.5 ? -1 : 1) : 0;
    for (var i = 0; i < tall; i++) {
      var y = 22 - i, xx = i >= tall - 2 ? x + lean : x;
      if (xx < 0 || xx >= SCENE_W || kindOf('grass', GRASS[y][xx]) === 'flower') continue;
      g[y][xx] = 'x';
    }
  }
  return rows(g);
}
/* How far down its own hill a cell is: 0 on the skyline of the far hills, the middle hills or the near hill (the pines
   count with the far hills), so the season can lie along the top of each; midTop is the middle hills' skyline row. */
var GROUP = { pine: 'far', far: 'far', ridge: 'mid', mid: 'mid', edge: 'near', near: 'near', flower: 'near' };
function midTop(x) { for (var y = 0; y < HILLS.length; y++) if (GROUP[kindOf('hills', HILLS[y][x])] === 'mid') return y; return -9; }
function depth(x, y) {
  var g = GROUP[kindOf('hills', HILLS[y][x])], d = 0;
  while (y - d > 0 && GROUP[kindOf('hills', HILLS[y - d - 1][x])] === g) d++;
  return d;
}
/* Autumn (on the bed layer), two inks: the bed's leaves turn, a plant or two at a time, amber or russet; along the
   top of the middle hills about every other stand of trees (a crown five cells wide, two tall) has turned too; leaves
   lie in the meadow grass and on the soil. */
function autumn(x, y) {
  var b = bedAt(x, y), k = hillAt(x, y), c = chance(Math.floor(x / 5), 0, 5) < 0.6 ? 'a' : 'b';
  if (b === 'leaf') return c;
  if (b === 'soil') return y <= BED_TOP + 46 && chance(x, y, 7) < 0.08 ? c : null;
  if (b) return null;
  var stand = Math.floor(x / 7), crown = [1, 2, 2, 2, 1][x % 7], top = midTop(x);
  if (crown && chance(stand, 1, 10) < 0.5 && y >= top - crown && y < top + 2) return chance(stand, 2, 10) < 0.65 ? 'a' : 'b';
  if (k === 'near') return chance(x, y, 6) < 0.02 ? (chance(x, y, 5) < 0.5 ? 'a' : 'b') : null;
  return null;
}
/* Winter (on the bed layer), snow and the bare bed: snow two cells deep along the top of every hill (the pines stand
   clear of it), on the top of every post, rail, stalk and plant in the bed and along the soil; under it the bed's
   leaves, stalks and flowers go to dry stems. The grass layer gets the snow line on the ground in front, with a few
   drifts. */
function winterSnow(x, y) {
  var b = bedAt(x, y), k = hillAt(x, y);
  if (b === 'soil') return y === BED_TOP + 45;
  if (b) return y === BED_TOP || !bedAt(x, y - 1);
  return !!k && k !== 'pine' && k !== 'flower' && depth(x, y) < 2;
}
function winterBare(x, y) { var b = bedAt(x, y); return b === 'leaf' || b === 'stalk' || b === 'flower'; }
function winterGround(x, y) { return y === 23 || (y === 24 && chance(x, y, 9) < 0.35); }

function writeSeasons() {
  ['summer', 'autumn', 'winter'].forEach(function (s) {
    fs.writeFileSync(path.join(out, 'vine-frame-' + s + '-day.svg'), frameSvg(SEASON_VINES[s]));
    fs.writeFileSync(path.join(out, 'vine-frame-' + s + '-night.svg'), frameSvg(nightOf(SEASON_VINES[s])));
    fs.writeFileSync(path.join(out, 'vine-rule-' + s + '-day.svg'), svg(RULE, SEASON_VINES[s]));
    fs.writeFileSync(path.join(out, 'vine-rule-' + s + '-night.svg'), svg(RULE, nightOf(SEASON_VINES[s])));
  });
  var MASK = { x: '#000000' };
  fs.writeFileSync(path.join(out, 'season-spring.svg'), svg(band(130, spring), MASK));
  fs.writeFileSync(path.join(out, 'season-summer.svg'), svg(summerGrass(), MASK));
  fs.writeFileSync(path.join(out, 'season-autumn.svg'), svg(band(130, function (x, y) { return autumn(x, y) === 'a'; })
    .concat(band(130, function (x, y) { return autumn(x, y) === 'b'; })), MASK));
  fs.writeFileSync(path.join(out, 'season-winter.svg'), svg(band(130, winterSnow).concat(band(130, winterBare), band(26, winterGround)), MASK));
}

function write(name, map, views) {
  fs.writeFileSync(path.join(out, name + '-day.svg'), svg(map, DAY, views));
  fs.writeFileSync(path.join(out, name + '-night.svg'), svg(map, NIGHT, views));
}
fs.mkdirSync(out, { recursive: true });
fs.writeFileSync(path.join(out, 'vine-frame-day.svg'), frameSvg(DAY));
fs.writeFileSync(path.join(out, 'vine-frame-night.svg'), frameSvg(NIGHT));
write('vine-rule', RULE);
write('moss', MOSS.rows, MOSS.views);
Object.keys(TEXTURES).forEach(function (n) { fs.writeFileSync(path.join(out, n + '.svg'), svg(TEXTURES[n].map, TEXTURES[n].pal)); });
fs.writeFileSync(path.join(out, 'gems.svg'), svg(gems(), GEM_PAL));
fs.writeFileSync(path.join(out, 'figures.svg'), svg(figures(), FIGURE_PAL,
  { gardener: [0, 0, 12, 12], cat: [12, 0, 12, 12], can: [24, 0, 12, 12], robin: [36, 0, 12, 12] }));
writeSeasons();
var total = 0;
fs.readdirSync(out).sort().forEach(function (f) { var n = fs.statSync(path.join(out, f)).size; total += n; console.log(f, n); });
console.log('total', total, 'of 61440');
