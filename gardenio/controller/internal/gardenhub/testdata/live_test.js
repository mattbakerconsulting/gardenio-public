/* The page's one EventSource (web/live.js, P2.4 gd-ymB1Pgg0fQ), run by TestLiveJSUnderNode:  node live_test.js <web dir>
   The pure half: a message's head, the full-jitter backoff, a stream's spec, which refusals are worth asking again and a
   part fetched since laid on a document (patch). The page half, over a fake EventSource and fetch, with Math.random pinned
   to 0 so every backoff is immediate:
   - the streams a turn opens are one connection, each named with its key, last id and url below the root;
   - after the hello, a stream opened or closed is one POST /garden/live/sub, never a reconnect;
   - a stream's events reach its listeners with their lastEventId; :open makes it OPEN;
   - :err 404 closes it for good (readyState 2, then error), :err 502 and :end make it CONNECTING and ask for it again with
     the last id it saw;
   - a dropped connection makes every stream CONNECTING and reconnects with every last id; so does a refused POST;
   - sleep and wake (M3.2 gd-Xo3Stftjv6): offline lets the connection go and #netbar says so at once, in words; online again
     cuts a backoff's wait short; a connection down past the grace says it is reconnecting, and the hello clears the line;
     a page back on screen after a quiet STALE_MS replaces its connection, from every last id;
   - closing the last stream closes the connection; without data-live, source() is a native EventSource, and offline still
     shows the line.
   Prints a JSON report. */
"use strict";
var path = require("path"), assert = require("assert");
var web = process.argv[2];

/* ---------- pure ---------- */
var P = require(path.join(web, "live.js"));
assert.deepStrictEqual(P.frame("3 patch 17\n{\"a\":1}\nsecond"), { key: "3", type: "patch", id: "17", data: "{\"a\":1}\nsecond" });
assert.deepStrictEqual(P.frame("3 hb\n{}"), { key: "3", type: "hb", id: null, data: "{}" });
assert.deepStrictEqual(P.frame("a ping 2026-10-29T03:00:00.5Z\n{}").id, "2026-10-29T03:00:00.5Z");
assert.deepStrictEqual(P.frame("a :err 404\nno such bead"), { key: "a", type: ":err", id: "404", data: "no such bead" });
assert.deepStrictEqual(P.frame("* :hello\n{\"sid\":\"x\"}").key, "*");
assert.strictEqual(P.frame("nospace"), null);
assert.strictEqual(P.frame(""), null);
assert.strictEqual(P.backoff(0, 0), 0);
assert.strictEqual(P.backoff(0, 0.999), 999, "the first wait is below a second");
assert.strictEqual(P.backoff(3, 0.5), 4000);
assert.strictEqual(P.backoff(10, 0.999), 29970, "capped at 30 s");
assert.strictEqual(P.backoff(400, 0.999), 29970, "a long outage stays capped");
var spread = {}; for (var i = 0; i < 200; i++) spread[Math.floor(P.backoff(2) / 1000)] = 1;
assert.deepStrictEqual(Object.keys(spread).sort(), ["0", "1", "2", "3"], "full jitter: anywhere in [0, 4 s)");
assert.strictEqual(P.spec("1", "2026-10-29T03:00:00Z", "/garden/c/events?land=g"), "1,2026-10-29T03%3A00%3A00Z,/garden/c/events?land=g");
assert.strictEqual(P.rel("/v/garden/glass/a/stream", "/v"), "/garden/glass/a/stream");
assert.strictEqual(P.rel("/garden/events", ""), "/garden/events");
[502, 503, 500, 408, 429].forEach(function (s) { assert.ok(P.retryable(s), s + " is for now"); });
[400, 401, 403, 404, 410].forEach(function (s) { assert.ok(!P.retryable(s), s + " is for good"); });
var doc = { id: "gd-a", title: "t", comments: [{ t: 1 }], comments_total: 1, mentions: { "gd-b": "B" }, unseen: ["gd-c"] };
var got = P.patch(doc, { id: "gd-a", comments: [{ t: 2 }], comments_total: 2, mentions: { "gd-d": "D" } });
assert.deepStrictEqual(got, { id: "gd-a", title: "t", comments: [{ t: 1 }, { t: 2 }], comments_total: 2, mentions: { "gd-b": "B", "gd-d": "D" }, unseen: ["gd-c"] });
assert.deepStrictEqual(doc.comments, [{ t: 1 }], "the document patched is left as it was");
assert.deepStrictEqual(doc.mentions, { "gd-b": "B" });
assert.deepStrictEqual(P.patch({}, { comments: [{ t: 1 }], mentions: { x: "X" } }), { comments: [{ t: 1 }], mentions: { x: "X" } }, "a list or object the document lacks is p's");

