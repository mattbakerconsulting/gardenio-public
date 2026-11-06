/* Browser check that a bead's Commons node and its bead view are one document (gd-0GnBi7SRfK), run from
   TestOneBeadViewUnderChromium against the card world, whose bead view reads the Commons' beads too.
   Usage: node onebead_browser.js <base url> <node_modules dir> <bead id>. GARDENHUB_SHOTS=<dir> saves a PNG of each view.

   The fixture wish opened as /garden?bead=<id> (the panel) and as /garden/c/n/<id> (the workbench) draws byte-identical
   face and Conversation sections, each with its reply composer settled; the workbench keeps its tree and rails, its
   document has no head, face or transcript of the node's own, and a bead link in its document opens that bead here. On a
   phone the panel opens with its newest line on screen. No console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], id = process.argv[4], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] })), shots = process.env.GARDENHUB_SHOTS || '';

/* the face and Conversation sections under root, once the conversation's composer is in (its place is read after the paint) */
async function sections(page, root) {
  await page.waitForFunction(function (root) {
    var b = document.querySelector(root), cv = b && b.querySelector('.bv-sec[data-sec=conversation]');
    return !!b && !!b.querySelector('.bv-face') && !!cv && !!cv.querySelector('.bv-cv-foot > *');
  }, root, { timeout: 15000 });
  return page.evaluate(function (root) {
    var b = document.querySelector(root);
    return { face: b.querySelector('.bv-face').outerHTML, conversation: b.querySelector('.bv-sec[data-sec=conversation]').outerHTML };
  }, root);
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log(String(e).split('\n')[0]); process.exit(3); }
  try {
    var page = await browser.newPage({ viewport: { width: 1440, height: 900 } }), errors = [];
    page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
    page.on('pageerror', function (e) { errors.push(String(e)); });

    await page.goto(url + '/garden?bead=' + encodeURIComponent(id));
    await page.locator('#bead').waitFor({ state: 'visible' });
    var panel = await sections(page, '#bv-body');
    if (shots) await page.screenshot({ path: shots + '/onebead-panel.png' });
    // Tab from the panel's Close goes head, conversation, composer, work, in that order (gd-1jjjx7CMU3)
    await page.focus('#bv-x');
    var order = [];
    for (var k = 0; k < 40; k++) {
      await page.keyboard.press('Tab');
      var at = await page.evaluate(function () {
        var a = document.activeElement;
        if (!a || !a.closest('#bv-body')) return '';
        return a.closest('.bv-cv-say') ? 'composer' : a.closest('.bv-sec[data-sec=conversation]') ? 'conversation' : a.closest('.bv-work') ? 'work' : a.closest('.bv-fold') ? 'rest' : 'head';
      });
      if (!at) break;
      if (order[order.length - 1] !== at) order.push(at);
    }
    assert.deepStrictEqual(order.slice(0, 4), ['head', 'conversation', 'composer', 'work'], 'focus order: ' + order.join(' > '));

    await page.goto(url + '/garden/c/n/' + encodeURIComponent(id));
    await page.waitForSelector('#wb2 .wb2-row[aria-selected=true]', { state: 'visible' });
    var node = await sections(page, '#wb2 .wb2-doc .wb2-bead');
    if (shots) await page.screenshot({ path: shots + '/onebead-node.png' });

    assert.ok(/data-face="wish"/.test(panel.face), 'the panel draws the wish face');
    assert.ok(/bv-cv-say/.test(panel.conversation), 'the panel conversation has its reply composer');
    assert.strictEqual(node.face, panel.face, 'the node and the panel draw one face');
    assert.strictEqual(node.conversation, panel.conversation, 'the node and the panel draw one conversation');

    var got = await page.evaluate(function () {
      var doc = document.querySelector('#wb2 .wb2-doc');
      return { rows: document.querySelectorAll('#wb2 .wb2-tree .wb2-row').length, open: document.querySelectorAll('#wb2 [data-rail=open] li').length,
        own: doc.querySelectorAll('.wb2-face, .wb2-transcript, .cm-thread, .cm-node-face').length, title: doc.querySelector('.wb2-head .cm-node-title').textContent,
        busy: doc.hasAttribute('aria-busy') };
    });
    assert.ok(got.rows > 1, 'the node keeps its tree: ' + got.rows + ' rows');
    assert.ok(got.open > 0, 'the node keeps its Open rail');
    assert.strictEqual(got.own, 0, 'no face or transcript of the node’s own');
    assert.ok(got.title && !got.busy, 'the head carries the bead’s title, the document is settled');

    // a bead link in the document (the conversation's "In the thread" or a card's agent, else any) opens that bead here
    var to = await page.evaluate(function () {
      var a = [].filter.call(document.querySelectorAll('#wb2 .wb2-bead a[href*="/garden?bead="]'), function (x) { return !!x.offsetParent && !x.target; })[0];
      if (!a) return '';
      a.dataset.probe = '1';
      return new URLSearchParams(a.search).get('bead');
    });
    assert.ok(to, 'the document links a bead');
    await page.click('#wb2 .wb2-bead a[data-probe]');
    await page.waitForFunction(function (to) { return COMMONS2.state().doc === to && /\/garden\/c\/n\//.test(location.pathname); }, to, { timeout: 10000 });

    // on a phone the panel opens with its newest line on screen, unscrolled (gd-1jjjx7CMU3): the list is no taller than the room under it
    var phone = await browser.newPage({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });
    phone.on('pageerror', function (e) { errors.push(String(e)); });
    await phone.goto(url + '/garden?bead=' + encodeURIComponent(id));
    await phone.locator('#bead').waitFor({ state: 'visible' });
    await sections(phone, '#bv-body');
    await phone.waitForTimeout(300);
    var last = await phone.evaluate(function () {
      var li = document.querySelector('#bv-body .bv-cv-list > li:last-child'), r = li.getBoundingClientRect(), body = document.getElementById('bv-body').getBoundingClientRect();
      return { bottom: Math.round(r.bottom), top: Math.round(r.top), room: Math.round(Math.min(body.bottom, innerHeight)), scrolled: document.getElementById('bv-body').scrollTop };
    });
    assert.strictEqual(last.scrolled, 0, 'the panel is not scrolled');
    assert.ok(last.top >= 0 && last.bottom <= last.room, 'the newest line is on a phone screen unscrolled: ' + JSON.stringify(last));
    // and so does the node's document, pushed from its tree on a phone (its Thread tab)
    await phone.goto(url + '/garden/c/n/' + encodeURIComponent(id));
    await phone.locator('#wb2 .wb2-tree:not(.wb2-tree--kept) .wb2-row[data-id="' + id + '"] > .wb2-line .wb2-t').click();
    await sections(phone, '#wb2 .wb2-doc .wb2-bead');
    await phone.waitForTimeout(300);
    last = await phone.evaluate(function () { var r = document.querySelector('#wb2 .wb2-bead .bv-cv-list > li:last-child').getBoundingClientRect(); return { top: Math.round(r.top), bottom: Math.round(r.bottom), room: innerHeight }; });
    assert.ok(last.top >= 0 && last.bottom <= last.room, 'the node document’s newest line is on a phone screen unscrolled: ' + JSON.stringify(last));
    await phone.close();

    assert.deepStrictEqual(errors, [], 'console errors');
    console.log('onebead: ' + id + ' face ' + node.face.length + ' and conversation ' + node.conversation.length + ' bytes alike; a link opened ' + to + ' here');
  } catch (e) {
    console.error(e && e.stack || e);
    process.exitCode = 1;
  } finally {
    await browser.close();
  }
})();
