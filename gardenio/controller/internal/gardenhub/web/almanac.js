/* ALMANAC: where the sun and the moon stand, and what the sky looks like, at a moment (plan gd-EJGA3zDGCk, step 2). The splash
   lights its scene from this: one directional light placed along direction(), the sky, ground light and fog from skyPalette().
   Pure: no DOM and no three.js, so TestAlmanacUnderNode runs it under node (testdata/almanac_test.js). Times are UTC, as unix
   ms or a Date. Angles in and out are degrees: elevation above the horizon, azimuth from north towards east (south is 180).
   The sun comes from the low-precision solar coordinates (good to about 0.01 degree this century). The moon is cruder: its age
   from a mean new moon and the synodic month (good to about half a day), and its place the sun's carried round the ecliptic by
   the phase angle, so a full moon rises at sunset and stands opposite the sun; it ignores the moon's 5 degree tilt. */
var ALMANAC = (function () {
  "use strict";
  var LAT = 45, DAY = 86400000, J2000 = 946728000000; // 2000-01-01 12:00 UTC
  var SYNODIC = 29.530589, NEW_MOON = Date.UTC(2000, 0, 6, 18, 14); // a known new moon
  var PHASES = ["new", "waxing crescent", "first quarter", "waxing gibbous", "full", "waning gibbous", "last quarter", "waning crescent"];
  var RAD = Math.PI / 180;

  function ms(t) { return typeof t === "number" ? t : t.getTime(); }
  function wrap(deg) { deg %= 360; return deg < 0 ? deg + 360 : deg; }
  function clamp(x, lo, hi) { return x < lo ? lo : x > hi ? hi : x; }
  function smooth(e0, e1, x) { var k = clamp((x - e0) / (e1 - e0), 0, 1); return k * k * (3 - 2 * k); }

  /* ecliptic gives the sun's ecliptic longitude L, the obliquity e and the Greenwich sidereal angle at unix ms t, in degrees. */
  function ecliptic(t) {
    var d = (t - J2000) / DAY;
    var g = (357.529 + 0.98560028 * d) * RAD, q = 280.459 + 0.98564736 * d;
    return {
      L: wrap(q + 1.915 * Math.sin(g) + 0.020 * Math.sin(2 * g)),
      e: 23.439 - 0.00000036 * d,
      gmst: wrap(280.46061837 + 360.98564736629 * d),
      q: wrap(q)
    };
  }

  /* sky places a body at ecliptic longitude L (on the ecliptic) for an observer at lat, lon at time ec: its declination, its
     hour angle, and its elevation and azimuth. */
  function sky(ec, L, lat, lon) {
    var e = ec.e * RAD, l = L * RAD, p = lat * RAD;
    var ra = Math.atan2(Math.cos(e) * Math.sin(l), Math.cos(l)) / RAD, dec = Math.asin(Math.sin(e) * Math.sin(l));
    var h = wrap(ec.gmst + lon - ra), H = h * RAD;
    var el = Math.asin(clamp(Math.sin(p) * Math.sin(dec) + Math.cos(p) * Math.cos(dec) * Math.cos(H), -1, 1));
    var az = Math.atan2(-Math.cos(dec) * Math.sin(H), Math.sin(dec) * Math.cos(p) - Math.cos(dec) * Math.cos(H) * Math.sin(p));
    return { elevation: el / RAD, azimuth: wrap(az / RAD), declination: dec / RAD, hourAngle: h > 180 ? h - 360 : h, ra: wrap(ra) };
  }

  /* sun(t, lat, lon) -> {elevation, azimuth, declination, hourAngle}: the sun from latitude lat (north +) and longitude lon
     (east +), its declination and its hour angle (0 at solar noon, + in the afternoon). */
  function sun(t, lat, lon) {
    var ec = ecliptic(ms(t));
    return sky(ec, ec.L, lat == null ? LAT : lat, lon || 0);
  }

  /* noon(t, lon) -> unix ms of the solar noon at longitude lon on t's UTC day: noon UTC moved by the longitude and by the
     equation of time. */
  function noon(t, lon) {
    var day = Math.floor(ms(t) / DAY) * DAY, n = day + DAY / 2 - (lon || 0) / 360 * DAY;
    for (var i = 0; i < 3; i++) n -= sun(n, 0, lon || 0).hourAngle / 360 * DAY * 0.99727;
    return n;
  }

  /* moon(t, lat, lon) -> {age, fraction, phase, angle, elevation, azimuth}: the moon's age in days since the last new moon, its
     lit fraction, its phase's name (one of PHASES), the phase angle (0 new, 180 full) and its approximate place in the sky. */
  function moon(t, lat, lon) {
    t = ms(t);
    var f = ((t - NEW_MOON) / DAY / SYNODIC) % 1;
    if (f < 0) f += 1;
    var angle = f * 360, ec = ecliptic(t), s = sky(ec, wrap(ec.L + angle), lat == null ? LAT : lat, lon || 0);
    return {
      age: f * SYNODIC, fraction: (1 - Math.cos(angle * RAD)) / 2, phase: PHASES[Math.floor(f * 8 + 0.5) % 8], angle: angle,
      elevation: s.elevation, azimuth: s.azimuth, declination: s.declination
    };
  }

  /* direction(elevation, azimuth) -> [x, y, z]: the unit vector towards that point of the sky in the scene's frame, y up, north
     along -z and east along +x. A directional light placed at target + direction * distance shines from that body. */
  function direction(elevation, azimuth) {
    var el = elevation * RAD, az = azimuth * RAD, c = Math.cos(el);
    return [c * Math.sin(az), Math.sin(el), -c * Math.cos(az)];
  }

  /* twilight(sunElevation) names the light: day, golden (the sun low, under 6 degrees), civil (set, to -6), nautical (to -12),
     astronomical (to -18), night. The sun counts as set when its upper limb is down, at -0.833 degrees after refraction. */
  function twilight(el) {
    return el >= 6 ? "day" : el >= -0.833 ? "golden" : el >= -6 ? "civil" : el >= -12 ? "nautical" : el >= -18 ? "astronomical" : "night";
  }

  /* place(search, tzOffset) -> {lat, lon}: ?lat= and ?lon= from a location.search, else 45 N and the longitude whose solar noon
     is noon on the viewer's clock, 15 degrees per hour of Date.getTimezoneOffset() (minutes, + west of UTC). */
  function place(search, tzOffset) {
    function q(k) { var m = new RegExp("[?&]" + k + "=([^&#]*)").exec(search || ""); return m ? parseFloat(decodeURIComponent(m[1])) : NaN; }
    var lat = q("lat"), lon = q("lon");
    if (!isFinite(lat)) lat = LAT;
    if (!isFinite(lon)) lon = (tzOffset || 0) / -4 + 0;
    return { lat: clamp(lat, -90, 90), lon: lon < -180 || lon > 180 ? wrap(lon + 180) - 180 : lon };
  }

  /* The sky's keyframes by the sun's elevation: zenith, horizon, ground light (the hemisphere light's ground colour) and fog,
     each read between the two frames either side, so every colour is continuous in the sun's height. MOONLIT is the night
     sky under a full moon high up; the night frames lean towards it by how much moon there is. */
  var FRAMES = [
    { el: -18, zenith: 0x05070f, horizon: 0x0b1020, ground: 0x050608, fog: 0x0a0e18 },
    { el: -12, zenith: 0x0a1028, horizon: 0x1a2240, ground: 0x0a0c12, fog: 0x161c30 },
    { el: -6, zenith: 0x1b2552, horizon: 0x5a4a6e, ground: 0x1c1a24, fog: 0x3e3a56 },
    { el: -2, zenith: 0x2e4680, horizon: 0xd77a4a, ground: 0x3a2c2a, fog: 0x8a6a6a },
    { el: 2, zenith: 0x4a6eb0, horizon: 0xf2a25c, ground: 0x5a4632, fog: 0xc89a7a },
    { el: 8, zenith: 0x4f86d0, horizon: 0xf5d2a0, ground: 0x6a5a3c, fog: 0xd8c4aa },
    { el: 25, zenith: 0x3f7fd6, horizon: 0xbfd8ee, ground: 0x6b6a44, fog: 0xc4d6e4 },
    { el: 60, zenith: 0x2f6fd0, horizon: 0xb2d2ee, ground: 0x6e7048, fog: 0xbcd2e6 }
  ];
  var MOONLIT = { zenith: 0x14234a, horizon: 0x2c3c62, ground: 0x161c26, fog: 0x2a3650 };
  var SUN_POWER = 3, MOON_POWER = 0.35, AMBIENT_DAY = 1.2, AMBIENT_NIGHT = 0.08;
  // the sun's light by its elevation: red at the horizon, gold low, near white high; the moon's is a cool blue white
  var SUN_LIGHT = [{ el: 0, c: 0xff7a36 }, { el: 6, c: 0xffb866 }, { el: 20, c: 0xffe6c4 }, { el: 45, c: 0xfff6ea }];
  var MOON_LIGHT = 0xb8c8ff;

  function rgb(hex) { return [(hex >> 16) & 255, (hex >> 8) & 255, hex & 255]; }
  function mix(a, b, k) {
    var x = rgb(a), y = rgb(b);
    return (Math.round(x[0] + (y[0] - x[0]) * k) << 16) | (Math.round(x[1] + (y[1] - x[1]) * k) << 8) | Math.round(x[2] + (y[2] - x[2]) * k);
  }
  /* ramp reads colour key at elevation el from frames sorted by el, holding the end frames beyond them. */
  function ramp(frames, key, el) {
    if (el <= frames[0].el) return frames[0][key];
    for (var i = 1; i < frames.length; i++) {
      if (el <= frames[i].el) return mix(frames[i - 1][key], frames[i][key], (el - frames[i - 1].el) / (frames[i].el - frames[i - 1].el));
    }
    return frames[frames.length - 1][key];
  }

  /* skyPalette(t, lat, lon) -> the sky at that moment: zenith, horizon, ground and fog colours (0xrrggbb), the directional
     light's colour, strength and direction and which body it is (source), an ambient strength, the twilight's name, and the
     sun and moon themselves. The sun lights the scene until it is 1 degree under the horizon; the moon takes over from 2.5
     degrees under, strengthening to full by -10 as the sky darkens, scaled by its lit fraction and by how high it stands.
     Neither shines at the hand-over, and the light's colour turns from the sun's to the moon's across civil twilight, so
     nothing steps at dusk or dawn. */
  function skyPalette(t, lat, lon) {
    lat = lat == null ? LAT : lat;
    var s = sun(t, lat, lon), m = moon(t, lat, lon), el = s.elevation;
    var moonlight = m.fraction * smooth(-2, 10, m.elevation) * (1 - smooth(-10, -2.5, el));
    var p = {};
    ["zenith", "horizon", "ground", "fog"].forEach(function (k) { p[k] = mix(ramp(FRAMES, k, el), MOONLIT[k], moonlight * (1 - smooth(-12, -4, el))); });
    var bySun = el > -2.5, sunK = smooth(-1, 10, el);
    p.source = bySun ? "sun" : "moon";
    p.strength = bySun ? SUN_POWER * sunK : MOON_POWER * moonlight;
    p.light = mix(ramp(SUN_LIGHT, "c", el), MOON_LIGHT, 1 - smooth(-6, -1, el));
    p.direction = bySun ? direction(s.elevation, s.azimuth) : direction(m.elevation, m.azimuth);
    p.ambient = AMBIENT_NIGHT + (AMBIENT_DAY - AMBIENT_NIGHT) * smooth(-12, 15, el) + 0.12 * moonlight;
    p.twilight = twilight(el);
    p.sun = s;
    p.moon = m;
    return p;
  }

  var POINTS = ["north", "north-east", "east", "south-east", "south", "south-west", "west", "north-west"];
  /* compass(azimuth) names the nearest of the eight points: 251 is "west". */
  function compass(az) { return POINTS[Math.round(wrap(az) / 45) % 8]; }

  /* words(palette) -> {when, sun, moon}: the sky of a skyPalette in words, for a screen reader. when is the hour by the sun:
     Midday (up 6 degrees or more and within two hours of noon), Morning, Afternoon, Dawn or Dusk (from 6 degrees up to 6
     under, before or after noon), Night. sun is where it stands while it is up or just set, "the sun high in the south",
     "the sun low in the west", "the sun just below the horizon in the east", else "". moon is the moon while it is up and
     more than a sliver lit, "a full moon, 100 percent, rising in the east" (high from 20 degrees up), else "". */
  function words(p) {
    var s = p.sun, m = p.moon, am = s.hourAngle < 0, el = s.elevation, sun = "", moon = "";
    var when = el >= 6 ? (Math.abs(s.hourAngle) < 30 ? "Midday" : am ? "Morning" : "Afternoon") : el >= -6 ? (am ? "Dawn" : "Dusk") : "Night";
    if (el >= 25) sun = "the sun high in the " + compass(s.azimuth);
    else if (el >= 6) sun = "the sun in the " + compass(s.azimuth);
    else if (el >= -0.833) sun = "the sun low in the " + compass(s.azimuth);
    else if (el >= -6) sun = "the sun just below the horizon in the " + compass(s.azimuth);
    if (m.elevation >= -0.833 && m.fraction >= 0.03) {
      moon = "a " + m.phase + " moon, " + Math.round(m.fraction * 100) + " percent, " +
        (m.elevation >= 20 ? "high in the " : wrap(m.azimuth) < 180 ? "rising in the " : "setting in the ") + compass(m.azimuth);
    }
    return { when: when, sun: sun, moon: moon };
  }

  return {
    LAT: LAT, SYNODIC: SYNODIC, SUN_POWER: SUN_POWER, MOON_POWER: MOON_POWER, NEW_MOON: NEW_MOON, PHASES: PHASES,
    sun: sun, noon: noon, moon: moon, direction: direction, twilight: twilight, place: place, skyPalette: skyPalette, rgb: rgb, compass: compass, words: words
  };
})();
if (typeof module !== "undefined") module.exports = ALMANAC;
