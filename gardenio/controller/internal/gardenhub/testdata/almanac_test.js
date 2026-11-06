/* The almanac (web/almanac.js, plan gd-EJGA3zDGCk step 2), run by TestAlmanacUnderNode:  node almanac_test.js <web dir>
   The sun at solar noon on the June solstice at 45 N stands about 68.4 degrees high and due south, and rises in the east and
   sets in the west; 26 Oct 2026 is a full moon, standing opposite the sun, and the phase is new again a synodic month after a
   new moon; direction() is a unit vector, y up and north along -z; twilight() names each band; place() reads ?lat=&lon= and
   otherwise guesses the longitude from the timezone; skyPalette() has no step from afternoon through dusk into moonlit night
   and back at dawn. Prints a JSON report. */
"use strict";
var path = require("path"), assert = require("assert");
var A = require(path.join(process.argv[2], "almanac.js"));
var H = 3600000, near = function (a, b, tol, what) { assert.ok(Math.abs(a - b) <= tol, what + ": " + a + ", want " + b + " +- " + tol); };

// the sun
var solstice = Date.UTC(2026, 5, 21), noon = A.noon(solstice, 0), s = A.sun(noon, 45, 0);
near(s.elevation, 68.4, 0.1, "solstice noon elevation at 45 N");
near(s.azimuth, 180, 0.01, "the sun is due south at solar noon");
near((noon - solstice) / 60000, 12 * 60 + 1.8, 1, "solar noon at 0 E on 21 June, in minutes after midnight UTC (equation of time)");
assert.ok(A.sun(noon - 0.1 * H, 45, 0).elevation < s.elevation && A.sun(noon + 0.1 * H, 45, 0).elevation < s.elevation, "noon is the day's highest");
near(A.sun(new Date(noon), 45, 0).elevation, s.elevation, 1e-9, "a Date reads as its unix ms");
near(A.sun(A.noon(Date.UTC(2026, 11, 21), 0), 45, 0).elevation, 21.6, 0.1, "the December solstice's noon at 45 N");
near(A.sun(A.noon(Date.UTC(2026, 2, 20), 0), 0, 0).elevation, 90, 0.5, "overhead at the equator on the March equinox");
var morning = A.sun(noon - 6 * H, 45, 0), evening = A.sun(noon + 6 * H, 45, 0);
assert.ok(morning.azimuth > 45 && morning.azimuth < 135 && evening.azimuth > 225 && evening.azimuth < 315,
  "east in the morning, west in the evening: " + morning.azimuth + ", " + evening.azimuth);
assert.ok(A.sun(noon + 12 * H, 45, 0).elevation < -20, "midnight is dark");
// longitude moves noon: 15 degrees east is an hour earlier
near((A.noon(solstice, 0) - A.noon(solstice, 15)) / 60000, 60, 0.5, "15 degrees east, an hour earlier");

// the moon
var full = A.moon(Date.UTC(2026, 9, 26, 12), 45, 0);
assert.strictEqual(full.phase, "full");
assert.ok(full.fraction > 0.98, "26 Oct 2026 is a full moon: fraction " + full.fraction);
near(A.moon(A.NEW_MOON, 45, 0).fraction, 0, 1e-9, "the epoch is a new moon");
var epochPlus = A.moon(A.NEW_MOON + A.SYNODIC * 24 * H, 45, 0);
assert.ok(epochPlus.age < 1e-6 || A.SYNODIC - epochPlus.age < 1e-6, "new again after 29.53 days: age " + epochPlus.age);
assert.strictEqual(epochPlus.phase, "new");
var cycle = [], last = -1;
for (var d = 0; d < 30; d++) {
  var m = A.moon(A.NEW_MOON + (d + 0.25) * 24 * H, 45, 0);
  assert.ok(m.age >= 0 && m.age < A.SYNODIC, "age within a month");
  if (cycle[cycle.length - 1] !== m.phase) cycle.push(m.phase);
  if (d < 29) assert.ok(m.age > last, "the age grows through the month"); last = m.age;
}
assert.deepStrictEqual(cycle.slice(0, 8), A.PHASES, "the phases in order");
near(A.moon(A.NEW_MOON + 7.38 * 24 * H, 45, 0).fraction, 0.5, 0.01, "half lit at first quarter");
// a full moon stands opposite the sun: up when the sun is down, and rising near sunset
var opp = -Infinity, n = 0, rise = null, set = null, prev = null;
for (var t = Date.UTC(2026, 9, 26, 0); t < Date.UTC(2026, 9, 27, 0); t += 60000) {
  var sn = A.sun(t, 45, 0), mn = A.moon(t, 45, 0);
  var dot = A.direction(sn.elevation, sn.azimuth).reduce(function (a, v, i) { return a + v * A.direction(mn.elevation, mn.azimuth)[i]; }, 0);
  opp = Math.max(opp, dot); n++;
  if (prev && prev.s.elevation >= 0 && sn.elevation < 0) set = t;
  if (prev && prev.m.elevation < 0 && mn.elevation >= 0) rise = t;
  prev = { s: sn, m: mn };
}
assert.ok(opp < -0.95, "the full moon is never less than about 160 degrees from the sun: max cos " + opp);
assert.ok(set && rise && Math.abs(rise - set) < 1.5 * H, "the full moon rises at sunset: set " + new Date(set).toISOString() + ", rise " + new Date(rise).toISOString());

