#!/usr/bin/env node
// Redraws every SVG of the shipped pixel-art login scene (Dawn Meadow, design
// doc section 13, direction A) straight into the chart's theme tree:
//   gardenio/keycloak-theme/gardenio/login/resources/img/
// Deterministic (seeded), so a re-run changes nothing; `git diff` after a run
// is the proof. Usage: node gen/index.js
//
// The art is original: sprite maps and seeded noise written for this repo, no
// sprite pack and no traced image. The other two S1g candidates (forest, hills)
// were removed with their generators when the owner picked the meadow; they are
// in git history (PR #539, gd-nWXkV6XRc5) if ever wanted again.
'use strict';
const fs = require('fs');
const path = require('path');
const { Grid, writeSvg } = require('./lib');

const RES = path.join(__dirname, '..', '..', '..', 'keycloak-theme', 'gardenio', 'login', 'resources');

// The sprout in front of the wordmark sign (a CSS mask, so it takes the sign's text colour).
function sprout() {
  const g = new Grid(9, 9, { wrapX: false });
  g.stamp(['.........', '.LL...LL.', 'LLLL.LLLL', '.LLLLLLL.', '..LL.LL..', '....L....', '....L....', '....L....', '...LLL...'], 0, 0, { L: '#000000' });
  return g;
}

const sizes = { 'img/sprout.svg': writeSvg(path.join(RES, 'img', 'sprout.svg'), sprout()) };
for (const [name, bytes] of Object.entries(require('./meadow').build(RES))) sizes[`img/${name}`] = bytes;
// The font is the one asset the generator does not make (see fonts/OFL-PixelifySans.txt).
for (const f of fs.readdirSync(path.join(RES, 'fonts'))) sizes[`fonts/${f}`] = fs.statSync(path.join(RES, 'fonts', f)).size;
for (const f of ['scene-base.css', 'scene-meadow.css']) sizes[`css/${f}`] = fs.statSync(path.join(RES, 'css', f)).size;
const total = Object.values(sizes).reduce((a, b) => a + b, 0);
console.log(JSON.stringify({ bytes: total, files: sizes }, null, 1));
