/* The pure part of web/tunes.js under node (TestTunesLogicUnderNode): fmt, shuffle, resolve, step, tracksOf and the panel's
   one rule. The playlist here is made up; the base is example.invalid. */
'use strict';
var assert = require('assert'), fs = require('fs'), vm = require('vm');
var src = fs.readFileSync(process.argv[2], 'utf8');
var ctx = {};
vm.runInNewContext(src, ctx);
var T = ctx.TUNES;
/* arrays come from the script's own realm, so compare their JSON, not their prototypes */
function same(a, b, msg) { assert.strictEqual(JSON.stringify(a), JSON.stringify(b), msg); }
assert.ok(T && typeof T.mount === 'undefined', 'under node only the pure part is built');

assert.strictEqual(T.fmt(0), '0:00');
assert.strictEqual(T.fmt(65), '1:05');
assert.strictEqual(T.fmt(162), '2:42');
assert.strictEqual(T.fmt(3600 + 7 * 60 + 9), '1:07:09');
assert.strictEqual(T.fmt(NaN), '');
assert.strictEqual(T.fmt(-1), '');

var seq = [0.1, 0.9, 0.5, 0.3, 0.7], k = 0;
function rnd() { return seq[k++ % seq.length]; }
var order = T.shuffle(5, rnd);
same(Array.prototype.slice.call(order).sort(), [0, 1, 2, 3, 4], 'a shuffle is a permutation');
k = 0;
same(T.shuffle(5, rnd), order, 'the same draws give the same order');
same(T.shuffle(0, rnd), []);
same(T.shuffle(1, rnd), [0]);

var BASE = 'https://hub.example.invalid/';
assert.strictEqual(T.resolve(BASE, '/audio/x.mp3'), 'https://hub.example.invalid/audio/x.mp3');
assert.strictEqual(T.resolve('https://hub.example.invalid', 'thumbs/a.jpg'), 'https://hub.example.invalid/thumbs/a.jpg');
assert.strictEqual(T.resolve(BASE, 'https://cdn.example.invalid/x.mp3'), 'https://cdn.example.invalid/x.mp3', 'an absolute URL is left alone');
assert.strictEqual(T.resolve(BASE, T.LIST), 'https://hub.example.invalid/playlist.json', 'the playlist sits at the base');

var list = { tracks: [
  { id: 't-alpha', kind: 'song', title: 'Alpha Test Tone', src: '/audio/alpha.mp3', href: '/w/t-alpha/', duration_s: 61 },
  { id: 't-beta', kind: 'song', title: 'Beta Test Tone', src: '/audio/beta.mp3', duration_s: 75 },
  { id: '', title: 'no id', src: '/audio/x.mp3' },
  { id: 't-nosrc', title: 'no src' },
  null,
] };
same(T.tracksOf(list).map(function (t) { return t.id; }), ['t-alpha', 't-beta'], 'only tracks with an id, a title and a src play');
same(T.tracksOf(null), [], 'a failed fetch: nothing to play');
same(T.tracksOf({}), []);
same(T.tracksOf({ tracks: 'nope' }), []);
same(T.tracksOf({ tracks: [] }), []);

var o = [2, 0, 1];
assert.strictEqual(T.step(o, 2, 1), 0);
assert.strictEqual(T.step(o, 1, 1), 2, 'wraps forward');
assert.strictEqual(T.step(o, 2, -1), 1, 'wraps back');
assert.strictEqual(T.step(o, -1, 1), 2, 'nothing playing yet: the first in the order');
assert.strictEqual(T.step([], 0, 1), -1);

assert.strictEqual(T.showPanel({ playing: true }), true);
assert.strictEqual(T.showPanel({ playing: false }), false, 'paused: the panel folds away');
assert.strictEqual(T.showPanel(null), false);
console.log(JSON.stringify({ order: order }));