// directions
function len(v) { return Math.hypot(v[0], v[1], v[2]); }
var up = A.direction(90, 123);
near(up[0], 0, 1e-12, "zenith x"); near(up[1], 1, 1e-12, "zenith y"); near(up[2], 0, 1e-12, "zenith z");
assert.deepStrictEqual(A.direction(0, 0).map(function (v) { return Math.round(v) + 0; }), [0, 0, -1], "north is -z");
assert.deepStrictEqual(A.direction(0, 90).map(function (v) { return Math.round(v) + 0; }), [1, 0, 0], "east is +x");
assert.deepStrictEqual(A.direction(0, 180).map(function (v) { return Math.round(v) + 0; }), [0, 0, 1], "south is +z");
for (var e = -90; e <= 90; e += 15) for (var a = 0; a < 360; a += 30) near(len(A.direction(e, a)), 1, 1e-12, "unit at " + e + "," + a);

// twilight
assert.deepStrictEqual([30, 6, 3, -0.5, -1, -6, -7, -12, -13, -18, -19].map(A.twilight),
  ["day", "day", "golden", "golden", "civil", "civil", "nautical", "nautical", "astronomical", "astronomical", "night"]);

// place
assert.deepStrictEqual(A.place("?lat=51.5&lon=-0.1", 0), { lat: 51.5, lon: -0.1 });
assert.deepStrictEqual(A.place("", 0), { lat: 45, lon: 0 }, "45 N by default");
assert.deepStrictEqual(A.place("?t=1", 300), { lat: 45, lon: -75 }, "UTC-5 is 75 W");
assert.deepStrictEqual(A.place("", -60), { lat: 45, lon: 15 }, "UTC+1 is 15 E");
assert.deepStrictEqual(A.place("?lat=95&lon=200", 0), { lat: 90, lon: -160 }, "clamped and wrapped");
assert.deepStrictEqual(A.place("?lat=abc", 0), { lat: 45, lon: 0 }, "an unreadable value is ignored");
// the guessed longitude puts solar noon near local noon
var guess = A.place("", 300), local = (A.noon(solstice, guess.lon) - solstice) / H - 5;
near(local, 12, 0.1, "solar noon on the clock at UTC-5");