/* ---------- the page ---------- */
var sources = [], posts = [], postOK = true;
function FakeES(url) { this.url = url; this.readyState = 0; this.closed = false; sources.push(this); }
FakeES.prototype.close = function () { this.closed = true; this.readyState = 2; };
function send(es, text) { es.onmessage({ data: text }); }
function streamsOf(es) {
  return es.url.split("?")[1].split("&").map(function (kv) { var v = decodeURIComponent(kv.slice(2)).split(","); return { k: v[0], l: decodeURIComponent(v[1]), u: v.slice(2).join(",") }; });
}
global.EventSource = FakeES;
global.fetch = function (url, o) { posts.push({ url: url, body: JSON.parse(o.body) }); return Promise.resolve({ ok: postOK, status: postOK ? 204 : 404 }); };
var wl = {}, dl = {}, bar = { hidden: true, textContent: "" }, skew = 0, realNow = Date.now;
Date.now = function () { return realNow() + skew; };
Object.defineProperty(globalThis, "navigator", { value: { onLine: true }, configurable: true, writable: true });
global.window = { addEventListener: function (t, f) { wl[t] = f; } };
global.document = { body: { dataset: { base: "/v", live: "1" } }, hidden: false, addEventListener: function (t, f) { dl[t] = f; },
  getElementById: function (id) { return id === "netbar" ? bar : null; } };
Math.random = function () { return 0; };
var realInterval = setInterval; global.setInterval = function () { return 0; };   // the watchdog is not under test here
delete require.cache[require.resolve(path.join(web, "live.js"))];
var L = require(path.join(web, "live.js"));
global.setInterval = realInterval;
assert.ok(L.shared());

var tick = function () { return new Promise(function (r) { setTimeout(r, 5); }); };
var got = { events: [], bead: [], glass: [] }, opens = 0;
var ev = L.source("/v/garden/events?since=4");
ev.addEventListener("patch", function (e) { got.events.push({ data: e.data, id: e.lastEventId }); });
ev.addEventListener("hb", function (e) { got.events.push({ hb: true, id: e.lastEventId }); });
ev.onopen = function () { opens++; };
ev.onerror = function () { got.events.push({ error: ev.readyState }); };
var farm = L.source("/v/garden/farm/events?since=0");
assert.strictEqual(sources.length, 0, "nothing opens before the turn ends");

