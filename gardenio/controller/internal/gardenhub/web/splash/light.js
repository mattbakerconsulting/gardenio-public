/* LIGHT: the splash's light (plan gd-EJGA3zDGCk, step 7), from ALMANAC every frame. One body lights the scene: the sun by
   day and the moon by night, as skyPalette hands over between them (neither shining at the hand-over, the colour turning from
   gold to blue-silver across civil twilight, so nothing pops), its strength the moon's lit fraction at night, so a full moon
   throws clear shadows across the valley and a new moon leaves only the stars, the farm's windows and the gems' glow. It casts
   soft PCF shadow from two cascades: the near one a tight square round the stone at NEAR_MAP px, so the chisel's cuts throw
   shadow on their floors, the far one the near hill, the oak and the meadow and the valley down to the farm (FAR_BOX) at
   FAR_MAP px, fitted along the light each frame, fine enough for the oak's dapple. Each cascade is a DirectionalLight of the
   same colour and strength; cascade() patches three's light loop so a fragment takes the near one's light and shadow inside
   its square and the far one's outside, the two always summing to one light. A hemisphere light gives the sky's and the
   ground's colours as ambient (after dusk lifting from the near-black night sky towards a dim blue, NIGHT_SKY, so what the
   moon does not reach still reads), and the tone mapping's exposure follows the twilight, so night is dark but readable.

   The sky is a dome with a small shader (dome()): the palette's zenith to its horizon, the fog's colour at the horizon itself
   so the far ridges sink into it, a Rayleigh-like warmth round a low sun, the sun's disc with a soft halo, and the moon with
   its true phase: its lit fraction from the almanac, the lit limb turned towards the sun. A few thousand seeded stars turn
   with the sidereal clock about the celestial pole and fade in under civil twilight; fireflies drift over the meadow from
   civil dusk until astronomical night. The pure parts take THREE as an argument, so testdata/splash_light_test.js runs them
   under node with the real three.js. */