// the palette: no step anywhere through a day, from noon through dusk into the moonlit night and back at dawn. Every colour
// channel, the light (its colour times its strength, full sun 255) and the ambient move at most 3/255 in any 10 s, where the
// sky's steepest real change (the horizon reddening at dusk) is about 1.3/255.
function chans(p) {
  var lit = A.rgb(p.light).map(function (c) { return c * p.strength / A.SUN_POWER; });
  return [].concat(A.rgb(p.zenith), A.rgb(p.horizon), A.rgb(p.ground), A.rgb(p.fog), lit, [p.ambient * 255]);
}
var worst = 0, sources = {}, bands = {}, step = 10000, p0 = null;
for (var t = Date.UTC(2026, 9, 26, 12); t < Date.UTC(2026, 9, 27, 12); t += step) {
  var p = A.skyPalette(t, 45, 0), c = chans(p);
  sources[p.source] = 1; bands[p.twilight] = 1;
  ["zenith", "horizon", "ground", "fog", "light"].forEach(function (k) { assert.ok(p[k] >= 0 && p[k] <= 0xffffff && p[k] === Math.round(p[k]), k + " is a colour"); });
  near(len(p.direction), 1, 1e-9, "the light's direction is a unit vector");
  if (p0) {
    for (var i = 0; i < c.length; i++) worst = Math.max(worst, Math.abs(c[i] - p0.c[i]));
    if (p.source !== p0.p.source) assert.ok(p.strength < 0.01 && p0.p.strength < 0.01, "the light hands over in the dark: " + p0.p.strength + " -> " + p.strength);
  }
  p0 = { p: p, c: c };
}
assert.ok(worst <= 3, "no channel moves more than 3/255 in 10 s across the day: " + worst);
assert.deepStrictEqual(Object.keys(sources).sort(), ["moon", "sun"], "the sun by day and the moon by night");
assert.deepStrictEqual(Object.keys(bands).sort(), ["astronomical", "civil", "day", "golden", "nautical", "night"]);
var day = A.skyPalette(noon, 45, 0), night = A.skyPalette(Date.UTC(2026, 9, 27, 0), 45, 0);
var dark = A.skyPalette(A.NEW_MOON + 0.5 * 24 * H, 45, 0); // a new moon's midnight is near 6 UTC at 0 E; this is 6:14 UTC
assert.strictEqual(day.source, "sun"); assert.strictEqual(night.source, "moon");
assert.ok(day.strength > 2 && night.strength > 0.1 && night.strength < day.strength / 5, "moonlight is far dimmer than sunlight: " + night.strength);
assert.ok(dark.strength < 0.01, "no moonlight at a new moon: " + dark.strength);
var lum = function (h) { var c = A.rgb(h); return c[0] + c[1] + c[2]; };
assert.ok(lum(night.zenith) > lum(dark.zenith), "a full moon brightens the night sky");
assert.ok(A.rgb(day.zenith)[2] > A.rgb(day.zenith)[0] && A.rgb(night.light)[2] > A.rgb(night.light)[0], "a blue sky by day and a blue moonlight");
var low = A.skyPalette(A.noon(solstice, 0) + 7.5 * H, 45, 0);
assert.strictEqual(low.twilight, "golden");
assert.ok(A.rgb(low.light)[0] > A.rgb(low.light)[2] + 60, "the golden hour's light is warm: " + low.light.toString(16));

// the sky in words, for the splash's screen reader name: the hour, where the sun stands, the moon while it is up
assert.strictEqual(A.compass(251), "west"); assert.strictEqual(A.compass(-10), "north"); assert.strictEqual(A.compass(110), "east");
[[Date.UTC(2026, 9, 26, 12), { when: "Midday", sun: "the sun high in the south", moon: "" }],
  [Date.UTC(2026, 9, 26, 9), { when: "Morning", sun: "the sun in the south-east", moon: "" }],
  [Date.UTC(2026, 9, 26, 16, 45), { when: "Dusk", sun: "the sun low in the west", moon: "" }],
  [Date.UTC(2026, 9, 26, 17, 15), { when: "Dusk", sun: "the sun just below the horizon in the west", moon: "a full moon, 100 percent, rising in the east" }],
  [Date.UTC(2026, 9, 26, 0), { when: "Night", sun: "", moon: "a full moon, 100 percent, high in the south" }],
  [Date.UTC(2026, 9, 26, 6, 45), { when: "Dawn", sun: "the sun low in the east", moon: "a full moon, 100 percent, setting in the west" }],
  [Date.UTC(2026, 9, 22, 16, 30), { when: "Dusk", sun: "the sun low in the west", moon: "a waxing gibbous moon, 87 percent, rising in the east" }],
  [A.NEW_MOON + 0.5 * 24 * H, { when: "Night", sun: "", moon: "" }]
].forEach(function (c) { assert.deepStrictEqual(A.words(A.skyPalette(c[0], 45, 0)), c[1], new Date(c[0]).toISOString()); });

console.log(JSON.stringify({
  noon: new Date(noon).toISOString(), elevation: +s.elevation.toFixed(2), fullMoon: +full.fraction.toFixed(4),
  moonrise: new Date(rise).toISOString(), sunset: new Date(set).toISOString(), worstStep: worst
}));
