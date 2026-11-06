/* What the Commons UX rubric's DOM probes (.claude/skills/commons-ux-acceptance/scripts/probes.js) look inside, per surface:
   the thing under test, not the farm behind it. A surface with no row is probed from body. Both the golden harness's C3
   pass (golden_browser.js, M4.1 gd-T3JPWCD9xM) and the skill's shoot.js read it, so CI and a grade probe the same box. Rows
   for the skill's own surfaces (its EXTRA and stress rows) live here too, beside the goldens' they vary. */
'use strict';

var ROOTS = { land: '.crumb', landwall: '.crumb .plaque', landcap: '.crumb .plaque', landstress: '.crumb', transcript: '#bead', facearms: '#bead', report: '#bead', costs: '#bead', menu: '#menu-list', facework: '#bead', faceepic: '#bead', faceplan: '#bead', facedecision: '#bead', facedecisionanswered: '#bead', faceescalation: '#bead', faceescalationdone: '#bead', faceagent: '#bead', facemessage: '#bead', facebundle: '#bead', facereport: '#bead', facegardener: '#bead', facemr: '#bead', facegeneric: '#bead', facedecisionstress: '#bead', faceescalationstress: '#bead', faceagentstress: '#bead', steward: '#stip', stewardstress: '#stip', goatarms: '#stip', goatgate: '#stip', goatghost: '#stip', goatpen: '#stip', pot: '#stip', potstress: '#stip', potbead: '#bead', potbeadstress: '#bead', staked: '#stip', stakedstress: '#stip', stakedbead: '#bead', stakedbeadstress: '#bead', postcardmenu: '#menu-list', noscript: '#ledger', toplod: '#lbls', opening: 'body', cursorworld: '#lbls', cursorproject: '#lbls', cursorplan: '#lbls', cursorphase: '#lbls', cursorharvest: '#lbls', bin: '#bin', binharvest: '#bin', porch: '#dock', porcharms: '#dock', bead: '#bead', beadwish: '#bead', beadstress: '#bead', beadharvest: '#bead', mailbox: '#dock', plansdrawer: '#dw-plans', beacons: '#bcns',
  beadplan: '#bead', projectpartial: '#bead', costsproject: '#bead', gardenerabout: '#bead', searchempty: '#bead', settingsedit: '#bead', queueinbox: '#bead', queueescalations: '#bead', projectagents: '#bead', projectgardeners: '#bead', projectaccounts: '#bead', projectactivity: '#bead', queues: '#bead', search: '#bead', bundle: '#bead', cockpit: '#bead', project: '#bead', projectcost: '#bead', projectcostcharts: '#bead', gardener: '#bead', settings: '#bead', cmdk: '#jump',
  faceagentline: '#bead', commons: '#wb2', commonsthread: '#wb2', commonsplan: '#wb2', commonsjobs: '#wb2', commonswishdoc: '#wb2', commonssteward: '#wb2', commonsphase: '#wb2', commonsplanstress: '#wb2', commonsagentnudge: '#wb2',
  snapbar: '.cam' };

/* Where the keyboard pass looks for focus, when it is not the probe root: the cursor rows probe the signs (#lbls, aria-hidden by
   design), but their focus is the cursor's hot link in #hots (cursor-grid spec 3.6). The steward's own hot in #ghots is out of the
   Tab order like every gardener's (spec 8: not grid cells); its open tip is the stop after the cursor (gd-xWD34dBFlF). */
var FOCUS_ROOTS = { escalations: '#hots', escalationsstress: '#hots', toplod: '#hots', cursorworld: '#hots', cursorproject: '#hots', cursorplan: '#hots', cursorphase: '#hots', cursorharvest: '#hots' };

module.exports = { ROOTS: ROOTS, FOCUS_ROOTS: FOCUS_ROOTS };