var LIGHT = (function () {
  "use strict";
  var A = typeof ALMANAC !== "undefined" ? ALMANAC : require("../almanac.js");
  var L = typeof LAND !== "undefined" ? LAND : require("./land.js");
  var RAD = Math.PI / 180;
  var DOME = 2500, SUN_FAR = 300, HEMI = 0.6; // the sky's radius, the near light's distance from the stone, the hemisphere light per unit of almanac ambient
  // the near cascade: a NEAR_SPAN m square round the stone at NEAR_MAP px, 4 mm a texel, fine enough for the cut's 2 cm walls
  // to throw shadow on its floor, NEAR_DEPTH m deep either side of the stone along the light
  var NEAR_SPAN = 8, NEAR_MAP = 2048, NEAR_DEPTH = 30;
  // the far cascade: what it shades, a box in the scene's frame (m), fitted along the light each frame at FAR_MAP px, 12 to
  // 29 cm a texel as the light swings, fine enough for the oak's dapple; FAR_OFF m from the box's centre
  var FAR_MAP = 1024, FAR_BOX = { x0: -70, x1: 70, y0: 0, y1: 55, z0: -80, z1: 180 }, FAR_OFF = 600;
  var BLEND = 0.1; // the fraction of the near square, in from its edge, over which the near cascade hands over to the far
  // the tone mapping's exposure: EXPOSE_LIGHT over the light the land gets, square-rooted, between 1 (the sun up) and
  // EXPOSE_MAX (a moonless night, left dark); a full moon high gets about 2.4
  var EXPOSE_LIGHT = 4.3, EXPOSE_MAX = 3;
  // the bodies' drawn radii in radians: the real ones are 0.0047 and 0.0045, a few pixels, drawn larger to be seen
  var SUN_R = 0.013, MOON_R = 0.011;
  var STARS = 2600, FIREFLIES = 90, SEED = 20261027;
  // the sky light's colours at night: the night zenith is near black, and the sky light is its colour times the almanac's
  // ambient, so a shadow at night would be black; after dusk its colours lift towards these, a dim blue the land still reads by
  var NIGHT_SKY = 0x6a7cb0, NIGHT_GROUND = 0x2a2e38;
  // the warmth round a low sun by its elevation: a rose afterglow, red at the horizon, gold, then gone
  var GLOW = [{ el: -6, c: 0xb85a78 }, { el: -1, c: 0xff6a30 }, { el: 4, c: 0xffa050 }, { el: 14, c: 0xffd8a8 }];
  // where the fireflies drift: the meadow between the camera and the crest, round the stone
  var MEADOW = { x0: -14, x1: 15, z0: L.CREST_Z + 1, z1: L.CAM_Z - 3 };

  function clamp(x, lo, hi) { return x < lo ? lo : x > hi ? hi : x; }
  function smooth(e0, e1, x) { var k = clamp((x - e0) / (e1 - e0), 0, 1); return k * k * (3 - 2 * k); }
  function wrap(deg) { deg %= 360; return deg < 0 ? deg + 360 : deg; }
  function mix(a, b, k) {
    var x = A.rgb(a), y = A.rgb(b);
    return (Math.round(x[0] + (y[0] - x[0]) * k) << 16) | (Math.round(x[1] + (y[1] - x[1]) * k) << 8) | Math.round(x[2] + (y[2] - x[2]) * k);
  }

  /* exposure(p) is the tone mapping's exposure under palette p, following the twilight by the light the land gets (the
     body's strength and twice the sky's ambient, both smooth in the sun's and the moon's heights): 1 by day, opening as the
     light fails at dusk so the night is dark but the land still reads, up to EXPOSE_MAX, short of what a moonless night
     would need to look like day. */
  function exposure(p) { return clamp(Math.sqrt(EXPOSE_LIGHT / (p.strength + 2 * p.ambient)), 1, EXPOSE_MAX); }

  /* moonlit(p) is how much moon there is: its lit fraction, as it clears the horizon. */
  function moonlit(p) { return p.moon.fraction * smooth(-2, 10, p.moon.elevation); }

  /* starlight(p) is how bright the stars are, 0 to 1: they come out under civil twilight (the sun 1 to 10 degrees down) and a
     bright moon washes the faint ones out. */
  function starlight(p) { return (1 - smooth(-10, -1, p.sun.elevation)) * (1 - 0.45 * moonlit(p)); }

  /* fireflies(p) is how many of the fireflies are out, 0 to 1: from civil dusk (the sun 0 to 3 degrees down) until
     astronomical night (14 to 18 down), and again before dawn. */
  function fireflies(p) { var el = p.sun.elevation; return (1 - smooth(-3, 0, el)) * smooth(-18, -14, el); }

  /* night(p) is how far the day has gone, 0 with the sun 2 degrees up, 1 once it is 8 down. */
  function night(p) { return 1 - smooth(-8, 2, p.sun.elevation); }

  /* warmth(p) is how much the sky warms round the sun: from its afterglow 8 degrees down until it stands 20 degrees up. */
  function warmth(p) { var el = p.sun.elevation; return smooth(-8, -1, el) * (1 - smooth(4, 20, el)); }

  /* glow(el) is the colour of the warmth round the sun at elevation el (0xrrggbb). */
  function glow(el) {
    if (el <= GLOW[0].el) return GLOW[0].c;
    for (var i = 1; i < GLOW.length; i++) if (el <= GLOW[i].el) return mix(GLOW[i - 1].c, GLOW[i].c, (el - GLOW[i - 1].el) / (GLOW[i].el - GLOW[i - 1].el));
    return GLOW[GLOW.length - 1].c;
  }

  /* bodies(p) -> {sun, moon, toSun, up, east}: the unit vectors (scene frame) to the sun and the moon, and the moon's face's
     frame on the sky: toSun the direction across its disc towards the sun (its lit limb), up the local vertical across the
     disc and east its right hand, so the drawn seas stay put. At new and full moon, where the sun stands behind or before
     the moon and the tangent is undefined, toSun falls back to the face's east. */
  function bodies(p) {
    var s = A.direction(p.sun.elevation, p.sun.azimuth), m = A.direction(p.moon.elevation, p.moon.azimuth);
    function tangent(v) {
      var d = v[0] * m[0] + v[1] * m[1] + v[2] * m[2], t = [v[0] - m[0] * d, v[1] - m[1] * d, v[2] - m[2] * d];
      var n = Math.sqrt(t[0] * t[0] + t[1] * t[1] + t[2] * t[2]);
      return n < 1e-6 ? null : [t[0] / n, t[1] / n, t[2] / n];
    }
    var up = tangent([0, 1, 0]) || [0, 0, -1];
    var east = [up[1] * m[2] - up[2] * m[1], up[2] * m[0] - up[0] * m[2], up[0] * m[1] - up[1] * m[0]];
    return { sun: s, moon: m, toSun: tangent(s) || east, up: up, east: east };
  }

  /* pole(lat, lst) -> the 3x3 rotation (column major, 9 numbers) taking the stars' equatorial frame (x to RA 0, y to RA 6h,
     z to the north celestial pole) into the scene's at latitude lat and local sidereal time lst (degrees), so a star at
     hour angle 0 stands on the meridian at elevation 90 - lat + its declination and the sky turns west through the night. */
  function pole(lat, lst) {
    var E0 = A.direction(90 - lat, 180), W = A.direction(0, 270), P = A.direction(lat, 0), c = Math.cos(lst * RAD), s = Math.sin(lst * RAD);
    return [c * E0[0] + s * W[0], c * E0[1] + s * W[1], c * E0[2] + s * W[2],
      s * E0[0] - c * W[0], s * E0[1] - c * W[1], s * E0[2] - c * W[2],
      P[0], P[1], P[2]];
  }

  /* rnd(seed) -> r(i, k), a seeded value in [0, 1) for item i's k-th draw: the land's integer hash, the same everywhere. */
  function rnd(seed) { return function (i, k) { return L.hash(i, k, seed); }; }

  /* starField() -> {position, color, size, phase}: STARS seeded stars on the unit sphere in the equatorial frame, uniform in
     area, most faint and a few bright (size in px at 1x), white with a few blue and orange, and each its twinkle's phase. */
  function starField() {
    var r = rnd(SEED), pos = new Float32Array(STARS * 3), col = new Float32Array(STARS * 3), size = new Float32Array(STARS), ph = new Float32Array(STARS);
    for (var i = 0; i < STARS; i++) {
      var z = 2 * r(i, 0) - 1, a = 2 * Math.PI * r(i, 1), c = Math.sqrt(1 - z * z), mag = r(i, 2), tint = r(i, 3);
      pos[i * 3] = c * Math.cos(a); pos[i * 3 + 1] = c * Math.sin(a); pos[i * 3 + 2] = z;
      var b = 0.18 + 0.82 * mag * mag * mag * mag;
      var rgb = tint < 0.12 ? [0.78, 0.86, 1] : tint > 0.9 ? [1, 0.84, 0.66] : [1, 0.98, 0.94];
      col[i * 3] = rgb[0] * b; col[i * 3 + 1] = rgb[1] * b; col[i * 3 + 2] = rgb[2] * b;
      size[i] = 1.1 + 2.2 * Math.pow(mag, 8);
      ph[i] = r(i, 4);
    }
    return { position: pos, color: col, size: size, phase: ph };
  }

  /* swarm() -> {position, seed}: FIREFLIES seeded over the meadow, each 0.3 to 1.6 m above the ground and clear of the stone,
     with its drift's phase, speed and reach and its blink's rate. */
  function swarm() {
    var r = rnd(SEED + 1), pos = new Float32Array(FIREFLIES * 3), seed = new Float32Array(FIREFLIES * 4), n = 0;
    for (var i = 0; n < FIREFLIES; i++) {
      var x = MEADOW.x0 + (MEADOW.x1 - MEADOW.x0) * r(i, 0), z = MEADOW.z0 + (MEADOW.z1 - MEADOW.z0) * r(i, 1);
      if (Math.abs(x - L.STONE.x) < 1.4 && Math.abs(z - L.STONE.z) < 1.4) continue;
      pos[n * 3] = x; pos[n * 3 + 1] = L.height(x, z) + 0.3 + 1.3 * r(i, 2); pos[n * 3 + 2] = z;
      seed[n * 4] = r(i, 3); seed[n * 4 + 1] = 0.25 + 0.5 * r(i, 4); seed[n * 4 + 2] = 0.4 + 1.2 * r(i, 5); seed[n * 4 + 3] = 0.7 + 1.6 * r(i, 6);
      n++;
    }
    return { position: pos, seed: seed };
  }

  /* cascade(THREE) patches three's light loop, once: with two directional lights casting shadow (the near cascade first, as
     build() adds it), a fragment takes the near light inside the near light's shadow square, the far one outside it, and
     both across the BLEND band at its edge, so the pair adds up to one light everywhere. Every lit material gets it, the
     ones FLORA, STONE and FARM patch too. Without shadow maps the pair shares the light half and half. */
  function cascade(THREE) {
    var C = THREE.ShaderChunk;
    if (C.lights_fragment_begin.indexOf("splashNear") >= 0) return;
    var info = "getDirectionalLightInfo( directionalLight, directLight );";
    if (C.lights_fragment_begin.indexOf(info) < 0 || C.shadowmap_pars_fragment.indexOf("vDirectionalShadowCoord") < 0) throw new Error("light: three's light loop has changed; cascade() cannot patch it");
    C.shadowmap_pars_fragment += [
      "",
      "#if defined( USE_SHADOWMAP ) && NUM_DIR_LIGHT_SHADOWS == 2",
      "float splashNear() {",
      "  vec3 c = vDirectionalShadowCoord[ 0 ].xyz / vDirectionalShadowCoord[ 0 ].w;",
      "  vec2 e = min( c.xy, 1.0 - c.xy );",
      "  return smoothstep( 0.0, " + BLEND.toFixed(3) + ", min( e.x, e.y ) ) * smoothstep( 0.0, 0.05, c.z ) * smoothstep( 1.0, 0.95, c.z );",
      "}",
      "#endif"
    ].join("\n");
    C.lights_fragment_begin = C.lights_fragment_begin.replace(info, [
      info,
      "		#if defined( USE_SHADOWMAP ) && NUM_DIR_LIGHT_SHADOWS == 2 && NUM_DIR_LIGHTS == 2",
      "		directLight.color *= UNROLLED_LOOP_INDEX == 0 ? splashNear() : 1.0 - splashNear();",
      "		#elif NUM_DIR_LIGHTS == 2",
      "		directLight.color *= 0.5;",
      "		#endif"
    ].join("\n"));
  }

  var SKY_VERTEX = [
    "varying vec3 vDir;",
    "void main() {",
    "  vDir = position;",
    "  gl_Position = projectionMatrix * modelViewMatrix * vec4( position, 1.0 );",
    "}"
  ].join("\n");
  var SKY_FRAGMENT = [
    "uniform vec3 uZenith, uHorizon, uFog, uGlow, uSunColor, uSun, uMoon, uToSun, uUp, uEast, uMoonColor;",
    "uniform float uWarm, uSunK, uMoonK, uMoonLit, uNight;",
    "varying vec3 vDir;",
    // the moon's seas: soft dark patches fixed on its face (q in its radius, x east, y up)
    "float seas( vec2 q ) {",
    "  float m = smoothstep( 0.45, 0.0, length( q - vec2( -0.28, 0.3 ) ) ) + 0.8 * smoothstep( 0.36, 0.0, length( q - vec2( 0.18, 0.28 ) ) );",
    "  m += 0.7 * smoothstep( 0.3, 0.0, length( q - vec2( -0.02, -0.08 ) ) ) + 0.5 * smoothstep( 0.24, 0.0, length( q - vec2( 0.36, -0.32 ) ) );",
    "  return clamp( m, 0.0, 1.0 );",
    "}",
    "void main() {",
    "  vec3 d = normalize( vDir );",
    "  vec3 col = mix( uHorizon, uZenith, sqrt( max( d.y, 0.0 ) ) );",
    // the fog's colour at the horizon itself, so the far ridges sink into it
    "  col = mix( uFog, col, smoothstep( -0.02, 0.12, d.y ) );",
    // a Rayleigh-like warmth round a low sun, strongest along the horizon
    "  float cs = max( dot( d, uSun ), 0.0 );",
    "  float lobe = 0.55 * pow( cs, 4.0 ) + 0.45 * pow( cs, 24.0 );",
    "  col = mix( col, uGlow, clamp( lobe * ( 1.0 - smoothstep( -0.02, 0.45, d.y ) ) * uWarm, 0.0, 0.85 ) );",
    // the sun's disc and its soft halo
    "  float ts = length( d - uSun );",
    "  col += uSunColor * ( 0.45 * exp( -ts * 28.0 ) + 0.18 * exp( -ts * 6.0 ) ) * uSunK;",
    "  col = mix( col, uSunColor, ( 1.0 - smoothstep( " + (SUN_R * 0.8).toFixed(4) + ", " + SUN_R.toFixed(4) + ", ts ) ) * uSunK );",
    // the moon's glow at night, then its disc: lit on the side towards the sun as far as its lit fraction says
    "  float tm = length( d - uMoon );",
    "  col += uMoonColor * uMoonLit * uMoonK * uNight * ( 0.08 * exp( -tm * 14.0 ) + 0.04 * exp( -tm * 3.0 ) );",
    "  if ( tm < " + (MOON_R * 1.2).toFixed(4) + " ) {",
    "    vec3 o = d - uMoon * dot( d, uMoon );",
    "    vec2 q = vec2( dot( o, uToSun ), dot( o, cross( uMoon, uToSun ) ) ) / " + MOON_R.toFixed(4) + ";",
    "    float disc = 1.0 - smoothstep( 0.9, 1.0, length( q ) );",
    "    float term = ( 1.0 - 2.0 * uMoonLit ) * sqrt( max( 1.0 - q.y * q.y, 0.0 ) );",
    "    float lit = smoothstep( term - 0.07, term + 0.07, q.x );",
    "    vec3 face = uMoonColor * ( 1.0 - 0.3 * seas( vec2( dot( o, uEast ), dot( o, uUp ) ) / " + MOON_R.toFixed(4) + " ) );",
    "    vec3 dark = mix( col, col * 0.8 + uMoonColor * 0.03, uNight );",
    "    col = mix( col, mix( dark, face, lit ), disc * uMoonK );",
    "  }",
    "  gl_FragColor = vec4( col, 1.0 );",
    "  #include <colorspace_fragment>",
    "}"
  ].join("\n");

  /* dome(THREE) is the sky: a sphere seen from inside, its shader drawing the palette's gradient, the fog at the horizon, the
     warmth round a low sun, the sun's disc and halo and the moon in its phase, unfogged and not tone mapped, so its horizon
     is the colour the fogged far hills fade into. Its uniforms are set by update(). */
  function dome(THREE) {
    var u = {};
    ["uZenith", "uHorizon", "uFog", "uGlow", "uSunColor", "uMoonColor"].forEach(function (k) { u[k] = { value: new THREE.Color() }; });
    ["uSun", "uMoon", "uToSun", "uUp", "uEast"].forEach(function (k) { u[k] = { value: new THREE.Vector3(0, 1, 0) }; });
    ["uWarm", "uSunK", "uMoonK", "uMoonLit", "uNight"].forEach(function (k) { u[k] = { value: 0 }; });
    var m = new THREE.Mesh(new THREE.SphereGeometry(DOME, 48, 24), new THREE.ShaderMaterial({
      uniforms: u, vertexShader: SKY_VERTEX, fragmentShader: SKY_FRAGMENT, side: THREE.BackSide, fog: false, toneMapped: false, depthWrite: false
    }));
    m.name = "sky";
    m.renderOrder = -1;
    return m;
  }

  /* stars(THREE) is the star field, Points on a sphere just inside the dome, turned by update() with the sidereal clock: round
     soft points of their size in px, each twinkling a little, dimmed towards the horizon, added to the sky. */
  function stars(THREE) {
    var f = starField(), g = new THREE.BufferGeometry(), pos = new Float32Array(f.position.length);
    for (var i = 0; i < pos.length; i++) pos[i] = f.position[i] * DOME * 0.92;
    g.setAttribute("position", new THREE.BufferAttribute(pos, 3));
    g.setAttribute("color", new THREE.BufferAttribute(f.color, 3));
    g.setAttribute("aSize", new THREE.BufferAttribute(f.size, 1));
    g.setAttribute("aPhase", new THREE.BufferAttribute(f.phase, 1));
    var pts = new THREE.Points(g, new THREE.ShaderMaterial({
      uniforms: { uK: { value: 0 }, uTime: { value: 0 }, uScale: { value: 1 } },
      vertexShader: [
        "attribute float aSize; attribute float aPhase; attribute vec3 color;",
        "uniform float uK, uTime, uScale;",
        "varying vec3 vC;",
        "void main() {",
        "  vec4 w = modelMatrix * vec4( position, 1.0 );",
        "  float up = normalize( w.xyz - cameraPosition ).y;",
        "  float tw = 0.82 + 0.18 * sin( uTime * ( 1.1 + 2.0 * aPhase ) + aPhase * 40.0 );",
        "  vC = color * uK * tw * smoothstep( -0.02, 0.2, up );",
        "  gl_Position = projectionMatrix * viewMatrix * w;",
        "  gl_PointSize = aSize * uScale;",
        "}"
      ].join("\n"),
      fragmentShader: [
        "varying vec3 vC;",
        "void main() {",
        "  float a = smoothstep( 0.5, 0.12, length( gl_PointCoord - 0.5 ) );",
        "  gl_FragColor = vec4( vC * a, 1.0 );",
        "  #include <colorspace_fragment>",
        "}"
      ].join("\n"),
      transparent: true, depthWrite: false, blending: THREE.AdditiveBlending, fog: false, toneMapped: false
    }));
    pts.name = "stars";
    pts.matrixAutoUpdate = false;
    pts.frustumCulled = false;
    pts.renderOrder = -1;
    return pts;
  }

  /* flies(THREE) is the fireflies: Points over the meadow whose shader drifts each on its own slow loop and blinks it, a warm
     green glow added to the scene, sized by distance. */
  function flies(THREE) {
    var s = swarm(), g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.BufferAttribute(s.position, 3));
    g.setAttribute("aSeed", new THREE.BufferAttribute(s.seed, 4));
    var pts = new THREE.Points(g, new THREE.ShaderMaterial({
      uniforms: { uK: { value: 0 }, uTime: { value: 0 }, uScale: { value: 400 } },
      vertexShader: [
        "attribute vec4 aSeed;",
        "uniform float uK, uTime, uScale;",
        "varying float vA;",
        "void main() {",
        "  vec3 p = position;",
        "  float t = uTime * aSeed.y, k = aSeed.x * 6.2832;",
        "  p.x += aSeed.z * ( sin( t + k ) + 0.6 * sin( t * 0.37 + k * 2.7 ) );",
        "  p.z += aSeed.z * cos( t * 0.83 + k * 1.7 );",
        "  p.y += 0.35 * sin( t * 0.61 + k * 3.1 );",
        "  vec4 mv = modelViewMatrix * vec4( p, 1.0 );",
        "  float blink = pow( max( sin( uTime * aSeed.w + k * 7.0 ), 0.0 ), 6.0 );",
        "  vA = uK * ( 0.12 + 0.88 * blink );",
        "  gl_Position = projectionMatrix * mv;",
        "  gl_PointSize = max( 1.5, uScale * 0.1 * ( 0.6 + 0.6 * blink ) / -mv.z );",
        "}"
      ].join("\n"),
      fragmentShader: [
        "varying float vA;",
        "void main() {",
        "  float a = smoothstep( 0.5, 0.0, length( gl_PointCoord - 0.5 ) );",
        "  gl_FragColor = vec4( vec3( 0.72, 1.0, 0.32 ) * a * a * vA * 1.6, 1.0 );",
        "  #include <colorspace_fragment>",
        "}"
      ].join("\n"),
      transparent: true, depthWrite: false, blending: THREE.AdditiveBlending, fog: false, toneMapped: false
    }));
    pts.name = "fireflies";
    pts.frustumCulled = false;
    return pts;
  }

  /* fit(light, box) fits a directional light's shadow camera round box ({x0, x1, y0, y1, z0, z1}) as seen along the light:
     the box's corners in the light's view, rounded out to whole metres so the map does not crawl as the light swings, its
     near plane pulled 20 m towards the light for what stands just outside. */
  function fit(light, box) {
    var cam = light.shadow.camera, v = light.position.clone(), lo = [Infinity, Infinity, Infinity], hi = [-Infinity, -Infinity, -Infinity];
    cam.position.copy(light.position);
    cam.lookAt(light.target.position);
    cam.updateMatrixWorld(true);
    for (var i = 0; i < 8; i++) {
      v.set(i & 1 ? box.x1 : box.x0, i & 2 ? box.y1 : box.y0, i & 4 ? box.z1 : box.z0).applyMatrix4(cam.matrixWorldInverse);
      for (var k = 0; k < 3; k++) { lo[k] = Math.min(lo[k], v.getComponent(k)); hi[k] = Math.max(hi[k], v.getComponent(k)); }
    }
    cam.left = Math.floor(lo[0]); cam.right = Math.ceil(hi[0]); cam.bottom = Math.floor(lo[1]); cam.top = Math.ceil(hi[1]);
    cam.near = Math.max(0.5, -hi[2] - 20); cam.far = -lo[2] + 5;
    cam.updateProjectionMatrix();
  }

  /* build(THREE, opts) -> the light, a Group named "light": the sky ("sky") with its stars ("stars"), the near cascade (named
     for the body that shines, "sun" or "moon") aimed at opts.stone, the far one ("<body> far") at its target ("far target"),
     the hemisphere light ("sky light") and the fireflies ("fireflies"). opts.eye is where the sky is centred (the camera),
     opts.fog the scene's fog, whose colour it keeps, opts.lat and opts.lon the place, opts.quality a tier's cascades and maps
     (quality(); both cascades at NEAR_MAP and FAR_MAP without it); update(group, opts.t) sets it all. */
  function build(THREE, opts) {
    opts = opts || {};
    cascade(THREE);
    var group = new THREE.Group(), sky = dome(THREE), st = stars(THREE);
    group.name = "light";
    if (opts.eye) sky.position.copy(opts.eye);
    sky.add(st);
    var near = new THREE.DirectionalLight(0xffffff, 0), far = new THREE.DirectionalLight(0xffffff, 0), farTarget = new THREE.Object3D();
    near.target = opts.stone || new THREE.Object3D();
    near.castShadow = true;
    near.shadow.mapSize.set(NEAR_MAP, NEAR_MAP);
    var sc = near.shadow.camera;
    sc.left = sc.bottom = -NEAR_SPAN / 2; sc.right = sc.top = NEAR_SPAN / 2; sc.near = SUN_FAR - NEAR_DEPTH; sc.far = SUN_FAR + NEAR_DEPTH;
    sc.updateProjectionMatrix();
    near.shadow.bias = -0.0001; near.shadow.normalBias = 0.0015; near.shadow.radius = 1.5;
    farTarget.name = "far target";
    farTarget.position.set((FAR_BOX.x0 + FAR_BOX.x1) / 2, (FAR_BOX.y0 + FAR_BOX.y1) / 2, (FAR_BOX.z0 + FAR_BOX.z1) / 2);
    far.target = farTarget;
    far.castShadow = true;
    far.shadow.mapSize.set(FAR_MAP, FAR_MAP);
    far.shadow.bias = -0.0004; far.shadow.normalBias = 0.04; far.shadow.radius = 1.5;
    var hemi = new THREE.HemisphereLight(0xffffff, 0xffffff, 0);
    hemi.name = "sky light";
    var ff = flies(THREE);
    // the near cascade first: three lists lights that cast shadow in the scene's order, and cascade() reads index 0 as near
    group.add(sky, near, far, farTarget, hemi, ff);
    group.userData = { near: near, far: far, hemi: hemi, sky: sky, stars: st, fireflies: ff, fog: opts.fog || null, lat: opts.lat == null ? A.LAT : opts.lat, lon: opts.lon || 0, t: null, seconds: 0, cascades: 2 };
    if (opts.quality) quality(group, opts.quality);
    update(group, opts.t == null ? Date.now() : opts.t);
    return group;
  }

  /* quality(group, q) casts the shadows of a tier (QUALITY.TIERS): q.cascades 2, the near cascade at q.near px and the far
     at q.far; 1, the far cascade alone at q.far px, the near light hidden (three leaves a hidden light out of the light loop,
     so the far one lights everything in full and cascade()'s split does not apply). A map whose size changes is let go, and
     three makes it again at the new size on the next shadow pass; the caller asks for that pass. It says whether anything
     changed. */
  function quality(group, q) {
    var o = group.userData, two = q.cascades !== 1, changed = o.cascades !== (two ? 2 : 1);
    o.near.visible = two;
    o.cascades = two ? 2 : 1;
    changed = size(o.near, two ? q.near || NEAR_MAP : NEAR_MAP) || changed;
    return size(o.far, q.far || FAR_MAP) || changed;
  }
  function size(light, px) {
    if (light.shadow.mapSize.x === px) return false;
    light.shadow.mapSize.set(px, px);
    if (light.shadow.map) { light.shadow.map.dispose(); light.shadow.map = null; }
    return true;
  }

  /* update(group, t) lights the scene at unix ms t from the almanac and returns the palette: both cascades along the body's
     direction in its colour and strength, the far one refitted, the sky light, the fog, the sky's colours and bodies, the
     stars turned to the sidereal time and faded in, the fireflies out or not, and the exposure on group.userData. */
  function update(group, t) {
    var o = group.userData, p = A.skyPalette(t, o.lat, o.lon), d = p.direction, b = bodies(p);
    o.t = t;
    o.palette = p;
    o.exposure = exposure(p);
    var tgt = o.near.target.position;
    [o.near, o.far].forEach(function (l) {
      var at = l === o.near ? tgt : o.far.target.position, r = l === o.near ? SUN_FAR : FAR_OFF;
      l.position.set(at.x + d[0] * r, at.y + d[1] * r, at.z + d[2] * r);
      l.color.set(p.light);
      l.intensity = p.strength;
    });
    o.near.name = p.source;
    o.far.name = p.source + " far";
    fit(o.far, FAR_BOX);
    var nk = night(p);
    o.hemi.color.set(mix(p.zenith, NIGHT_SKY, nk));
    o.hemi.groundColor.set(mix(p.ground, NIGHT_GROUND, nk));
    o.hemi.intensity = p.ambient * HEMI;
    if (o.fog) o.fog.color.set(p.fog);

    var u = o.sky.material.uniforms;
    u.uZenith.value.set(p.zenith); u.uHorizon.value.set(p.horizon); u.uFog.value.set(p.fog);
    u.uGlow.value.set(glow(p.sun.elevation)); u.uWarm.value = warmth(p);
    u.uSun.value.fromArray(b.sun);
    u.uSunColor.value.set(mix(glow(p.sun.elevation), 0xfff8ec, smooth(2, 25, p.sun.elevation))).multiplyScalar(1.4);
    u.uSunK.value = smooth(-1.5, 0.5, p.sun.elevation);
    u.uMoon.value.fromArray(b.moon); u.uToSun.value.fromArray(b.toSun); u.uUp.value.fromArray(b.up); u.uEast.value.fromArray(b.east);
    u.uMoonColor.value.set(0xf2eedf).multiplyScalar(0.55 + 0.45 * nk);
    u.uMoonLit.value = p.moon.fraction;
    // the moon by day is a pale ghost of itself; it clears the horizon over its own diameter or so
    u.uMoonK.value = smooth(-1, 0.6, p.moon.elevation) * (0.4 + 0.6 * nk);
    u.uNight.value = nk;

    var sk = starlight(p), su = o.stars.material.uniforms, lst = p.sun.ra + p.sun.hourAngle;
    su.uK.value = sk;
    o.stars.visible = sk > 0.003;
    o.stars.matrix.fromArray(basis(pole(o.lat, wrap(lst))));
    o.stars.matrixWorldNeedsUpdate = true;
    var fk = fireflies(p);
    o.fireflies.material.uniforms.uK.value = fk;
    o.fireflies.visible = fk > 0.003;
    return p;
  }
  /* basis turns pole()'s 3x3 into a Matrix4's 16 numbers, column major. */
  function basis(m) { return [m[0], m[1], m[2], 0, m[3], m[4], m[5], 0, m[6], m[7], m[8], 0, 0, 0, 0, 1]; }

  /* tick(group, seconds) moves the stars' twinkle and the fireflies to seconds of the page's own clock. */
  function tick(group, seconds) {
    var o = group.userData;
    o.seconds = seconds;
    o.stars.material.uniforms.uTime.value = seconds;
    o.fireflies.material.uniforms.uTime.value = seconds;
  }

  /* resize tells the points how big the drawing buffer is: the stars' px by its pixel ratio, the fireflies' by its height. */
  function resize(group, height, ratio) {
    var o = group.userData;
    o.stars.material.uniforms.uScale.value = ratio;
    o.fireflies.material.uniforms.uScale.value = height * ratio / 2;
  }

  /* tip(THREE, group, stone, land) -> where the stone's shadow ends, read from the scene: a ray from the top of the stone's
     body along the near cascade's light, away from it, onto the land (LAND.build's group: the field and its far ring; a low sun throws
     the tip off the field). {source, strength, stone: [x, z], tip: [x, y, z],
     length (m, level), bearing (degrees from north towards east, from the stone to the tip), west, east}; null when the body
     that shines is under the horizon or the ray misses the land. */
  function tip(THREE, group, stone, land) {
    var o = group.userData, light = o.near, toward = light.position.clone().sub(light.target.position).normalize();
    if (toward.y <= 0.001) return null;
    stone.updateMatrixWorld(true);
    land.updateMatrixWorld(true);
    var box = new THREE.Box3().setFromObject(stone.getObjectByName("stone body") || stone);
    var top = new THREE.Vector3((box.min.x + box.max.x) / 2, box.max.y, (box.min.z + box.max.z) / 2);
    var hit = new THREE.Raycaster(top, toward.clone().negate(), 0, 2000).intersectObject(land, true)[0];
    if (!hit) return null;
    var dx = hit.point.x - stone.position.x, dz = hit.point.z - stone.position.z;
    return {
      source: light.name, strength: light.intensity, stone: [stone.position.x, stone.position.z], tip: hit.point.toArray(),
      length: Math.sqrt(dx * dx + dz * dz), bearing: wrap(Math.atan2(dx, -dz) / RAD), west: dx < 0, east: dx > 0
    };
  }

  return {
    DOME: DOME, SUN_FAR: SUN_FAR, HEMI: HEMI, NEAR_SPAN: NEAR_SPAN, NEAR_MAP: NEAR_MAP, NEAR_DEPTH: NEAR_DEPTH, FAR_MAP: FAR_MAP, FAR_BOX: FAR_BOX,
    EXPOSE_LIGHT: EXPOSE_LIGHT, EXPOSE_MAX: EXPOSE_MAX, STARS: STARS, FIREFLIES: FIREFLIES, MEADOW: MEADOW,
    NIGHT_SKY: NIGHT_SKY, NIGHT_GROUND: NIGHT_GROUND,
    exposure: exposure, moonlit: moonlit, night: night, starlight: starlight, fireflies: fireflies, warmth: warmth, glow: glow, bodies: bodies, pole: pole,
    starField: starField, swarm: swarm, cascade: cascade, fit: fit, build: build, quality: quality, update: update, tick: tick, resize: resize, tip: tip
  };
})();
if (typeof module !== "undefined") module.exports = LIGHT;