(async function () {
  await tick();
  assert.strictEqual(sources.length, 1, "two streams in one turn are one connection");
  var c1 = sources[0];
  assert.ok(c1.url.indexOf("/v/garden/live/events?") === 0, c1.url);
  assert.deepStrictEqual(streamsOf(c1), [{ k: ev.key, l: "", u: "/garden/events?since=4" }, { k: farm.key, l: "", u: "/garden/farm/events?since=0" }]);

  // before the hello a new stream waits; the hello sends it
  var bead = L.source("/v/garden/bead/gd-x/events");
  bead.onerror = function () { got.bead.push(bead.readyState); };
  await tick();
  assert.strictEqual(sources.length, 1, "a stream opened later never reconnects");
  assert.strictEqual(posts.length, 0, "no sid yet");
  send(c1, "* :hello\n{\"sid\":\"s1\"}");
  await tick();
  assert.deepStrictEqual(posts, [{ url: "/v/garden/live/sub", body: { sid: "s1", add: [{ k: bead.key, u: "/garden/bead/gd-x/events" }], rm: [] } }]);

  // events reach their stream
  send(c1, ev.key + " :open");
  assert.strictEqual(ev.readyState, 1); assert.strictEqual(opens, 1);
  send(c1, ev.key + " patch 5\n{\"prev\":4}");
  send(c1, ev.key + " hb\n{}");
  send(c1, "zz patch 9\n{}");   // a key nobody holds
  assert.deepStrictEqual(got.events, [{ data: "{\"prev\":4}", id: "5" }, { hb: true, id: "5" }], "an event with no id keeps the last one");

  // a refusal for good: closed, never asked again
  send(c1, bead.key + " :err 404\nno such bead");
  assert.strictEqual(bead.readyState, 2); assert.deepStrictEqual(got.bead, [2]);
  await tick();
  assert.strictEqual(posts.length, 1, "a 404 is not asked for again");

  // a stream refused for now, and one that ended: CONNECTING, then asked for again with the last id it saw
  send(c1, farm.key + " :open");
  send(c1, farm.key + " patch 12\n{}");
  send(c1, farm.key + " :err 502\nthe beads daemon did not answer");
  assert.strictEqual(farm.readyState, 0);
  await tick(); await tick();
  assert.deepStrictEqual(posts[1].body, { sid: "s1", add: [{ k: farm.key, l: "12", u: "/garden/farm/events?since=0" }], rm: [] });
  send(c1, ev.key + " :end");
  assert.strictEqual(ev.readyState, 0);
  await tick(); await tick();
  assert.deepStrictEqual(posts[2].body.add, [{ k: ev.key, l: "5", u: "/garden/events?since=4" }]);

  // a card's stream: opened and closed by POSTs, the others untouched
  var card = L.source("/v/garden/glass/wall?agent=gd-a");
  await tick();
  assert.deepStrictEqual(posts[3].body.add, [{ k: card.key, u: "/garden/glass/wall?agent=gd-a" }]);
  card.close();
  await tick();
  assert.deepStrictEqual(posts[4].body, { sid: "s1", add: [], rm: [card.key] });
  assert.strictEqual(sources.length, 1, "still one connection");

  // a dropped connection: every stream CONNECTING, then one new connection naming each with its last id
  send(c1, ev.key + " :open"); send(c1, farm.key + " :open");
  got.events = [];
  c1.onerror({});
  assert.ok(c1.closed, "the browser's own retry is never used");
  assert.deepStrictEqual(got.events, [{ error: 0 }]);
  assert.strictEqual(farm.readyState, 0);
  await tick();
  assert.strictEqual(sources.length, 2);
  var c2 = sources[1];
  assert.deepStrictEqual(streamsOf(c2), [{ k: ev.key, l: "5", u: "/garden/events?since=4" }, { k: farm.key, l: "12", u: "/garden/farm/events?since=0" }]);
  send(c2, "* :hello\n{\"sid\":\"s2\"}");
  send(c1, ev.key + " patch 99\n{}");   // the old connection is gone
  assert.strictEqual(got.events.length, 1);

  // a refused POST (the connection went, or another replica has it): reconnect, and the new one names them all
  postOK = false;
  var plan = L.source("/v/garden/plan/gd-p/events");
  await tick(); await tick(); await tick();
  assert.strictEqual(sources.length, 3, "a refused POST reconnects");
  assert.deepStrictEqual(streamsOf(sources[2]).map(function (s) { return s.u; }), ["/garden/events?since=4", "/garden/farm/events?since=0", "/garden/plan/gd-p/events"]);
  postOK = true;

  // sleep and wake: offline is said at once, and the connection let go; a backoff waits (Math.random near 1 now)
  send(sources[2], "* :hello\n{\"sid\":\"s3\"}");
  assert.ok(bar.hidden, "connected: no offline line");
  Math.random = function () { return 0.999; };
  navigator.onLine = false; wl.offline({ type: "offline" });
  assert.ok(sources[2].closed, "offline: the connection is let go");
  assert.ok(!bar.hidden && /^Offline · as of \d\d:\d\d$/.test(bar.textContent), bar.textContent);
  await tick();
  assert.strictEqual(sources.length, 3, "a backoff's wait, not a retry at once");
  navigator.onLine = true; wl.online({ type: "online" });
  await tick();
  assert.strictEqual(sources.length, 4, "online again: no backoff's wait");
  assert.deepStrictEqual(streamsOf(sources[3]).map(function (s) { return s.l; }), ["5", "12", ""], "every stream from its last id");
  assert.ok(bar.hidden, "online, within the grace: no line for a blip");
  skew += 3000; dl.visibilitychange({ type: "visibilitychange" });
  assert.ok(!bar.hidden && /^Reconnecting · as of \d\d:\d\d$/.test(bar.textContent), "past the grace, in words: " + bar.textContent);
  send(sources[3], "* :hello\n{\"sid\":\"s4\"}");
  assert.ok(bar.hidden, "the hello clears the line");
  // asleep with the connection held: quiet past STALE_MS on wake is replaced at once, never left to the watchdog
  document.hidden = true; skew += 25000; dl.visibilitychange({ type: "visibilitychange" });
  assert.strictEqual(sources.length, 4, "nothing while hidden");
  document.hidden = false; dl.visibilitychange({ type: "visibilitychange" });
  assert.ok(sources[3].closed && farm.readyState === 0, "the quiet connection went, every stream CONNECTING");
  await tick();
  assert.strictEqual(sources.length, 5, "and was replaced on wake, with no backoff's wait");
  assert.deepStrictEqual(streamsOf(sources[4]).map(function (s) { return s.l; }), ["5", "12", ""]);
  send(sources[4], "* :hello\n{\"sid\":\"s5\"}");
  dl.visibilitychange({ type: "visibilitychange" });
  await tick();
  assert.strictEqual(sources.length, 5, "a connection heard from lately is kept on wake");
  Math.random = function () { return 0; };

  // the last stream closed: the connection goes
  ev.close(); farm.close(); plan.close();
  await tick();
  assert.ok(sources[4].closed, "no stream, no connection");
  assert.strictEqual(sources.length, 5);

  // a page without data-live: native EventSources
  global.document = { body: { dataset: { base: "" } }, addEventListener: function (t, f) { dl[t] = f; }, getElementById: function () { return bar; } };
  delete require.cache[require.resolve(path.join(web, "live.js"))];
  global.setInterval = function () { return 0; };
  var N = require(path.join(web, "live.js"));
  global.setInterval = realInterval;
  var n = N.source("/garden/events?since=1");
  assert.ok(n instanceof FakeES && n.url === "/garden/events?since=1", "no shared stream: the browser's own");
  navigator.onLine = false; wl.offline({ type: "offline" });
  assert.ok(!bar.hidden && /^Offline · as of/.test(bar.textContent), "the browser's own streams: offline is still said");
  navigator.onLine = true; wl.online({ type: "online" });
  assert.ok(bar.hidden, "and cleared once online");

  console.log(JSON.stringify({ connections: sources.length, posts: posts.length }));
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
