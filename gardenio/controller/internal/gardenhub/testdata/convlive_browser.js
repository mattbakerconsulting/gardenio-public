/* Browser check that the open bead panel's conversation is live (gd-MpqfrPhLZP), run from TestConversationLiveUnderChromium
   against the card world with its Commons stream live.
   Usage: node convlive_browser.js <base url> <node_modules dir> <wish id> <its task's id>.

   The fixture wish open as /garden?bead=<id>, its Commons stream open (BEADTALK.listening): a squawk its crew writes to the
   farm (POST /test/squawk) appears as the newest line of the Conversation, marked a squawk, with the section's count and the
   face's replies one up, and a squawk past about 600 characters comes folded behind Show all; its task closing (POST /test/close) greys the task's Work row. Neither reloads the page or reads
   the bead's document again. No console errors. */
'use strict';
var url = process.argv[2], mods = process.argv[3], id = process.argv[4], task = process.argv[5], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log(String(e).split('\n')[0]); process.exit(3); }
  try {
    var page = await browser.newPage({ viewport: { width: 1440, height: 900 } }), errors = [], reads = [];
    page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
    page.on('pageerror', function (e) { errors.push(String(e)); });
    page.on('request', function (r) { var u = new URL(r.url()); if (u.pathname === '/garden/bead/' + id) reads.push(u.pathname + u.search); });

    await page.goto(url + '/garden?bead=' + encodeURIComponent(id));
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function (a) {
      var b = document.querySelector('#bv-body');
      return !!b.querySelector('.bv-sec[data-sec=conversation] .bv-cv-foot > *') && !!b.querySelector('.bv-work-tree li.cm-tn[data-id="' + a.task + '"]') &&
        BEADTALK.listening() === a.id;
    }, { id: id, task: task }, { timeout: 15000 });
    var before = await page.evaluate(function () {
      window.convLiveMark = 1;   // gone on a reload
      var b = document.querySelector('#bv-body'), r = b.querySelector('.bv-face-sum a[data-sec=conversation]');
      return { h: b.querySelector('.bv-sec[data-sec=conversation] > h3').textContent, lines: b.querySelectorAll('.bv-cv-list > li').length, replies: r ? r.textContent : '' };
    });
    var was = reads.length, words = 'live squawk ' + Date.now() + ': the panel hears its thread';

    await page.evaluate(function (a) { return fetch('/test/squawk?thread=' + encodeURIComponent(a.id) + '&text=' + encodeURIComponent(a.words), { method: 'POST' }).then(function (r) { if (!r.ok) throw new Error('squawk: ' + r.status); }); }, { id: id, words: words });
    await page.waitForFunction(function (words) {
      var li = document.querySelector('#bv-body .bv-cv-list > li:last-child');
      return !!li && li.dataset.via === 'squawk' && li.textContent.indexOf(words) >= 0;
    }, words, { timeout: 10000 });
    var after = await page.evaluate(function () {
      var b = document.querySelector('#bv-body'), r = b.querySelector('.bv-face-sum a[data-sec=conversation]'), li = b.querySelector('.bv-cv-list > li:last-child');
      return { h: b.querySelector('.bv-sec[data-sec=conversation] > h3').textContent, lines: b.querySelectorAll('.bv-cv-list > li').length, replies: r ? r.textContent : '',
        who: li.querySelector('.bv-cv-who').textContent, mark: window.convLiveMark };
    });
    var n = function (s) { return +((/(\d+)/.exec(s) || [0, 0])[1]); };
    assert.strictEqual(after.mark, 1, 'no reload');
    assert.strictEqual(after.lines, before.lines + 1, 'one line more');
    assert.strictEqual(n(after.h), n(before.h) + 1, 'the heading counts it: ' + before.h + ' -> ' + after.h);
    assert.ok(before.replies, 'the wish face counts its replies');
    assert.strictEqual(n(after.replies), n(before.replies) + 1, 'the face counts it: ' + before.replies + ' -> ' + after.replies);
    assert.strictEqual(after.who, 'gardenio-one-bead-view-k2p', 'the crew squawked it');

    // a line past about 600 characters comes folded behind Show all (rubric C11, gd-1jjjx7CMU3), and opens whole in place
    var long = 'long squawk ' + Date.now() + ': ' + new Array(80).join('the crew wrote more ');
    await page.evaluate(function (a) { return fetch('/test/squawk?thread=' + encodeURIComponent(a.id) + '&text=' + encodeURIComponent(a.words), { method: 'POST' }).then(function (r) { if (!r.ok) throw new Error('squawk: ' + r.status); }); }, { id: id, words: long });
    await page.waitForFunction(function (head) {
      var li = document.querySelector('#bv-body .bv-cv-list > li:last-child');
      return !!li && li.textContent.indexOf(head) >= 0;
    }, long.slice(0, 30), { timeout: 10000 });
    var folded = await page.evaluate(function () {
      var li = document.querySelector('#bv-body .bv-cv-list > li:last-child'), t = li.querySelector('.cm-cut-head'), b = li.querySelector('.cm-cut-go');
      return { len: t.textContent.length, more: b ? b.textContent : '' };
    });
    assert.ok(folded.len < 640, 'a long line shows about 600 characters: ' + folded.len);
    assert.strictEqual(folded.more, 'Show all ' + long.trim().length.toLocaleString('en-US') + ' characters', 'and says how much it holds');
    await page.click('#bv-body .bv-cv-list > li:last-child .cm-cut-go');
    assert.ok((await page.locator('#bv-body .bv-cv-list > li:last-child .cm-cut-whole').textContent()).indexOf(long.trim()) >= 0, 'Show all shows it whole');

    await page.evaluate(function (task) { return fetch('/test/close?id=' + encodeURIComponent(task), { method: 'POST' }).then(function (r) { if (!r.ok) throw new Error('close: ' + r.status); }); }, task);
    await page.waitForFunction(function (task) {
      var li = document.querySelector('#bv-body .bv-work-tree li.cm-tn[data-id="' + task + '"]');
      return !!li && li.classList.contains('cm-tn--closed') && li.querySelector(':scope > .cm-tn-meta > .cm-state').textContent === 'closed';
    }, task, { timeout: 10000 });
    await page.waitForTimeout(600);   // past the panel's 400 ms pause before any read again

    assert.deepStrictEqual(reads.slice(was), [], 'the bead document is not read again');
    assert.strictEqual(await page.evaluate(function () { return window.convLiveMark; }), 1, 'no reload');
    assert.deepStrictEqual(errors, [], 'console errors');
    console.log('convlive: ' + id + ' heard a squawk (' + before.h + ' -> ' + after.h + ', ' + before.replies + ' -> ' + after.replies + ') and ' + task + ' closing, no read of its document');
  } catch (e) {
    console.error(e && e.stack || e);
    process.exitCode = 1;
  } finally {
    await browser.close();
  }
})();
