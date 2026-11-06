(function(){
var B=document.body,me=B.dataset.me||'',V=+B.dataset.v,base=B.dataset.base||'';
var SECS=['decision','work','agent'],L={},N={},S={};
SECS.forEach(function(s){L[s]=document.getElementById('l-'+s);N[s]=document.getElementById('n-'+s);S[s]=document.getElementById('s-'+s);});
var scope='mine';
try{scope=localStorage.getItem('garden.scope')||'mine';}catch(e){}
if(!me)scope='all';   // "Mine" needs an identity; without one it would show an empty page while the plans show everything
var live=document.getElementById('live'),livetxt=document.getElementById('livetxt'),livesr=document.getElementById('livesr'),sub=document.getElementById('sub'),liveState='';
var lastMsg=Date.now(),es=null,hiddenAt=0,CUT=B.dataset.cut==='1';

function tag(li){li.classList.toggle('mine',!!me&&li.dataset.op===me);var who=li.querySelector('.who[data-who]');if(who&&li.classList.contains('mine'))who.textContent='asked of you';var a=li.querySelector('.act');if(a&&typeof TITLES!=='undefined')TITLES.dress(a);}   // a bead id in the ask or context reads as its title (web/titles.js)
function ago(t){var d=Math.max(0,Math.floor(Date.now()/1000)-t);if(d<60)return d+'s';if(d<3600)return Math.floor(d/60)+'m';if(d<86400)return Math.floor(d/3600)+'h';return Math.floor(d/86400)+'d';}
function times(){var a=document.getElementsByTagName('time');for(var i=0;i<a.length;i++){var t=+a[i].dataset.t;if(t)a[i].textContent=ago(t)+' ago';}}
function counts(){
  var c={};
  SECS.forEach(function(s){
    /* an in-hand escalation (the desk's or the operators') is listed, not counted (gd-bmOiteQi36) */
    var n=L[s].querySelectorAll(scope==='mine'?'.item.mine:not(.inhand)':'.item:not(.inhand)').length,all=L[s].querySelectorAll('.item:not(.inhand)').length,em=S[s].querySelector('.empty');c[s]=n;
    N[s].textContent=n;S[s].classList.toggle('none',n===0);
    if(em){if(em.dataset.base===undefined)em.dataset.base=em.textContent;
      var msg=em.dataset.base+(n===0&&all>0?' '+all+' more under Everyone.':'');if(em.textContent!==msg)em.textContent=msg;}
  });
  B.classList.toggle('has-ripe',c.decision>0);var g=gcount();if(g)c.agent=g.n;   // the Porch's count once the farm has gardeners[]: the Ledger's agents are another set
  /* the counts sit on the HUD's title plaque; a phone drops the .lw words (the Mine / Everyone buttons already say whose) */
  var line=c.decision+' <span class="lw">ready </span>to harvest · '+c.work+' growing · '+plural(c.agent,'gardener','gardeners')+'<span class="lw">'+(scope==='mine'?' · yours':' · everyone')+'</span>';
  if(sub.dataset.line!==line){sub.dataset.line=line;sub.innerHTML=line;}   // numbers and fixed words only
  syncDock(c);
  farmChanged();
}
function setScope(s){
  scope=s;try{localStorage.setItem('garden.scope',s);}catch(e){}
  B.classList.toggle('scope-mine',s==='mine');
  document.getElementById('b-mine').setAttribute('aria-pressed',s==='mine');
  document.getElementById('b-all').setAttribute('aria-pressed',s==='all');
  counts();
}
/* ---------- dock: Porch (gardeners) and Mailbox (decisions) mirror the ledger lists, so the stream only feeds one place ---------- */
var D={porch:document.getElementById('d-porch'),mail:document.getElementById('d-mail')},DSEC={porch:'agent',mail:'decision'};
var screen='farm';
/* The dock is reconciled, not rebuilt: a stream patch must not wipe a half-typed answer or an armed button. A row is reused while its source row is unchanged (same data-u). */
function mirror(key){
  var ul=D[key],src=L[DSEC[key]].children,pane=document.getElementById('pane-'+key),have={},want=[],i;
  for(i=0;i<ul.children.length;i++)have[ul.children[i].dataset.k?'k:'+ul.children[i].dataset.k:ul.children[i].dataset.src]=ul.children[i];   // a Porch row from gardeners[] (data-k, web/porch.js) never stands in for a Ledger row, so it goes (gd-aVUvUN8gQy)
  for(i=0;i<src.length;i++){
    if(scope==='mine'&&!src[i].classList.contains('mine'))continue;
    var id=src[i].id,old=have[id];
    if(old&&old.dataset.u===src[i].dataset.u){want.push(old);delete have[id];continue;}
    var c=parse(src[i].outerHTML).firstElementChild;c.dataset.src=id;c.removeAttribute('id');c.classList.remove('fresh');want.push(c);   // not cloneNode: Chromium fetches a cloned lazy <img>
  }
  Object.keys(have).forEach(function(k){if(have[k].parentNode===ul)ul.removeChild(have[k]);});
  want.forEach(function(n,j){if(ul.children[j]!==n)ul.insertBefore(n,ul.children[j]||null);});
  pane.classList.toggle('none',ul.children.length===0);
}
function pip(ids,n){ids.forEach(function(id){var p=document.getElementById(id);p.textContent=n>0?n:'';});}
function syncDock(c){
  if(!porch()){mirror('porch');pip(['tpip-porch','dpip-porch'],c.agent);}
  mirror('mail');BEACONS.mapLinks(D.mail,world,{base:base,go:mailGo});times();pip(['tpip-mail','dpip-mail'],c.decision);
}
/* The Porch from the farm's gardeners[] (web/porch.js) once a farm document has them, where the map placed each one, else the Ledger's mirror; gcount() is that set in scope, the one gardener count (gd-rC46vJomwy); barnCounts() is it by the biome whose Barn each stands in (28.6). */
function gplot(g){var s=gspots[3].spots[g.id],p=s&&s.plot&&world.index[s.plot];return p?{id:p.id,title:WORLD.standWhere(p),name:WORLD.standTitle(p),place:p.kind==='plot'?'':WORLD.STAND_PLACE[p.kind]}:null;}   // a plot's plan, or "In the Meadow: <bead>" beside a band item (gd-6FCoWelnx2): place names the band, name the bead
/* The cursor onto what a gardener stands at: into a plot, onto a band item or a goat. */
function standGo(p,keyed){return p.kind==='plot'?cursorInto(p.id):cursorTo(p.id,keyed);}function gcount(gs){return gspots?PORCH.groups(gs||farmDoc.gardeners,gplot,{me:me,only:scope==='mine',land:function(g){var s=gspots[3].spots[g.id];return !!(s&&s.land);}}):null;}
function barnCounts(){if(!gspots||!world)return null;var by={},out={};farmDoc.gardeners.forEach(function(g){var id=WORLD.homeFor(g);if(!world.index[id])id='project:';(by[id]=by[id]||[]).push(g);});Object.keys(by).forEach(function(id){out[id]=gcount(by[id]);});return out;}
function porch(){return !!gspots&&PORCH.sync(farmDoc.gardeners,{me:me,only:scope==='mine',base:base,night:night,ago:ago,go:porchGo,plotOf:gplot,whereOf:function(g){return WORLD.fieldWhere(gspots,g);},escOf:function(g){return world&&world.escs&&g.escalation?world.escs[g.escalation]||null:null;}});}
/* "Go to plot": the farm, the cursor inside the gardener's plot (or on its field figure at home), focus on the cursor. A jump, as the bead view's "Show on the map" (3b R13): the plot's framing leaves the fence the gardener stands on out of view. zoom (the Glass's Enter, H-D21): plotGo's flight to its plot figure instead. */
function porchGo(id,zoom){var s=gspots&&gspots[3].spots[id],p=s&&s.plot&&world.index[s.plot];showScreen('farm');if(s&&s.land){cursorTo(s.land,true);return;}if(zoom&&p){plotGo(id,p);return;}if(!p){cursorTo(id,true);return;}if(standGo(p,true))focusCursor();}
/* The Glass's second stop (H-D21): the cursor on the gardener's plot (or the band item it stands at), the camera flown to its
   plot figure at the map's full zoom (frameCursor's "zoom"), and focus on the figure's link once it is drawn (figFocus,
   syncHots), whose Enter goes home to its field figure (fieldGo). */
var zoomOn=null,figFocus='';
function plotGo(id,p){
  var h=gardenerHots.filter(function(x){return x.kind==='gardener'&&x.id===id;})[0],sp=gspots[3].spots[id],g=gspots.byId[id];
  if(!navOpen||!nav.to(p.id).found)return;
  auto=false;zoomOn=h?{x:h.x+h.w/2,y:h.y+h.h/2}:sp&&isFinite(sp.x)?{x:sp.x+T/2,y:sp.y+T*.75}:null;
  cursorMoved('to','zoom');
  figFocus=h?id:'';syncHots();   // after cursorMoved's own focus: the figure, not the plot's cell, holds it
  signsNext('On '+WORLD.standTitle(p)+', '+((g&&g.agent)||id)+(coarse?': tap it to go home to the yard':': Enter goes home to the yard'));   // a phone has no Enter (gd-a628FpJRO5)
}
/* A field figure's click or Enter (H-D21): its Glass, the figure the opener (below the operator tier the Workbench opens its
   transcript alone, which has no way on to the plot, gd-6LpSDBI9cK). A plot figure's: back to the yard, the cursor on its field
   figure, the strip framed. A phone has no Enter: its sign names the Glass's button (gd-a628FpJRO5). */
function fieldGlass(g,opener){var n=g.agent||g.id;benchOpen('glass',g.id,{opener:opener});signsNext(B.dataset.glass==='1'?'Watching '+n+' in the Glass; '+(coarse?'Go to its plot takes you there':'Enter again goes to its plot'):'Reading the transcript of '+n);}
function fieldGo(g){var s=gspots&&gspots[3].spots[g.id],b=s&&s.field&&world.index[s.field.home];showScreen('farm');if(!cursorTo(WORLD.FIELD+g.id,true)&&!(b&&cursorTo('home:'+b.key+'/growing',true)))toast((g.agent||g.id)+' has no place in its field right now.');}
function standG(n){var it=n&&n.kind==='item'&&n.ref&&n.ref.kind==='stand'&&n.ref.item;return it&&it.g||null;}   // the gardener of a field figure's cursor node
/* A Mailbox row's "Open on the map" (web/beacons.js mapLinks): the farm, the cursor on the plan's plot, as its ?at= lands. */
function mailGo(id){showScreen('farm');cursorTo(id,true);}
function showScreen(s){
  screen=s;B.dataset.screen=s;
  var dd=document.getElementById('dw-dock');if(dd&&s!=='farm')dd.hidden=false;   // the dock drawer holds the panes; a phone screen, a #mail link or a Porch/Mailbox button opens it
  var pane=s==='farm'?(D.pane||'porch'):s;
  if(s!=='farm')D.pane=s;
  ['porch','mail'].forEach(function(k){
    var on=k===pane,pe=document.getElementById('pane-'+k);
    if(!on&&pe.contains(document.activeElement))document.activeElement.blur();   // a pane hidden with focus in it (the Porch's purse chip, then #mail) lets it go now, not at the browser's next focus fixup, so the map's keys are the map's again
    pe.hidden=!on;
    document.getElementById('dbtn-'+k).setAttribute('aria-expanded',on&&!dd.hidden);
  });
  markTabs();
  try{if(history.replaceState)history.replaceState(history.state,'',s==='farm'?location.pathname+location.search+(/^#(mail|porch)$/.test(location.hash)?'':location.hash):'#'+s);}catch(e){}   // the state is a bead panel's (web/workbench.js)
  if(s==='farm')farmChanged();
}
/* The phone's tab on screen (M1.4 gd-4Y2qOgfmJE): the Glass or Workbench while its panel shows over the map, else the screen. lastPanel: the panel each of those two last showed, which its tab opens again. */
var lastPanel={};
function wbNow(){return typeof WORKBENCH!=='undefined'&&WORKBENCH.current?WORKBENCH:null;}
function panelTab(p){return (p.kind==='glass'||p.kind==='creds')&&document.querySelector('#tabbar [data-go=glass]')?'glass':'workbench';}
function tabOn(){var wb=wbNow(),p=wb&&wb.current(),t=p&&panelTab(p);if(t)lastPanel[t]=p;return t&&screen==='farm'?t:screen;}
function markTabs(){var on=tabOn();Array.prototype.forEach.call(document.querySelectorAll('#tabbar button'),function(b){b.setAttribute('aria-current',b.dataset.go===on?'page':'false');});}
/* The Workbench or Glass tab: its last panel, else the inbox or the Wall, pushing no history. */
function openTab(t){
  var wb=wbNow(),cur=wb&&wb.current();
  if(cur&&panelTab(cur)===t)return;
  var p=lastPanel[t]||(t==='glass'?{kind:'glass',id:'wall'}:{kind:'queue',id:'inbox'});
  var go=function(){
    if(wbNow()){WORKBENCH.open(p.kind,p.id,{replace:true});return;}
    LAZY.need('workbench').then(function(){if(wbNow()&&!WORKBENCH.current())WORKBENCH.open(p.kind,p.id,{replace:true});},function(){toast('The Workbench did not load. Try again.');});
  };
  go();
}
var mailPane=document.getElementById('pane-mail');
document.getElementById('mail-all').onclick=function(){delete mailPane.dataset.only;};
/* The Mailbox opens whole (the Escalations cell then narrows it to escalations, openThing, 28.13). A phone shows one screen at a time (another tab closes a panel over the map); on desktop a screen is the pane the drawer shows. The Commons tab is the workbench's page (commonsHref). */
function goPane(s){
  if(s==='mail')delete mailPane.dataset.only;
  var wb=wbNow();
  if(s==='commons'){location.assign(commonsHref());return;}
  if(s==='workbench'||s==='glass'){openTab(s);return;}
  if(window.innerWidth<900&&wb&&wb.current()){var f=document.activeElement;wb.close({tab:true});if(f&&f.closest&&f.closest('#tabbar'))try{f.focus({preventScroll:true});}catch(e){f.focus();}}
  showScreen(s);if(window.innerWidth<900)window.scrollTo(0,0);
}
/* The Commons: the steward of the page's ?land=, else the viewer's (/garden/c/s, web/commons2.js; the band it replaced is
   gone, gardenio/docs/adr-commons-band-replaced.md). */
function commonsHref(){var l=new URLSearchParams(location.search).get('land');return (B.dataset.base||'')+'/garden/c/s'+(l?'/'+encodeURIComponent(l):'');}
(function(a){if(a)a.href=commonsHref();})(document.getElementById('b-commons'));
/* ---------- act in place: answer a decision, or approve / reject an escalation, from the Mailbox (web/answer.js) ---------- */
var toastTimer=0,toastEl=document.getElementById('toast');
function toast(msg){toastEl.textContent=msg;if(typeof TITLES!=='undefined')TITLES.dress(toastEl);toastEl.hidden=false;clearTimeout(toastTimer);toastTimer=setTimeout(function(){toastEl.hidden=true;},5000);}
ANSWER.install({base:base,toast:toast,mail:D.mail,counts:counts});var post=ANSWER.post;
function parse(html){var t=document.createElement('template');t.innerHTML=html;return t.content;}
function upsert(sec,html){
  var li=parse(html).firstElementChild;if(!li||!L[sec])return;
  if(ANSWER.stale(li.id.slice(2)))return;   // answered here: the server closed it, so this is a snapshot from before
  var old=document.getElementById(li.id);if(old)old.remove();
  tag(li);li.classList.add('fresh');
  var u=+li.dataset.u,kids=L[sec].children,at=null;
  for(var i=0;i<kids.length;i++){if(+kids[i].dataset.u<u){at=kids[i];break;}}
  L[sec].insertBefore(li,at);
}
function told(d){try{document.dispatchEvent(new CustomEvent('garden:lists',{detail:d}));}catch(e){}}   // for web/queue.js
function reset(r){
  ANSWER.clear();
  SECS.forEach(function(s){L[s].innerHTML=r.html[s]||'';Array.prototype.forEach.call(L[s].children,tag);});
  V=r.v;times();counts();told({reset:1});
}
function apply(p){
  if(p.prev!==V){connect();return;}   // gap: reconnect with our version; server answers with a reset
  (p.rm||[]).forEach(function(id){ANSWER.landed(id);var e=document.getElementById('i-'+id);if(e)e.remove();});
  (p.up||[]).forEach(function(f){upsert(f.sec,f.html);});
  V=p.v;times();counts();told(p);
}
function setLive(state,text){if(liveState===state)return;liveState=state;livesr.textContent=text;}   // announced only when the connection state changes, not on the ticking clock
function status(){
  if(CUT){live.className='live off';livetxt.textContent='as of page load';setLive('off','Lists as of page load');return;}   // body data-cut: the lists are this viewer's cut, painted once; the hub's stream is admin-only (http.go Cut)
  if(!es||es.readyState!==1){live.className='live off';livetxt.textContent='reconnecting';setLive('off','Live updates lost, reconnecting');return;}
  var s=Math.round((Date.now()-lastMsg)/1000);
  live.className='live on';livetxt.textContent='live · synced '+(s<2?'just now':s+'s ago');setLive('on','Live updates connected');
}
function connect(){
  if(CUT)return;
  if(es)es.close();
  es=LIVE.source(base+'/garden/events?since='+V);   // web/live.js
  es.onopen=function(){lastMsg=Date.now();status();};
  es.onerror=status;
  es.addEventListener('patch',function(e){lastMsg=Date.now();apply(JSON.parse(e.data));status();});
  es.addEventListener('reset',function(e){lastMsg=Date.now();reset(JSON.parse(e.data));status();});
  es.addEventListener('hb',function(){lastMsg=Date.now();status();});
}
/* ---------- farm: the canvas scene is drawn from the same list DOM the stream keeps current ---------- */
var STALE_S=12*3600;                        // in progress with no update for this long = wilted
var ZW={mailbox:'decision',growing:'growing',barn:'agent'};   // a strip zone's item in WORLD.itemWords' words
var cv=document.getElementById('cv'),ctx=cv.getContext('2d'),board=document.getElementById('board');
var hotsEl=document.getElementById('hots'),lblsEl=document.getElementById('lbls');
/* Reduced motion and coarse pointers are read live (section 8): a flip mid-session repaints at once. */
var reduceMq=window.matchMedia?window.matchMedia('(prefers-reduced-motion: reduce)'):null,reduce=!!(reduceMq&&reduceMq.matches);
var coarseMq=window.matchMedia?window.matchMedia('(pointer: coarse)'):null,coarse=!!(coarseMq&&coarseMq.matches);
function onMq(q,f){if(!q)return;if(q.addEventListener)q.addEventListener('change',f);else if(q.addListener)q.addListener(f);}
var night=SKY.night(),frame=0,model=null,slots={},homeFill=null,farmQueued=false,escLin={};   // slots[project][zone]: each strip item's sticky slot (WORLD.homeFill, 28.2); homeFill: the strips' letters, plants and figures by biome id, the cursor tree's, that the scene paints (28.8); escLin: ESCKIND.lineage of the plans
var raf=window.requestAnimationFrame?function(f){return window.requestAnimationFrame(f);}:function(f){return setTimeout(f,16);};

/* ---------- camera: pan and zoom over the world (gardenio/docs/homestead-world-design.md; pure maths in web/world.js) ----------
   Every biome, the Commons at the origin included, carries its home strip (28.1); everything is cut into cached chunks, so the
   canvas is the viewport only and the work per frame is the chunks that meet it. */
var worldBox=WORLD.domainRect(0,0);
var DPR_MAX=2,vp={w:0,h:0,dpr:1},cam={cx:0,cy:0,z:1},level=3,auto=true,anim=null,renderQueued=false;
var SC=SCENE.scene(cv);HERALDRY.init(base,function(){queueRender();syncBeacons();});   // the World's drawing and its caches (web/scene.js); a plan's arms PNG repaints the map when it arrives; its plot gate and the words its hots say (web/heraldry.js)
var hotNodes={},hotKey='';
/* ---------- the world: one biome per project, laid out by WORLD.layout from the /garden/farm document ----------
   (design section 3-4). plans.js fetches the document and hands it over on a 'farmdoc' event; the layout is sticky through a
   per-browser memo (localStorage garden.world.v1) so growth moves nothing already placed, and the beds' cells through a per-tab
   memo (sessionStorage garden.cells.v1, gd-xhpccozIcC) so a bead keeps its cell across documents and a refresh. */
var MEMO_KEY='garden.world.v1',CELLS_STORE='garden.cells.v1',world=null,farmDoc=null,memo=null,cellMemo=null,footSig={},stripHots=[],biomeHots=[];
var gspots=null,sst=null,gardenerHots=[],homeSig='';   // the farm's gardeners (homestead-agents-as-people.md): WORLD.gardenerState (null until a farm document is in, when the barns hold the Ledger's agents), their links as world hots, and the barns' villagers' signature
var T=WORLD.TILE;
var NB=NEIGHBOURS,nbStyles={};function lit(){return scope==='mine'&&!!me;}   // whose plan (web/neighbours.js, gd-MFcv2hSfue): operator fences per farm document, and Mine (lit) dims everyone else's plots
/* '/' jump and deep links (design section 7): jumpIdx the search index (rebuilt per layout), pendingAt a ?at= target waiting
   for the first layout, urlTimer the settle debounce for the address bar. */
var jumpIdx=null,pendingAt=null,urlTimer=0,urlHold=false;
var jumpEl=document.getElementById('jump'),jumpQ=document.getElementById('jump-q'),jumpList=document.getElementById('jump-list'),jumpHits=[],jumpSel=0;   // labels per level (section 6); at Bead the signs still carry their names, tasks are tooltip-only
try{memo=JSON.parse(localStorage.getItem(MEMO_KEY)||'null');}catch(e){memo=null;}
try{cellMemo=JSON.parse(sessionStorage.getItem(CELLS_STORE)||'null');}catch(e){cellMemo=null;}
/* The cursor (web/curnav.js over web/cursor.js; gardenio/docs/superpowers/specs/2026-10-20-lands-cursor-grid-design.md, cited
   `spec n`): nav holds the tree, the path and its memory; navOpen once the opening cursor is placed (the first layout); camTo the
   camera the cursor last framed; looked while GARDEN.look's camera stands; #ghots the gardeners' links beside the grid; #cursay the polite live region for descends. */
var nav=CURNAV.nav(),navOpen=false,camTo=null,looked=false,fitAt=null,camShift=null,ghotsEl=document.getElementById('ghots'),sayEl=document.getElementById('cursay'),tourEl=document.getElementById('tour');

function readSec(sec){
  var out=[],kids=L[sec].children,now=Date.now()/1000;
  for(var i=0;i<kids.length;i++){
    var li=kids[i],a=li.querySelector('a.t');
    if(!a||li.classList.contains('inhand')||(scope==='mine'&&!li.classList.contains('mine')))continue;   // an in-hand escalation is the desk's or the operators', not an ask of yours (gd-bmOiteQi36)
    out.push({id:li.id.slice(2),href:a.getAttribute('href'),title:a.textContent,project:li.dataset.pj||'',state:li.dataset.st||'',type:li.dataset.ty||'',
      prio:li.dataset.pr===undefined?2:+li.dataset.pr,blocked:li.classList.contains('blocked'),report:li.dataset.rp==='1',u:+li.dataset.u,
      stale:sec==='work'&&now-(+li.dataset.u)>STALE_S,armsName:li.dataset.aname||''});
  }
  return out;
}
/* Before the farm's gardeners are in, the home strips' figures are the hub's agent rows (data-role); each wears the look the farm's gardener
   will (WORLD.gardenerLook: its own shirt, the straw hat until its plot is known), so nothing changes colour when the farm streams in (gd-wJVEevAYPP). */
function hubWear(items){items.forEach(function(it){it.wear=WORLD.gardenerLook(it);});return items;}
/* The page's model (28.2): the Ledger's asks and loose work and the Growing fields' stands (every running gardener, those out on
   plots first, 28.12), farm-wide and uncapped; CURNAV.homeSpec splits them by project into each strip, which caps each zone. */
function buildModel(){
  var m={decision:readSec('decision'),growing:readSec('work'),agent:gspots?WORLD.gardenerField(gspots,scope==='mine'&&me,base,0,ANIM.still):hubWear(readSec('agent')),items:{}};
  ['decision','growing','agent'].forEach(function(z){m[z].forEach(function(it){m.items[it.id]=it;});});
  return m;
}
function render(){
  renderQueued=false;
  if(!vp.w||!vp.h||veil)return;   // under the veil the panel covers the map (lift queues a frame)
  if(SC.draw({cam:cam,vp:vp,level:level,night:night,frame:frame,reduce:reduce,coarse:coarse,world:world,gs:gspots,sst:sst,edgeIx:edgeIx,weedIx:weedIx,nb:nbStyles,mine:lit()&&me,safe:EDGES.safeView(signView,cam,vp),bcns:bcnNow,fill:homeFill,gate:gatePlotId()}))queueRender();   // the World (web/scene.js)
  if(world)VIEWCUT.see({level:level,view:WORLD.viewRect(cam,vp,0),gs:gspots});   // tell the farm stream what is in view (web/viewcut.js)
  drawGlyphs();
}
function queueRender(){if(renderQueued)return;renderQueued=true;raf(function(){if(renderQueued)render();});}   // a frame render() already painted is not painted twice

/* Size the canvas backing store to the viewport in device pixels, at most DPR_MAX of them per CSS px; keep the camera where it was (or fit the world if the cursor is not open yet).
   Every frame of a camera move repaints the whole store, so its pixel count is the frame's raster cost: on a 2.75x phone a 2x store is
   half the pixels, and the art's pixels were already a non-integer count of device pixels there (gd-PoCdbE1SJX). #cv scales up pixelated.
   Sizing clears the canvas after this frame's rAF, so once the cursor is open it paints now: a sliding phone toolbar resizes every frame (gd-KZDKktrdQC). */
function resize(){
  var w=board.clientWidth,h=board.clientHeight,d=Math.min(window.devicePixelRatio||1,DPR_MAX);
  if(w===vp.w&&h===vp.h&&d===vp.dpr)return;
  vp={w:w,h:h,dpr:d};
  cv.width=Math.max(1,Math.round(w*d));cv.height=Math.max(1,Math.round(h*d));
  if(!w||!h)return;
  if(!navOpen)cam=WORLD.fit(worldBox,vp);else if(!looked)frameCursor('',true);   // a camera the viewer moved stays where they left it
  camChanged();signsSoon();
  if(!navOpen&&world&&!veil){openCursor();welcomeOnce();}
  if(navOpen)render();
}
/* The camera frames the cursor (spec 4.1, 4.2), the world's box until the first layout places it, on a jump to a Thing (the
   palette, the outline, a beacon, 0, Home, n): it eases in 240 ms (reduced motion: a cut, flyTo). A move or a sibling jump
   moves no camera, and In and Out are a zoom step (spec revision 3, zoomStep). */
function camVp(){
  var c=camInset(),v={w:vp.w,h:vp.h,top:Math.max(c.top,hdrBottom()),right:c.right,bottom:footInset(),coarse:coarse},t=tourEl.hidden?null:TOUR.measure(board,vp,hdrBottom());   // the chore tour's card is an inset while it is up (spec 8)
  if(t){v.top=Math.max(v.top,t.top);v.right=Math.max(v.right,t.right);v.bottom=Math.max(v.bottom,t.bottom);v.left=t.left;}
  return v;
}
/* The board px a fit keeps clear of the camera buttons: on a desktop, where they are a row along the top, down to its bottom
   (gd-zjATSCW3s1); on a phone, where they are a column on the right edge, in from its left side, so a channel leaving the
   framed region there is not under Go and the Menu (gd-pyrowrsfDF). */
function camInset(){
  var c=document.querySelector('.cam'),r=c&&c.getBoundingClientRect(),o=board.getBoundingClientRect();
  if(!r||!r.height)return {top:0,right:0};
  return getComputedStyle(c).flexDirection==='column'?{top:0,right:Math.max(0,Math.round(o.right-r.left))}:{top:Math.max(0,r.bottom-o.top),right:0};
}
/* fit(v) frames path's grid into the HUD insets v; an arrow (a beacon) that would sit on one of the grid's Things at that framing
   makes its edge band one more inset and the grid is fitted again, once, so no Thing is under one (gd-SWjiYouIQt, BEACONS.inset).
   The refit keeps the level of detail of the first (zmin): on a phone a plan's beds fill the width, and the band alone would
   drop them to Project art. */
function fitClear(path,fit){
  var v=camVp(),f=fit(v),t=nav.tree,g=CURSOR.gridOf(t,path),things=[];
  if(g)g.kids.forEach(function(id){var r=t.nodes[id].rect;if(r)things.push(WORLD.tiles(r));});
  var b=BEACONS.inset(bcnLayout(WORLD.clampCam({cx:f.cx,cy:f.cy,z:f.z},worldBox)),f,vp,things);
  if(!b)return f;
  ['top','right','bottom','left'].forEach(function(k){v[k]=Math.max(v[k]||0,b[k]);});
  v.zmin=Math.min(f.z,WORLD.levelRange(WORLD.lodFor(f.z))[0]);
  return fit(v);
}
/* cut: true cuts to the framing; "zoom" flies to zoomOn (plotGo's plot figure) at the map's full zoom instead, centred in the HUD
   insets, cutting only under reduced motion (H-D21). */
function frameCursor(how,cut){
  if(!vp.w||!navOpen)return;
  // the signs are placed against the framing, never a camera mid-ease (spec 6.2). signFr is set in the var statement: a
  // statement of its own beside it, the minifier folds into it as a new local (assets.go, newDecls)
  var f=fitClear(nav.path,function(v){return nav.frame(v);}),c=cut==='zoom'&&zoomOn?zoomCam(zoomOn):f,to=signFr=WORLD.clampCam({cx:c.cx,cy:c.cy,z:c.z},worldBox),at=anim?camTo:cam;
  if(!cut&&at&&Math.abs(to.cx-at.cx)<.5&&Math.abs(to.cy-at.cy)<.5&&Math.abs(to.z/at.z-1)<.001)return;
  looked=false;fitAt=null;zoomOn=null;camTo=to;flyTo(to,cut&&cut!=='zoom'?0:240);
}
function zoomCam(p){var v=camVp(),z=WORLD.Z_MAX,l=v.left||0,iw=v.w-l-(v.right||0),ih=v.h-v.top-(v.bottom||0);return {cx:p.x-(l+iw/2-v.w/2)/z,cy:p.y-(v.top+ih/2-v.h/2)/z,z:z};}
function camChanged(){
  cam=WORLD.clampCam(cam,worldBox);
  var l=level;level=WORLD.lodFor(cam.z,level);if(l!==level&&!signTask)signsNext('');   // a sign's words are its level's (gd-0I89c9ifka)
  hotsEl.style.transform=ghotsEl.style.transform='translate('+(vp.w/2-cam.cx*cam.z)+'px,'+(vp.h/2-cam.cy*cam.z)+'px) scale('+cam.z+')';
  hotsEl.style.setProperty('--z',cam.z);ghotsEl.style.setProperty('--z',cam.z);
  syncHots();syncBeacons();placeLabels();scheduleURL();queueRender();STIP.place();if(window.WALL&&WALL.place)WALL.place();
}
function setCam(c,user){if(user)auto=false;cam=c;camChanged();}
function stopAnim(){anim=null;}
/* Fly to a camera. A jump cut under reduced motion; otherwise ease the centre linearly and the zoom exponentially, eased out
   (cubic). A frame moves it at most a quarter of the way, so a long frame after the click (the move's own work, a slow
   machine) still shows the fly rather than cutting to its end (gd-yOOVahJMCh). */
function flyTo(to,ms){
  auto=false;stopAnim();
  to=WORLD.clampCam(to,worldBox);
  if(reduce||!(ms>0)){setCam(to);return;}
  var from=cam,t=Date.now(),run=0,me=anim={to:to};   // to: anchorCam re-aims it
  (function step(){
    if(anim!==me)return;
    var n=Date.now();run+=Math.min(n-t,ms/4);t=n;
    var k=Math.min(1,run/ms),e=1-Math.pow(1-k,3);
    if(k>=1)anim=null;   // cleared before the last setCam so camChanged sees a settled camera
    setCam({cx:from.cx+(to.cx-from.cx)*e,cy:from.cy+(to.cy-from.cy)*e,z:from.z*Math.pow(to.z/from.z,e)});
    if(k<1)raf(step);
  })();
}
/* ---------- pointer and touch (cursor-grid spec revision 3, 4.3): the free camera ----------
   A mouse drag or one finger pans; the wheel, a trackpad pinch (ctrl+wheel) and two fingers zoom about the pointer or the
   fingers' midpoint, and the cursor's depth follows the zoom (follow). A click or tap on a cell moves the cursor there and a
   second on the cursor's cell goes in (on a leaf it opens, as a click always did); a tap on a Thing outside the current grid
   is a cursor jump to it, and neither moves the camera. A double click or tap elsewhere zooms a step about that point.
   ptrs holds every pointer the board has seen go down and not yet come up. A pointer is forgotten only when its pointerup or
   pointercancel reaches the board, and a finger's events follow the element it landed on: when that is a hot-link that
   syncHots() removes while the camera moves under a pinch, a browser may keep delivering the finger's later events to the
   detached node, where they bubble nowhere. The finger then haunts ptrs for the rest of the session, every later two-finger
   gesture counts three pointers and pinch is never set (gd-iYlxddKzuu). Two guards: a primary touch can only begin when no
   other touch is active (Pointer Events), so it starts the set afresh; and the pointers of a drag or a pinch are captured to
   the board, which is never removed, so the hots under them may come and go. */
var ptrs={},drag=null,pinch=null,suppressClick=false,downCur=null,upAt=0,inAt=0,WHEEL_K=.003,PINCH_K=.01;
function local(e){var r=board.getBoundingClientRect();return [e.clientX-r.left,e.clientY-r.top];}
function forgetPointers(){ptrs={};drag=null;pinch=null;}
function capturePointers(){for(var k in ptrs){try{board.setPointerCapture(k*1);}catch(x){}}}
function pdist(){var a=[];for(var k in ptrs)a.push(ptrs[k]);return a.length<2?0:Math.hypot(a[0][0]-a[1][0],a[0][1]-a[1][1]);}
function pmid(){var a=[];for(var k in ptrs)a.push(ptrs[k]);return [(a[0][0]+a[1][0])/2,(a[0][1]+a[1][1])/2];}
/* The viewer's own camera: no fly runs under it, and a resize or a late header keeps it (looked). */
function userCam(c){stopAnim();looked=true;fitAt=null;setCam(c,true);}
board.addEventListener('pointerdown',function(e){
  if(e.target.closest&&e.target.closest('.cam,.curbar,.wl-card'))return;
  if(e.pointerType==='mouse'&&e.button!==0)return;
  if(e.pointerType==='touch'&&e.isPrimary&&Object.keys(ptrs).length)forgetPointers();   // a new touch sequence: whatever is left is a ghost
  ptrs[e.pointerId]=local(e);
  var n=Object.keys(ptrs).length;
  if(n===1){drag={id:e.pointerId,q:ptrs[e.pointerId],x:e.clientX,y:e.clientY,on:false};downCur=nav.node();}   // the cursor before the press focuses a link (which moves it)
  else if(n===2){pinch={d:pdist(),m:pmid()};if(drag)drag.on=true;downCur=null;capturePointers();}   // a pinch's zoom moves the cursor's depth (follow): panBack leaves it
});
board.addEventListener('pointermove',function(e){
  if(!ptrs[e.pointerId])return;
  var p=ptrs[e.pointerId]=local(e);
  if(pinch&&Object.keys(ptrs).length>=2){
    var d1=pdist(),m=pmid(),f=pinch.d>0&&d1>0?d1/pinch.d:1;
    userCam(WORLD.panBy(WORLD.zoomAt(cam,f,pinch.m[0],pinch.m[1],vp,worldBox),m[0]-pinch.m[0],m[1]-pinch.m[1],worldBox));   // the world point under the midpoint stays under it
    pinch.d=d1;pinch.m=m;if(f!==1)follow(f>1?1:-1,m);
    return;
  }
  if(!drag||drag.id!==e.pointerId)return;
  if(!drag.on&&Math.hypot(e.clientX-drag.x,e.clientY-drag.y)>=5){drag.on=true;capturePointers();}   // past a click: a pan
  if(drag.on){userCam(WORLD.panBy(cam,p[0]-drag.q[0],p[1]-drag.q[1],worldBox));drag.q=p;}
});
function pend(e){
  if(!ptrs[e.pointerId])return;
  delete ptrs[e.pointerId];upAt=Date.now();
  if(drag&&drag.on){suppressClick=true;setTimeout(function(){suppressClick=false;},0);panBack();}
  if(Object.keys(ptrs).length<2)pinch=null;
  if(!Object.keys(ptrs).length)drag=null;else if(drag){drag.id=Object.keys(ptrs)[0]*1;drag.q=ptrs[drag.id];drag.on=true;}   // the finger left from a pinch pans on
}
/* A pan moves no cursor: the press focused the link under it, which moved the cursor there (focus is the cursor, spec 3.6), so
   put it back on the cell it was on before the press, and its link back in focus. */
function panBack(){var w=downCur;downCur=null;if(!w||!navOpen||w===nav.node())return;nav.to(w.id);syncHots();scheduleURL();refreshSigns();var c=cellNodes[w.id];if(c)try{c.focus({preventScroll:true});}catch(x){c.focus();}}
board.addEventListener('pointerup',pend);board.addEventListener('pointercancel',pend);var watch=BIRDS.wire(board,{level:function(){return level;},cam:function(){return cam.cx+','+cam.cy+','+cam.z;},local:local,node:function(){return nav.node();},st:function(){return gspots&&gspots[level];},toast:toast});   // a long press or right-click on a bird's plate opens its gardener's Glass, and w (watch) the cursor's (web/birds.js, bird-captions spec 10)
board.addEventListener('click',function(e){if(suppressClick){e.preventDefault();e.stopPropagation();suppressClick=false;}},true);
board.addEventListener('wheel',function(e){
  e.preventDefault();
  var dy=e.deltaY*(e.deltaMode===1?16:e.deltaMode===2?vp.h:1),q=local(e);
  if(!dy||!navOpen)return;   // a sideways wheel is not a zoom
  userCam(WORLD.zoomAt(anim&&camTo?camTo:cam,Math.pow(2,-dy*(e.ctrlKey?PINCH_K:WHEEL_K)),q[0],q[1],vp,worldBox));
  follow(dy<0?1:-1,q);
},{passive:false});
/* A double click or tap that did not go in (the second click on a cell does) zooms a step about its point. */
board.addEventListener('dblclick',function(e){
  if(!navOpen||Date.now()-inAt<600||(e.target.closest&&e.target.closest('.cam,.curbar,.wl-card')))return;
  e.preventDefault();var q=local(e);zoomStep(1,q,Math.min(3,level+1));follow(1,q);
});
/* Depth follows the camera (revision 3): after a zoom in, while the cursor's depth is under the camera's level, it descends into
   the cell under q (off the cells, its own Thing); after a zoom out, while its depth is over the level, it ascends. A leaf is
   not opened, and the camera stays where the zoom put it. */
function follow(dir,q){
  if(!navOpen||!nav.tree||!nav.path.length)return;
  var was=nav.path.join('\n'),lv=WORLD.lodFor((anim&&camTo?camTo:cam).z),c=anim&&camTo?camTo:cam;
  for(var k=0;k<6&&(dir>0?nav.path.length-1<lv:nav.path.length-1>lv);k++){
    if(dir<0){nav.ascend();continue;}
    var w=WORLD.toWorld(c,vp,q[0],q[1]),id=nav.pick(w.x,w.y),r;
    if(id&&nav.path.indexOf(id)<0)nav.to(id);
    r=nav.descend(needIds());if(r.bumped||r.open||r.drawer!==undefined)break;
  }
  if(nav.path.join('\n')!==was)cursorMoved(dir>0?'in':'out',false,null);
}
/* One zoom step about screen point q toward level lv: twice the zoom (half, out), and at least into lv's range; at 1x and over,
   a resting zoom, so the pixel art stays crisp. fit (tiles): the farmstead or zone the step went into, kept whole (fitHome);
   fitAt keeps the nodes fitted, the step's zoom and the camera it gave, so a re-layout that grows them fits them again (29.11). */
function zoomStep(dir,q,lv,fit){
  var c=anim&&camTo?camTo:cam,r=WORLD.levelRange(Math.max(0,Math.min(3,lv))),z=dir>0?Math.max(c.z*2,r[0]):Math.min(c.z/2,r[1]),to;
  if(z>=1)z=WORLD.snapZ(z);
  to=WORLD.zoomAt(c,z/c.z,q[0],q[1],vp,worldBox);if(fit)to=fitHome(to,fit);
  fitAt=fit?{p:nav.path[nav.path.length-2],n:nav.path[nav.path.length-1],z:z,cam:to}:null;
  camTo=signFr=to;looked=true;flyTo(to,240);
}
/* In from a home cell or one of its zones (HW 28.6, 29.6, gd-cTKXpm8JH7): the step's zoom no more than fits the rect r (tiles)
   whole in the board clear of the header, the camera column and the tab bar, a resting zoom at 1x and over, centred there; so a
   360 px phone shows all of the farmstead's zones, or all of a zone's letters or goats, wherever the camera was. Under a finger,
   into the Growing field, it is never less than keeps its stands 44 px (zoneFloor, gd-a628FpJRO5): a crowded field then runs
   off the screen, the cursor's Thing kept on it. */
function fitHome(to,r){
  var v=camVp(),b=WORLD.tiles(r),l=v.left||0,m=8,iw=vp.w-l-v.right-2*m,ih=vp.h-v.top-v.bottom-2*m,z=Math.min(to.z,iw/b.w,ih/b.h),lo=coarse?zoneFloor():0,n=nav.node(),t=n&&n.rect&&WORLD.tiles(n.rect);
  if(!(z>0))return to;
  if(z>=1)z=WORLD.SNAPS.filter(function(s){return s<=z;}).pop();
  if(lo>z)z=WORLD.SNAPS.filter(function(s){return s>=lo;})[0]||WORLD.Z_MAX;
  var cx=b.x+b.w/2,cy=b.y+b.h/2,hw=iw/2/z,hh=ih/2/z;
  if(t){cx=Math.min(Math.max(cx,t.x+t.w-hw),t.x+hw);cy=Math.min(Math.max(cy,t.y+t.h-hh),t.y+hh);}   // the cursor's Thing on the screen
  return WORLD.clampCam({cx:cx-(l-v.right)/2/z,cy:cy-(v.top-v.bottom)/2/z,z:z},worldBox);
}
/* The least zoom at which the biggest Thing in the cursor's Growing field (its stands) is a 44 px target (WORLD.MIN_HOT), or 0
   anywhere else: a Mailbox's letters and a Pen's goats fit their zone at 44 px already. */
function zoneFloor(){var p=nav.tree&&nav.tree.nodes[nav.path[nav.path.length-2]],big=0;if(!p||p.kind!=='zone'||p.zone!=='growing')return 0;(p.kids||[]).forEach(function(id){var k=nav.tree.nodes[id];if(k&&k.rect)big=Math.max(big,Math.min(k.rect.w,k.rect.h)*WORLD.TILE);});return big?WORLD.MIN_HOT/big:0;}
/* The rect of the home cell or zone the cursor has just gone into, with its new Thing (a goat out grazing is outside its pen), for
   fitHome; pid and nid name them instead (fitAt's). */
function homeIn(pid,nid){var t=nav.tree,p=t&&t.nodes[pid||nav.path[nav.path.length-2]],n=pid?t&&t.nodes[nid]:nav.node();return p&&(p.kind==='home'||p.kind==='zone')?n&&n.rect?WORLD.union(p.rect,n.rect):p.rect:null;}
/* Where In and Out zoom (revision 3): about the cursor's Thing when it is on the board; else about the board's middle, and In
   first moves the cursor to the cell there. */
function aim(into){
  var c=anim&&camTo?camTo:cam,n=nav.node(),r=n&&n.rect&&CURSOR.screenRect(n.rect,c,vp),x=r?r.x+r.w/2:-1,y=r?r.y+r.h/2:-1;
  if(x>=0&&y>=0&&x<=vp.w&&y<=vp.h)return [x,y];
  var w=WORLD.toWorld(c,vp,vp.w/2,vp.h/2),id=nav.pick(w.x,w.y);
  if(into&&id&&nav.inGrid(id))nav.to(id);
  return [vp.w/2,vp.h/2];
}
/* A click on a grid cell (cellClick, from makeHot) or on the ground. A click with no press before it (a screen reader's, a
   script's) is on the cursor's cell, so it goes in as Enter does. */
function tapWas(){var w=downCur;downCur=null;return Date.now()-upAt<1000?w:undefined;}
function cellClick(node,e){
  var n=node._n;
  if(!n||!navOpen||e.button||e.metaKey||e.ctrlKey||e.shiftKey||e.altKey)return;
  var was=tapWas(),tip=STIP.tap(node,e);if(was===undefined)was=n;   // tip: a finger's first tap on a staked plot opened its tip (web/stip.js), the next goes in
  if(was===n&&!tip)inAt=Date.now();   // a double click that goes in or opens zooms no step on top
  if(was!==n||tip){e.preventDefault();e.stopImmediatePropagation();if(was!==n)cursorTap(n.id);return;}
  if(n.kids.length){e.preventDefault();e.stopImmediatePropagation();cursorAct({act:'in'});}   // a leaf's click is left to its link
}
board.addEventListener('click',function(e){
  if(e.defaultPrevented||e.button||e.metaKey||e.ctrlKey||e.shiftKey||e.altKey||!navOpen||(e.target!==cv&&e.target!==board&&!lblsEl.contains(e.target)))return;
  var q=local(e);if(e.target===cv&&goats.click(e,q[0],q[1])){tapWas();return;}
  var was=tapWas(),w=WORLD.toWorld(cam,vp,q[0],q[1]),id=nav.pick(w.x,w.y),cur=nav.node();
  if(!id||!cur)return;
  if(id===cur.id){if(was===cur)cursorAct({act:'in'});return;}
  if(nav.path.indexOf(id)<0)cursorTap(id);   // bare ground inside the cursor's own containers is no Thing
});
/* Goats are drawn, not links: a mouse over one names its wish, a click (a finger's second tap) opens its thread in the Commons,
   as a gardener's sprite does, else the wish's bead view (GOATS.wire, gd-2cPnm71cxT); goats.go, the cursor's way to one (gd-xlRpUpTjPZ). */
var goats=GOATS.wire(board,cv,{level:function(){return level;},local:local,tapped:tapWas,world:function(){return world;},to:cursorTo,into:cursorInto,focus:focusCursor,open:function(g){if(typeof CHAT!=='undefined')CHAT.open(g.id);else location.href=base+g.href;}});
/* Where the map's keys apply: the map on the screen, and nothing focused, the map itself, or the open bead view (its title, a
   link, a button: never a field someone is typing in), so opening a task does not take the arrows away (gd-IUV4yofidl). The map
   is on the screen on the farm screen, and on a desktop whatever the screen: its dock sits beside the map, so the Porch or the
   Mailbox open takes no key from it (gd-DT9pEP0YaG); a phone's dock hides the map (.left), and the keys with it. */
var beadEl=document.getElementById('bead'),leftEl=document.querySelector('.left');
function mapShown(){return screen==='farm'||!!(leftEl&&getComputedStyle(leftEl).display!=='none');}
function camKeyTarget(){
  var a=document.activeElement;
  return mapShown()&&(!a||a===document.body||a===document.documentElement||(board.contains(a)&&!jumpEl.contains(a))||(beadEl&&beadEl.contains(a)&&!typing(a)));
}
function typing(a){var t=a?a.tagName:'';return t==='INPUT'||t==='TEXTAREA'||t==='SELECT'||!!(a&&a.isContentEditable);}
function pressable(a){var t=a?a.tagName:'';return t==='A'||t==='BUTTON'||t==='SUMMARY'||typing(a);}
function beadOpen(){return !!(beadEl&&!beadEl.hidden);}
/* The cursor's keys (spec 3.2, CURNAV.action): arrows move, Shift+arrows jump to the sibling grid, Enter/+ go in, Esc/- come out,
   Space opens the Thing, 0 is depth 0, Home a home cell, n/N what needs you. A camera button, a beacon or a link or button in
   the bead view keeps its own Enter and Space, and Esc closes an open bead view, tour or outline before it ascends
   (the bead view closes its own when it holds focus; from the map, this does). */
document.addEventListener('keydown',function(e){
  if(e.ctrlKey||e.metaKey||e.altKey||!camKeyTarget())return;
  var k=e.key,a=CURNAV.action(e),ae=document.activeElement,inView=!!(beadEl&&ae&&beadEl.contains(ae));
  if((k==='s'||k==='S')&&!e.shiftKey&&!snapBtn.hidden){e.preventDefault();snapFarm(SNAP.MAX);return;}
  if(k==='Escape'&&overlayOpen()){if(beadOpen()&&!inView&&tourEl.hidden&&!outEl.classList.contains('open')&&typeof BEAD!=='undefined'){e.preventDefault();BEAD.close();}return;}
  if(!a||((a.act==='in'||a.act==='open')&&ae&&(inView?pressable(ae):ae!==board&&board.contains(ae)&&!hotsEl.contains(ae))))return;
  e.preventDefault();
  if(a.act==='find')openJump();else if(a.act==='watch'||a.act==='transcript')watch(ae,a.act==='transcript');else cursorAct(a);
});
function overlayOpen(){return beadOpen()||!tourEl.hidden||outEl.classList.contains('open');}
/* One cursor action: the path moves (web/curnav.js), then the grid's links, the camera, focus (only if it was in the map, spec
   3.6), the live region and the address follow. A leaf's A opens it, the plans crate's the Plans drawer; at an edge it bumps. */
function cursorAct(a){
  if(!navOpen)return;
  auto=false;figFocus='';var r,q,needs=needIds(),fg=a.act==='in'&&standG(nav.node());
  if(a.act==='in'||a.act==='open')inAt=Date.now();
  if(fg){fieldGlass(fg,cellNodes[nav.path[nav.path.length-1]]);return;}   // a field figure's Enter: its Glass (H-D21)
  if(a.act==='move')r=nav.move(a.dir);else if(a.act==='jump')r=nav.jump(a.dir);else if(a.act==='in'){if(goats.enter(nav.node()))return;q=aim(true);r=nav.descend(needs);}else if(a.act==='out'){q=aim();r=nav.ascend();}
  else if(a.act==='top')r=nav.top();else if(a.act==='home')r=nav.home(a.edge);else if(a.act==='end')r=nav.end();else if(a.act==='need')r=nav.need(needs,a.step);else r=openOf(nav.node());
  if(r.open||r.project||r.drawer!==undefined)openThing(r);else cursorMoved(r.bumped?'bump':a.act,false,q);
}
/* how the cursor moved; q: a screen point In or Out zooms a step about (zoomStep), or null where the camera stays (a move, a
   jump, a tap, depth following a zoom); otherwise the camera frames the cursor. */
function cursorMoved(how,cut,q){
  var inMap=board.contains(document.activeElement)&&document.activeElement!==board;
  if(how==='bump'){var b=cellNodes[nav.path[nav.path.length-1]];if(b&&!reduce){b.classList.add('bump');setTimeout(function(){b.classList.remove('bump');},140);}return;}
  if(nav.tree)syncBar();   // before the framing measures the foot (footInset counts the bar's strip); the signs wait for signsNext
  syncHots();
  if(q)zoomStep(how==='in'?1:-1,q,nav.path.length-1,how==='in'&&homeIn());else if(q===null||how==='move'||how==='jump')signFr=WORLD.clampCam(anim&&camTo?camTo:cam,worldBox);else frameCursor(how,cut);   // the signs are placed against the camera at rest
  if(inMap)focusCursor();
  viewFollow();
  var s=nav.say(how);   // said once, by signsNext's task
  try{localStorage.setItem(CURNAV.STORE,nav.save());}catch(e){}
  scheduleURL();signsNext(s||'');
}
/* The open bead view follows the cursor (gd-IUV4yofidl): on a Thing with a bead (a task, a phase, a plan, a strip's item) it shows
   that one in place, with no history entry, so arrowing along a bed reads each step without closing and reopening the view.
   The cursor's link becomes what its close hands focus back to, so closing it does not snap the cursor back to the first bead. */
function viewFollow(){
  if(typeof BEAD==='undefined'||!BEAD.current()||!nav.node())return;
  var r=openOf(nav.node());
  if(r.open&&!/^(home|release):/.test(r.open)&&r.open!==BEAD.current())BEAD.open(r.open,{replace:true,opener:cellNodes[nav.path[nav.path.length-1]]||board});
}
/* A cursor jump to any Thing (the jump palette, the outline, a beacon, the bead view, the Porch); keyed hands focus to its link. */
function cursorTo(id,keyed){if(!navOpen||!nav.to(id).found)return false;auto=false;cursorMoved('to');if(keyed)focusCursor();return true;}
/* A click or tap on a Thing: the cursor moves to it in the current grid, or jumps to it anywhere else on the map (spec 7). */
function cursorTap(id){var near=nav.inGrid(id);if(!navOpen||!nav.to(id).found)return;auto=false;cursorMoved(near?'move':'to',false,null);}
/* ... and into it: a click on a container's link (a project, a zone, a band, a bed). */
function cursorInto(id){if(!navOpen||!nav.to(id).found)return false;auto=false;nav.descend(needIds());cursorMoved('in');return true;}
function focusCursor(){var n=cellNodes[nav.path[nav.path.length-1]];if(n){outPin(false);try{n.focus({preventScroll:true});}catch(e){n.focus();}}}
/* Beacon order (spec section 8): what a descend lands on first, and what n and N walk. */
function needIds(){return [].concat.apply([],bcnTg.map(function(t){return t.kind==='escalation'?t.asks:t.id;}));}   // an Escalations arrow: each of its escalations (28.13)
function openOf(n){return !n?{bumped:true}:n.kind==='biome'?(n.key&&bench('project')?{project:n.key}:{drawer:n.key}):n.kind==='zone'?{open:n.id}:n.open||n.drawer!==undefined?{open:n.open,drawer:n.drawer}:{bumped:true};}
/* A Workbench panel (web/workbench.js): one it shows, or any once it has loaded (web/lazy.js loads it after the World). */
function bench(kind){return typeof WORKBENCH!=='undefined'?WORKBENCH.has(kind):typeof LAZY!=='undefined';}
function benchOpen(kind,id,how){if(typeof WORKBENCH!=='undefined')WORKBENCH.open(kind,id,how);else LAZY.need('workbench').then(function(){WORKBENCH.open(kind,id,how);},function(){toast('That view did not load. Try again.');});}
function openThing(r){if(r.project)benchOpen('project',r.project);else if(r.drawer!==undefined)openPlansDrawer();else if(/^release:/.test(r.open)){var tn=nav.tree&&nav.tree.nodes[r.open];if(tn&&tn.ref.rel.id)benchOpen('release',tn.ref.rel.id);}else if(/^home:.*\/(mailbox|barn)$/.test(r.open))paneTo(/mailbox$/.test(r.open)?'mail':'porch',r.open);else if(/^home:.*\/escalation$/.test(r.open)){paneTo('mail',r.open);mailPane.dataset.only='escalation';}else if(!/^(home|release):/.test(r.open)&&typeof BEAD!=='undefined')BEAD.open(r.open);}   // a zone: its pane at its project; a truck: its changelog
/* The Porch or the Mailbox, whole-farm, scrolled to the project of the zone id (home:<key>/<zone>): its first row whose project
   (a gardener's home biome, an ask's project) is that strip's (28.6, H-D15). */
function paneTo(k,zid){
  goPane(k);
  var key=zid.slice(5,zid.lastIndexOf('/')),bk=function(pj){var b=world&&world.index['project:'+WORLD.projectKey(pj)];return b?b.key:'';},to=null;
  Array.prototype.some.call(D[k].children,function(li){var g=li.dataset.gid&&gspots&&gspots.byId[li.dataset.gid],h=g&&world&&world.index[WORLD.homeFor(g)];return (g?(h?h.key:''):bk(li.dataset.pj))===key&&(to=li);});
  if(to&&to.scrollIntoView)to.scrollIntoView({block:'nearest'});
}
document.getElementById('cam-in').onclick=function(){cursorAct({act:'in'});};
document.getElementById('cam-out').onclick=function(){cursorAct({act:'out'});};
document.getElementById('cam-home').onclick=function(){cursorAct({act:'home'});};
document.getElementById('cam-fit').onclick=function(){cursorAct({act:'top'});};
document.getElementById('cam-go').onclick=function(){openJump();};
/* The Menu (3b R8): a disclosure holding Home, All, Plans, Outline, Tend, Costs (web/costs.js), Snap, Postcard, Sky, Settings (web/settings.js), Legend & keys and Old viewer. Its button opens and
   closes the list; a choice, Esc, or a press or focus anywhere else closes it. Esc and a choice that leaves focus in the list hand focus back to the button.
   Up and Down move between the items while it is open, so they do not move the cursor under it. */
var menuEl=document.getElementById('menu'),menuBtn=document.getElementById('cam-menu'),menuList=document.getElementById('menu-list');
function menuItems(){return Array.prototype.filter.call(menuList.querySelectorAll('.mi'),function(b){return !b.hidden;});}
function menuShow(open,back){
  if(open===!menuList.hidden)return;
  var had=menuList.contains(document.activeElement);
  menuList.hidden=!open;menuBtn.setAttribute('aria-expanded',open?'true':'false');menuEl.parentNode.classList.toggle('menu-open',open);
  if(!open&&(back||had)){try{menuBtn.focus({preventScroll:true});}catch(e){menuBtn.focus();}}
}
menuBtn.onclick=function(){menuShow(menuList.hidden);};
menuList.addEventListener('click',function(e){if(e.target.closest&&e.target.closest('.mi'))menuShow(false);});   // after the item's own handler
menuEl.addEventListener('keydown',function(e){
  if(menuList.hidden||e.ctrlKey||e.metaKey||e.altKey)return;
  var k=e.key,items=menuItems(),i=items.indexOf(document.activeElement);
  if(k==='Escape')menuShow(false,true);
  else if(k==='ArrowDown'||k==='ArrowUp'){var to=items[k==='ArrowDown'?(i+1)%items.length:(i<=0?items.length:i)-1];if(to)to.focus();}
  else if(k!=='ArrowLeft'&&k!=='ArrowRight')return;
  e.preventDefault();e.stopPropagation();
});
document.addEventListener('pointerdown',function(e){if(!menuList.hidden&&!menuEl.contains(e.target))menuShow(false);},true);
document.addEventListener('focusin',function(e){if(!menuList.hidden&&!menuEl.contains(e.target))menuShow(false);});
/* The drawers (3b R9): the Menu's Plans and Legend & keys items open their panel, one at a time, with focus on its header; a
   second choice, the header or Esc closes it, and focus that was in it goes back to the Menu's button. */
var drawers=[['dw-plans','cam-plans'],['dw-help','cam-help']].map(function(p){
  var d=document.getElementById(p[0]),b=document.getElementById(p[1]);
  b.onclick=function(){drawerShow(d,!d.open);};
  d.addEventListener('toggle',function(){
    b.setAttribute('aria-expanded',d.open?'true':'false');
    var a=document.activeElement;
    if(!d.open&&(!a||a===document.body||d.contains(a)))try{menuBtn.focus({preventScroll:true});}catch(e){menuBtn.focus();}
  });
  return d;
});
function drawerShow(d,open){
  drawers.forEach(function(o){if(o!==d&&o.open)o.open=false;});
  d.open=open;
  if(open){var s=d.querySelector('summary');try{s.focus({preventScroll:true});}catch(e){s.focus();}}
}
/* The board's foot a fit keeps clear of (spec 4.1): from the top of the tab bar while it is a row (held sideways it is a rail
   beside the board, gd-p0DsdxVxJU), or of the title bar standing on it (a phone's strip along the foot), whichever is higher,
   down to the board's bottom; 0 on a desktop. */
var barEl=document.getElementById('cursor-bar');   // looked up here: footInset reads it before syncBar's block
function footInset(){
  var o=board.getBoundingClientRect(),top=o.bottom,tb=document.getElementById('tabbar'),r=tb&&tb.getBoundingClientRect(),b=!barEl||barEl.hidden?null:barEl.getBoundingClientRect();
  if(r&&r.height&&r.width>r.height)top=Math.min(top,r.top);
  if(b&&b.height&&b.top>o.top+o.height/2)top=Math.min(top,b.top);
  return Math.max(0,Math.round(o.bottom-top));
}
/* The id of the plot the cursor is inside whose arms the scene draws at every level (CURSOR.gatePlot), or ''. */
function gatePlotId(){var p=navOpen&&nav.tree&&CURSOR.gatePlot(nav.tree,nav.path);return p?p.id:'';}
/* The plot node the cursor is on or inside, or null. */
function cursorPlot(){if(!navOpen||!nav.tree)return null;for(var i=nav.path.length-1;i>=0;i--){var n=nav.tree.nodes[nav.path[i]];if(n&&n.kind==='plot')return n;}return null;}
/* Snap (gd-GbF2JS04h5, snap.js): the farm canvas as a PNG posted into the Commons, with what the view frames. Signed-in viewers
   only; frame() is the farm as the press finds it. */
var snapBtn=document.getElementById('cam-snap'),snapFarm=SNAP.wire(snapBtn,!!me,function(){
  return {cv:cv,vp:vp,cam:cam,level:level,world:world,model:model,hots:stripHots.concat(biomeHots).filter(hotWanted),path:base+'/garden',who:scope,night:night};
},toast,document.getElementById('cam-snapsw'));
/* Postcard (gd-WxRuyfdUtR, postcard.js): the plot at the cursor, cropped from the canvas, posted into the Commons at its plan's
   thread with a caption; on no plan, the cursor's project's farmstead and first ring (29.6). Signed-in viewers only; frame() is
   the cursor's plot or project as the press finds it. */
var cardBtn=document.getElementById('cam-postcard'),postcard=POSTCARD.wire(cardBtn,!!me,function(){
  var pn=cursorPlot(),bn=!pn&&navOpen&&nav.tree&&nav.tree.nodes[nav.path[0]],b=bn&&bn.kind==='biome'?bn.ref:null,r=pn?pn.rect:b?POSTCARD.farm(b):null;
  return {cv:cv,vp:vp,plot:pn?pn.ref:null,biome:b,rect:r?CURSOR.screenRect(r,cam,vp):null,at:pn?WORLD.atFor(world,'plot',pn.id):b?WORLD.atFor(world,'biome',b.id):'',level:level,base:base,path:base+'/garden',who:scope,night:night};
},toast);
board.addEventListener('dragstart',function(e){e.preventDefault();});   // a link under the pointer must not start a native drag
if(window.ResizeObserver)new ResizeObserver(resize).observe(board);else window.addEventListener('resize',resize);
resize();
/* A face that loads after the signs were laid out changes what they measure (gd-h8KQFc8UsX). */
if(document.fonts&&document.fonts.addEventListener)document.fonts.addEventListener('loadingdone',signsSoon);
/* The header floats over the map; its measured height is --bar, which the overlays and drawers position themselves under, so
   the beacons measure what they keep off again (the Plans drawer moves with it) and the cursor is framed under it (gd-1SFmkrSqId),
   unless GARDEN.look set the camera (looked): the purse loading later must not throw it back to the framing (gd-e83LE907oK). */
var hdr=document.querySelector('header.crumb');
if(hdr&&window.ResizeObserver)new ResizeObserver(function(){document.documentElement.style.setProperty('--bar',hdr.offsetHeight+'px');bcnAt=null;syncBeacons();if(!looked)frameCursor('',true);refreshSigns();STIP.place();}).observe(hdr);
/* The Porch and Mailbox buttons sit left of the camera's row, at --camw, its measured width; the arrows then measure again what
   they keep off, else one sat under the buttons, its focus ring hidden (gd-MY6m3a1n2N). */
var camRow=document.querySelector('.cam');
if(camRow&&window.ResizeObserver)new ResizeObserver(function(){document.documentElement.style.setProperty('--camw',camRow.offsetWidth+'px');bcnAt=null;syncBeacons();}).observe(camRow);

function el(tagName,cls,text){var e=document.createElement(tagName);if(cls)e.className=cls;if(text!==undefined)e.textContent=text;return e;}
function plural(n,one,many){return n+' '+(n===1?one:many);}
function cap(s){return s.charAt(0).toUpperCase()+s.slice(1);}
/* ---------- signs (spec section 6): the cursor's Thing has no label, its children short labels, the rest glyphs ----------
   The cursor's lineage, title and status line are the title bar's (syncBar, gd-zDdiA3HBh5). The set is CURSOR.signs for the
   cursor path at its framing (signFr, the camera at rest when the cursor last moved), so it is a function of the path and the
   viewport only: refreshSigns runs on a cursor move, a data change under the cursor and a settled resize, never on a pointer
   move or a camera frame (the flicker probe in testdata/cursor_browser.js). Each label keeps its screen offset from a
   reference point of its Thing (the side it sits on), so during an ease, a pan or a zoom it rides the world with its text fixed
   (placeLabels). Glyphs (a need mark "!N", a crate's "+N") are painted on the canvas (drawGlyphs), no DOM text; bed ordinals
   and pips are the stakes art.js paints. */
var signFr=null,signEls={},glyphs=[],resizeTimer=0,FADE_MS=120,signView=null;
function signRef(s){var a=s.anchor,sl=s.slot;return sl==='above'?{x:a.x+a.w/2,y:a.y}:sl==='below'?{x:a.x+a.w/2,y:a.y+a.h}:sl==='right'?{x:a.x+a.w,y:a.y+a.h/2}:sl==='left'?{x:a.x,y:a.y+a.h/2}:{x:a.x,y:a.y};}
/* The map the labels may use (spec 18 of HW, "text inside the window"): the board under the floating header and the camera row,
   above a phone's tab bar (a row; held sideways it is a rail beside the board, gd-p0DsdxVxJU). Measured per refresh, which is per cursor move, never per frame. */
function signVp(){
  var v=camVp(),o=board.getBoundingClientRect(),tb=document.getElementById('tabbar'),r=tb?tb.getBoundingClientRect():null;
  v.top=Math.max(v.top,hdrBottom());if(r&&r.height&&r.width>r.height)v.bottom=Math.max(v.bottom||0,o.bottom-r.top+4);
  return v;
}
function signsSoon(){clearTimeout(resizeTimer);resizeTimer=setTimeout(refreshSigns,FADE_MS);}
/* ---------- the steady title bar (gd-yozHl8UqIs, gd-zDdiA3HBh5): #cursor-bar reads Plan › Phase › Task (nav.bar), each a link
   whose press moves the cursor there, the cursor Thing's whole title, then its status line of cursor-grid spec 6.4 in full: the
   need words first, a harvest cell's badges as icons, and the words the page adds (a busy project, open wishes, a plot's goats
   past the cap, a release's). A press on the title or the status opens the Thing, as Space does. On a phone it is a strip on the
   tab bar, two rows at most. Refilled with the signs, never per frame. */
var barKey='';
var BAR_CAP=80;   // a lineage part's characters, past which it ends in "…" (its tooltip keeps it whole), so a long plan name leaves the cursor Thing's own title room on a phone (gd-AmIpCiVTvY)
function syncBar(){
  var b=nav.bar(world&&world.index,escLin),n=nav.node(),ex=n?TRUCK.line(n,level,0):'',key;
  if(n&&!b.tip)b.status=GOATS.sign(n,BIRDS.sign(n,b.status,level),level);   // web/birds.js, web/goats.js
  if(ex)b.status=b.status?b.status+' · '+ex:ex;   // a project's releases, a plot's "shipped in" (web/truck.js)
  key=JSON.stringify(b);
  if(key===barKey)return;barKey=key;barEl.textContent='';barEl.hidden=!b.parts.length;
  b.parts.forEach(function(p,i){
    var me=i===b.parts.length-1,cs=Array.from(p.name);
    if(i)barEl.appendChild(el('span','cb-sep','›'));
    var a=barEl.appendChild(el('a',me?'cb-p cb-me':'cb-p',me?p.title:cs.length>BAR_CAP?cs.slice(0,BAR_CAP-1).join('')+'…':p.name));a.href=location.pathname+WORLD.linkQuery(location.search,p.at,null);a.title=p.title;a.dataset.hot=p.id;
    if(me)a.setAttribute('aria-current','location');
  });
  var st=b.status||b.state,sp;
  if(st||b.icons){
    sp=barEl.appendChild(el('span',b.tip?'cb-st cb-tip':'cb-st'));sp.appendChild(el('span','cb-sw',st));
    if(b.icons)b.icons.forEach(function(k){var p=sp.appendChild(el('span','ic '+k.k));p.appendChild(ART.badgeIcon(k.k));p.appendChild(el('span','w',' '+k.w));});   // a harvest cell's badges, each a 12 px icon and its word (harvest spec 5); the space parts the words for a reader
  }
  bcnAt=null;syncBeacons();   // the arrows keep off the bar (web/beacons.js KEEP_OFF): measure it again
}
barEl.addEventListener('click',function(e){
  if(e.metaKey||e.ctrlKey||e.shiftKey)return;
  var a=e.target.closest('a[data-hot]');e.preventDefault();
  if(a&&!a.hasAttribute('aria-current'))cursorTo(a.dataset.hot,true);else cursorAct({act:'open'});   // the lineage moves the cursor; the rest opens its Thing (spec 7 Start)
});
/* A cursor move's signs, in a task of their own (gd-fsuxCGGbZr): they are placed against the framing, not the eased camera, so the
   input task keeps only the move and the camera, and a held arrow's moves before it runs refresh once. said is what the move said,
   which a project board's words follow. */
var signTask=0,signSaid='';function signsNext(said){signSaid=said;if(!signTask)signTask=setTimeout(function(){signTask=0;saidNow=signSaid;refreshSigns();if(saidNow&&sayEl)sayEl.textContent=saidNow;saidNow='';},0);}
function refreshSigns(){
  if(!navOpen||!nav.tree||!vp.w)return;
  syncBar();
  var fr=signFr||cam,sv=signView=signVp(),hud=hudRects();
  var list=CURSOR.signs(nav.tree,nav.path,sv,{cx:fr.cx,cy:fr.cy,z:fr.z}),keep={};
  glyphs=[];
  list.forEach(function(s){
    if(s.tier==='glyph'){var n=nav.tree.nodes[s.id];if(s.glyph&&n&&n.kind!=='bed'){s.plot=n.kind==='plot';glyphs.push(s);}return;}
    var key=s.tier+':'+s.id,e=signEls[key],txt=s.text,r=signRef(s);keep[key]=1;
    if(!e||e._t!==txt){
      if(e)e.remove();
      e=signEls[key]=el('span','lbl sign '+s.tier,txt);e._t=txt;e.dataset.id=s.id;   // a child's short name
      lblsEl.appendChild(e);
    }
    anchorSign(e,s,r,fr);
  });
  worldPlates(list,fr,keep,sv,hud);
  Object.keys(signEls).forEach(function(k){if(!keep[k]){signEls[k].remove();delete signEls[k];}});
  placeLabels();queueRender();
}
/* A label's slot class and its screen offset from its Thing's reference point r at the framing fr. */
function anchorSign(e,s,r,fr){
  if(e._slot!==s.slot){if(e._slot)e.classList.remove('s-'+e._slot);e._slot=s.slot;e.classList.add('s-'+s.slot);}
  e._r=r;e._dx=s.box.x-((r.x-fr.cx)*fr.z+vp.w/2);
  e._dy=s.box.y-((r.y-fr.cy)*fr.z+vp.h/2);e._at='';
}
/* ---------- the World depth's project plates (4b.2, gd-zceqmx40pD) ----------
   Whenever the cursor is at the World depth, at any zoom, the nearest HEADS other projects in view get a one-line header plate,
   flush with its project or its name alone, else dropped (CURSOR.worldSigns). The cursor's project has none: its words are the
   title bar's. No board lists its plans over the map (gd-OOt8Mx2Mps): a tap or In goes into the project, where they are drawn.
   Fully zoomed out, its link and the live region still read them (WORLD.boardWords: those that need you first, then the moving
   ones). The plates are in #lbls (aria-hidden), so the projects' links carry the same words. */
var HEADS=12,SAY_ROWS=8,boardSaid='',saidNow='',described=[],heads={w:null,g:null,by:{},els:{}};
function worldPlates(list,fr,keep,sv,hud){
  var t=nav.tree,n=nav.node(),want={};
  var grow=WORLD.lodFor(fr.z,level)===0,atWorld=nav.path.length<=1;   // the plan words only fully zoomed out; the headers at the World depth (the cursor in the root grid) at any zoom
  if(!grow&&!atWorld){describeAll(want);boardSaid='';return;}
  var v=CURSOR.insets(sv),gp=gspots?gspots[3].plots:null,scr=function(r){return CURSOR.screenRect(r,fr,vp);},said='';
  if(heads.w!==world||heads.g!==gspots)heads={w:world,g:gspots,by:{},els:heads.els};   // the headers' counts, per layout and gardeners; their elements, kept while off the map
  /* the nearest HEADS other projects in view, by the cursor's project (else the map's middle), each with its plate */
  var kids=t.root.kids.filter(function(id){return t.nodes[id].rect;}).map(function(id){return {id:id,r:scr(t.nodes[id].rect)};});
  var c0=n&&n.kind==='biome'&&n.rect?scr(n.rect):{x:v.x+v.w/2,y:v.y+v.h/2,w:0,h:0},dist=function(kr){return Math.hypot(kr.r.x+kr.r.w/2-c0.x-c0.w/2,kr.r.y+kr.r.h/2-c0.y-c0.h/2);};
  var headed=kids.filter(function(kr){var k=t.nodes[kr.id];return k.kind==='biome'&&k.ref&&!(n&&kr.id===n.id)&&CURSOR.overlap(kr.r,v);}).sort(function(a,b){return dist(a)-dist(b);}).slice(0,HEADS),on={};
  headed.forEach(function(kr){on[kr.id]=1;});
  Object.keys(heads.els).forEach(function(id){if(!on[id]){if(heads.els[id].parentNode)heads.els[id].remove();delete signEls['head:'+id];}});
  var fresh=[];
  headed.forEach(function(kr){
    var id=kr.id,k=t.nodes[id];
    var hb=heads.by[id]||(heads.by[id]=WORLD.board(k.ref,gp,0)),e=heads.els[id],txt=k.title+'|'+hb.head;
    if(!e||e._t!==txt){
      if(e)e.remove();
      e=heads.els[id]=el('span','lbl sign head');e._t=txt;e.dataset.id=id;
      e.appendChild(el('span','ttl',CURSOR.shortName(k.title,CURSOR.LBL.prefix)));e._st=e.appendChild(el('span','st',' · '+hb.head));lblsEl.appendChild(e);fresh.push(e);
    }
    kr.e=e;kr.hb=hb;
  });
  fresh.forEach(function(e){e._w=e.offsetWidth;e._h=e.offsetHeight;e._wt=e._w-(e._st.offsetWidth||0);e._st=null;});   // measured once per text, whole and its name alone, after every new plate is in: one layout, not one per plate
  headed.forEach(function(kr){var e=kr.e;kr.w=e._w;kr.wt=e._wt;kr.h=e._h;});
  /* the plates may use the map's whole width (on a phone the camera's column on the right holds buttons only at its foot, which are in the hud) */
  var keepOff=kids.map(function(kr){var r=kr.r,g=CURSOR.RING;return n&&kr.id===n.id?{id:kr.id,r:{x:r.x-g,y:r.y-g,w:r.w+2*g,h:r.h+2*g}}:kr;});   // the cursor's project with its ring: a plate flush with its edge sat under it (gd-dS4KEs0ohA)
  var plates=CURSOR.worldSigns(headed,keepOff,{x:v.x,y:v.y,w:vp.w-4-v.x,h:v.h},list.filter(function(s){return s.box;}).map(function(s){return s.box;}).concat(hud));
  if(n&&n.kind==='biome'&&n.ref){
    if(grow){said=WORLD.boardWords(WORLD.board(n.ref,gp,SAY_ROWS));want[n.id]=said;}   // fully zoomed out its plans are not drawn: its link and the live region read them
    else{var hm=heads.by[n.id]||(heads.by[n.id]=WORLD.board(n.ref,gp,0));want[n.id]=n.title+': '+hm.head+'.'+WORLD.onWords(hm.on);}   // the cursor's project, as a header
  }
  headed.forEach(function(kr){
    var id=kr.id,k=t.nodes[id],key='head:'+id,e=kr.e,b=plates[id];
    if(!b){if(e.parentNode)e.remove();delete signEls[key];return;}
    if(!e.parentNode)lblsEl.appendChild(e);
    e.classList.toggle('brief',b.brief);e._hw=b.w;e._hh=b.h;
    signEls[key]=e;keep[key]=1;
    var hs={tier:'head',anchor:WORLD.tiles(k.rect),slot:'head',box:b};anchorSign(e,hs,signRef(hs),fr);
    want[id]=k.title+': '+kr.hb.head+'.'+WORLD.onWords(kr.hb.on);
  });
  describeAll(want);
  if(said&&said!==boardSaid){if(saidNow)saidNow+=' '+said;else if(sayEl)sayEl.textContent=said;}
  boardSaid=said;
}
/* The controls floating over the map that the signs' insets leave in it (the camera buttons, the Porch and Mailbox buttons
   beside them, the beacons), in board px: a header plate keeps off them. */
function hudRects(){return SIGNFIT.keep('.cam .cb,.dbtns .dbtn,#bcns>*,#cursor-bar');}
/* The projects' links carry their words (id -> words): set where they changed, removed where they went. */
function describeAll(want){
  described.forEach(function(nd){if(!want[nd._did]||cellNodes[nd._did]!==nd){nd.removeAttribute('aria-description');nd._desc='';}});
  described=[];
  for(var id in want){var nd=cellNodes[id];if(!nd)continue;if(nd._desc!==want[id]){nd.setAttribute('aria-description',want[id]);nd._desc=want[id];}nd._did=id;described.push(nd);}
}
/* The labels at this camera: each at its Thing's reference point plus its offset (a style write only when it moved). */
function placeLabels(){
  for(var k in signEls){
    var e=signEls[k],a=WORLD.toScreen(cam,vp,e._r.x,e._r.y),x=Math.round(a.x+e._dx),y=Math.round(a.y+e._dy),t='translate('+x+'px,'+y+'px)';
    if(e._at!==t){e._at=t;e.style.transform=t;}
    if(e._hw){var cut=x<0||y<0||x+e._hw>vp.w||y+e._hh>vp.h;if(e._cut!==cut){e._cut=cut;e.style.visibility=cut?'hidden':'';}}   // a header plate the camera carried off the board's edge waits for the next layout, never cut (gd-zceqmx40pD)
  }
}
/* The glyph tier, painted in screen px at the top-left of each cell and kept inside it (CURSOR.fitGlyph): a gold "!N" where a
   human is wanted, a light "+N" on a crate, in a 3x5 pixel font at 2x (no canvas text, so a golden does not hang on the system's
   fonts). */
var GLYPH_PX={'0':'kkkk.kk.kk.kkkk','1':'.k.kk..k..k.kkk','2':'kk...k.k.k..kkk','3':'kk...k.k...kkk.','4':'k.kk.kkkk..k..k','5':'kkkk..kk...kkk.','6':'.kkk..kk.k.k.k.','7':'kkk..k.k..k..k.','8':'.k.k.k.k.k.k.k.','9':'.k.k.k.kk..kkk.','!':'.k..k..k.....k.','+':'....k.kkk.k....'};
function drawGlyphs(){
  if(!glyphs.length)return;
  var d=vp.dpr;ctx.setTransform(d,0,0,d,0,0);
  glyphs.forEach(function(s){
    if(level<=1&&s.plot)return;   // a plot's "!N" is its rollup stamp's need mark (cursor-grid spec 6.6)
    var a=WORLD.toScreen(cam,vp,s.anchor.x,s.anchor.y),e=WORLD.toScreen(cam,vp,s.anchor.x+s.anchor.w,0).x,f=CURSOR.fitGlyph(s.glyph,Math.round(a.x),Math.round(e)),g=f.g,w=CURSOR.glyphW(g),x=f.x,y=Math.round(a.y)+2;
    if(x>vp.w||y>vp.h||x+w<0||y+14<0)return;
    ctx.fillStyle='#2a1a14';ctx.fillRect(x,y,w,14);ctx.fillStyle=g.charAt(0)==='!'?'#ffe680':'#fbf6e4';
    for(var i=0;i<g.length;i++){var m=GLYPH_PX[g.charAt(i)]||'';for(var j=0;j<15;j++)if(m.charAt(j)==='k')ctx.fillRect(x+3+i*8+(j%3)*2,y+2+Math.floor(j/3)*2,2,2);}
  });
  ctx.setTransform(1,0,0,1,0,0);
}
/* ---------- hot links: the cursor's grid (spec 3.6) ----------
   #hots is a role=grid of the current grid's occupied cells: a role=row per used row, each cell a role=gridcell holding the
   Thing's link or button. Exactly one, the cursor's, is in the tab order (roving tabindex) and carries .cur, the focus ring.
   The gardeners on their plots are links in #ghots beside it (not grid cells, spec section 8), culled to the view. Both are
   child-diffs, so a focused link survives live updates, positioned in world px under the one transform camChanged() moves. */
function hotWanted(h){return WORLD.wantHot(h,level,cam.z,coarse);}   // levels, then the 44 px rule for coarse pointers (section 8)
var LINK_KINDS={bead:1,task:1,plot:1,tree:1,flower:1,seed:1,stone:1,gardener:1,gesc:1,steward:1,crate:1,escalation:1,truck:1};
function makeHot(h){
  var node=el(LINK_KINDS[h.kind]?'a':'button','hot'+(h.kind==='bead'?'':' w-'+h.kind));
  if(!LINK_KINDS[h.kind])node.type='button';
  node.addEventListener('click',function(e){cellClick(node,e);});   // first, so a first click that only moves the cursor stops the rest
  node.addEventListener('focus',function(){if(node._n&&navOpen&&node._n!==nav.node()){nav.to(node._n.id);syncHots();scheduleURL();refreshSigns();}});   // the focused link is the cursor (spec 3.6)
  node.addEventListener('click',function(e){var g=h.kind==='gardener'||h.kind==='gesc'?h.g:node._it&&node._it.g;if(!g||e.defaultPrevented||e.button||e.metaKey||e.ctrlKey||e.shiftKey||e.altKey)return;e.preventDefault();if(node._it&&node._it.kind==='stand'){fieldGlass(g,node);return;}if(h.kind==='gardener'){fieldGo(g);return;}var porch=function(){goPane('porch');if(!PORCH.show(g.id))toast((g.agent||g.id)+' is not on your Porch. Switch to Everyone to see it.');};if(!g.chat||typeof CHAT==='undefined'){porch();return;}CHAT.open(g.id,{none:porch});});   // one key, three stops (H-D21, homestead-agents-as-people.md 12.3): a field figure opens its Glass, a plot figure goes home to its field figure; an escalation's second stand opens its thread in the Commons, else its Porch row. The thread stays one click away, on the card and the Porch row
  if(h.kind==='steward'||h.kind==='gesc'||h.kind==='seed'||h.kind==='stone'||h.kind==='plot'||h.kind==='escalation'||h.kind==='truck')STIP.wire(node);   // a Nursery pot's tip carries its plan decision's card (9.11b), as a staked plot's does (a planted plot wears none); a headstone's, how its wish ended and what its kin merged; an escalation's, its words and, when the viewer may approve it, its answer box (28.11)
  if(h.kind==='crate')node.addEventListener('click',function(e){var n=node._n;if(e.defaultPrevented||!n||!/^home:.*\/escalation$/.test(n.open||'')||e.button||e.metaKey||e.ctrlKey||e.shiftKey||e.altKey)return;e.preventDefault();openThing({open:n.open});});   // the Escalations' "+N": the Mail pane on them all, as the cell opens it
  if(h.kind==='overflow')node.onclick=openPlansDrawer;
  else if(h.kind==='gmore')node.onclick=function(){goPane('porch');};else if(h.kind==='gask')node.onclick=function(){REASON.open(REASON.letter(h.g));};   // the Porch, grouped by plot, names every gardener on it; a speech bubble's letter in the Mailbox (web/reason.js)
  return node;
}
STIP.init(function(){return {cam:cam,vp:vp,view:signView,bcns:bcnNow,base:base,ago:ago,lin:escLin,gs:gspots,world:world,go:goats.go};});   // the steward's tip beside its sprite (web/stip.js, Lands 9.8); go: a goat card's "Zoom to it on the map" (gd-xlRpUpTjPZ)
/* The plots past a biome's cap are listed in the Plans drawer (the crate used to link the viewer's Plans page). */
function openPlansDrawer(){drawerShow(drawers[0],true);}
/* Point a hot node at the current model: label, link and rect. Cheap, so it runs for every visible node on each model refresh. */
function fillHot(node,h){
  var it=h.kind==='bead'&&h.id?model.items[h.id]:null;   // a plot task can share an id with a strip's plant; only the strips' hots read the model
  node._h=h;node._it=it;
  if(h.kind==='bead'){node.href=it.href;node.setAttribute('aria-label',it.aria||it.title+' — '+WORLD.itemWords(h.zone,it)[0]+(it.armsName?'. '+ARMS.sentence(it.armsName):''));}
  else fillWorldHot(node,h);
  node.style.left=h.x+'px';node.style.top=h.y+'px';node.style.width=h.w+'px';node.style.height=h.h+'px';
}
/* hotsGen counts data generations (a model refresh or a new world): a node filled in the current one from the same hot object
   is up to date, so a camera move refills nothing. gridKey is the grid and its cells; cellNodes maps a tree id to its link. */
var hotsGen=0,gridKey='',cellNodes={},rowEls=[],hotIx={};
function syncHots(force){
  if(!model||!nav.tree)return;
  if(force)hotsGen++;
  var key=nav.grid().id+'|'+nav.tree.gen,id,cur=nav.node();   // a camera frame allocates nothing here: the grid is rebuilt per grid and tree
  if(key!==gridKey||force){
    var all=nav.cells(),byRow=[],keep={},ref;gridKey=key;
    all.cells.forEach(function(c){(byRow[c.row]=byRow[c.row]||[]).push(c);keep[c.node.id]=1;});
    for(id in cellNodes)if(!keep[id]){cellNodes[id].parentNode.remove();delete cellNodes[id];}
    rowEls.forEach(function(r,i){if(!byRow[i]&&r.parentNode)r.remove();});
    ref=hotsEl.firstChild;
    byRow.forEach(function(list,i){
      var row=rowEls[i]||(rowEls[i]=el('div')),cref;row.setAttribute('role','row');row.setAttribute('aria-rowindex',i);
      if(row===ref)ref=ref.nextSibling;else hotsEl.insertBefore(row,ref);
      cref=row.firstChild;
      list.forEach(function(c){
        var h=gridHot(c.node),n=cellNodes[c.node.id];
        if(n&&n._h._id!==h._id){n.parentNode.remove();n=null;}
        if(!n){n=cellNodes[c.node.id]=makeHot(h);el('div').appendChild(n);n.parentNode.setAttribute('role','gridcell');}
        var gc=n.parentNode;gc.setAttribute('aria-colindex',c.col||1);if(!c.col)gc.setAttribute('aria-colspan',all.cols);
        if(gc===cref)cref=cref.nextSibling;else row.insertBefore(gc,cref);
        n._n=c.node;n._cur=undefined;
        if(n._gen!==hotsGen||n._h!==h){fillHot(n,h);n.setAttribute('aria-label',CURNAV.sayAt(n.getAttribute('aria-label')||c.node.title,nav.where(c,all)));n._gen=hotsGen;}
      });
      while(cref){var nx=cref.nextSibling;cref.remove();cref=nx;}
    });
    hotsEl.setAttribute('aria-label',all.label);hotsEl.setAttribute('aria-rowcount',all.rows);hotsEl.setAttribute('aria-colcount',all.cols);
  }
  if(!cur)for(id in cellNodes){cur=cellNodes[id]._n;break;}   // no cursor before the first layout: the first cell is the tab stop
  for(id in cellNodes){var cn=cellNodes[id],on=cn._n===cur;if(cn._cur!==on){cn._cur=on;cn.tabIndex=on?0:-1;cn.classList.toggle('cur',on);}}
  var vis=WORLD.cull(gardenerHots.filter(hotWanted),WORLD.viewRect(cam,vp,.25)),gk=vis.map(function(h){return h._id;}).join('|');
  if(gk!==hotKey||force){
    hotKey=gk;var want={};vis.forEach(function(h){want[h._id]=h;});
    Object.keys(hotNodes).forEach(function(k){if(!want[k]){hotNodes[k].remove();delete hotNodes[k];}});
    vis.forEach(function(h){var n=hotNodes[h._id];if(!n){n=hotNodes[h._id]=makeHot(h);n.tabIndex=-1;ghotsEl.appendChild(n);}if(n._gen!==hotsGen||n._h!==h){fillHot(n,h);n._gen=hotsGen;}});
  }
  var fn=figFocus&&hotNodes['gardener:'+figFocus];   // plotGo's figure, once its link is drawn
  if(fn){figFocus='';outPin(false);try{fn.focus({preventScroll:true});}catch(e){fn.focus();}}
}
/* The link for a grid cell: the Thing's own from the world or a strip (its card and accessible name as before), else a button
   or link for what only the cursor world has: a home cell and its zones, a band, a bed, a crate. */
function gridHot(n){
  var h=hotIx[n.kind==='item'?'bead:'+n.id:n.drawer!==undefined?'overflow:'+n.id:n.kind+':'+n.id]||worldHot(n.kind,n.id,n.rect||{x:0,y:0,w:1,h:1},0,n.kind==='truck'?{tr:n.ref}:null);
  h.node=n;return h;
}
/* The cursor's tree (web/cursor.js) from the layout and each strip's model, rebuilt when the layout changed (a 10x farm's is
   ~15k nodes), else only the home cells that changed; the cursor follows its Thing. fill: homeSpec's, when already placed. */
function rebuildTree(fill){
  var o={trucks:world?TRUCK.hots(world).map(function(h){return h.tr;}):[]},hs=fill||CURNAV.homeSpec(world,model,slots,barnCounts());homeFill=hs;   // one fill for the cursor and the scene: a figure is drawn where its cell is
  if(!nav.tree||nav.tree.w!==world){var gen=nav.tree?nav.tree.gen+1:0;nav.build(world,hs,{gardeners:gspots,weeds:weedCounts(),trucks:o.trucks});nav.tree.w=world;nav.tree.gen=gen;}else if(nav.patch(hs,o))nav.tree.gen++;
  stripHots=[];Object.keys(hs).forEach(function(bid){['mailbox','growing','barn'].forEach(function(z){hs[bid][z].items.forEach(function(e){stripHots.push(worldHot('bead',e.id,e,undefined,{zone:e.kind==='stand'?'agent':ZW[z]}));});});});   // each strip's letters, plants and figures on their slots (28.2)
  hotIx={};stripHots.concat(biomeHots).forEach(function(h){hotIx[h._id]=h;});
}
/* The opening cursor (spec 3.5): a ?at=, else the saved path, else depth 0 on the first project, framed as a cut. */
function openCursor(){
  var at=pendingAt?pendingAt.at:'',saved=null;pendingAt=null;try{saved=localStorage.getItem(CURNAV.STORE);}catch(e){}
  navOpen=true;var found=nav.open(at,saved),r=at&&found?WORLD.resolve(world,at):null;
  if(at&&!found){toast('Nothing on the map is called “'+clip(at,40)+'”.');urlHold=true;}   // keep the link as typed until the cursor moves
  else if(r&&!r.found)toast('“'+clip(r.missing,40)+'” is not on the map right now (closed, or past a cap). Showing '+(r.biome?r.biome.name:'the map')+'.');
  else if(r&&r.moved)toast('The Yard is gone: every project has its own home now. Showing '+movedTo(r)+'.');
  syncHots(true);frameCursor('',true);
  if(r&&r.moved){urlHold=false;writeURL();}   // at once: no link copied says yard (H-D12)
}
/* Where a legacy yard link landed (28.7): "the Commons home", "the Commons pen", "the gardenio “<title>”". */
function movedTo(r){var n=nav.tree.nodes[nav.path[nav.path.length-1]],z=n.kind==='home'||n.kind==='zone'||n.kind==='pen';return 'the '+(r.biome?r.biome.name+' ':'')+(z?n.title.toLowerCase():'“'+clip(n.title,40)+'”');}
/* A plot's phase names for its accessible name, in row order, with the rows past the plot's ten. */
function phaseNames(p){
  var t=p.beds.map(function(bd){return bd.title;}).filter(Boolean);
  if(!t.length)return '';
  return 'Phases: '+t.join(' · ')+(p.moreRows?' · +'+p.moreRows+' more':'');
}
/* ---------- the world from the farm document ---------- */
function clip(s,n){var r=Array.from(String(s||'').trim());return r.length<=n?r.join(''):r.slice(0,n).join('').replace(/\s+$/,'')+'…';}
function stateWord(st){return {in_progress:'growing',review:'in review',blocked:'blocked',open:'planted',deferred:'dormant',closed:'closed',planned:'staked'}[st]||st||'';}
function biomeSig(b){return JSON.stringify([b.used,b.palette,b.seed,b.lanes,b.plots.map(function(p){return [p.id,p.slot,p.beds.length];})]);}   // the lanes are baked ground (29.7)
function worldHot(kind,id,r,lv,extra){var h={kind:kind,id:id,x:r.x*T,y:r.y*T,w:r.w*T,h:r.h*T,lv:lv};h._id=kind+':'+id;if(extra)for(var k in extra)h[k]=extra[k];return h;}
/* Lay the world out from a farm document and rebuild what depends on it: chunk invalidation under changed footprints, the hot
   list, the labels and the camera bounds. The memo is read only (never written) from an empty or truncated document. */
function onFarmDoc(doc){
  farmDoc=doc;nbStyles=NB.assign(doc);escLin=ESCKIND.lineage(doc&&doc.plans);   // an escalation's plan, for its cell's tip (28.11)
  var empty=!doc||(!(doc.plans||[]).length&&!(doc.seeds||[]).length&&!((doc.meadow||{}).shown||[]).length&&!((doc.closed_plans||{}).shown||[]).length);
  var w=WORLD.layout(doc,memo,{readonly:empty||!!(doc&&doc.meta&&doc.meta.truncated),cells:cellMemo,prev:world});   // prev: the cells' last widths, for their shrink hysteresis (29.10)
  if(w.memoChanged){memo=w.memo;try{localStorage.setItem(MEMO_KEY,JSON.stringify(memo));}catch(e){}}
  if(w.cellsChanged){cellMemo=w.cells;try{sessionStorage.setItem(CELLS_STORE,JSON.stringify(cellMemo));}catch(e){}}
  var sig={};w.biomes.forEach(function(b){sig[b.key]=biomeSig(b);});
  var an=anchorOf(),prev=world;world=w;
  Object.keys(footSig).forEach(function(k){if(sig[k]!==footSig[k]){var pb=prev&&prev.biomes.filter(function(b){return b.key===k;})[0];if(pb)SC.drop(WORLD.tiles(pb.foot));}});
  w.biomes.forEach(function(b){if(sig[b.key]!==footSig[b.key])SC.drop(WORLD.tiles(b.foot));});
  footSig=sig;
  worldBox=w.bbox;anchorCam(an,prev);   // after the bbox: the camera is clamped to the new world
  relint();edgeIx=EDGES.index(doc,w);
  /* the farm's gardeners on this layout (design section 4): their spots at the Plan and Bead levels and their looks, per farm
     document, never per frame; the stewards at their homesteads: the cost rows and a live run (Lands 9.8) */
  var prev=gspots;gspots=WORLD.gardenerState(world,doc&&doc.gardeners);sst=WORLD.stewardState(world,doc&&doc.cost,doc&&doc.gardeners,gspots);
  if(veil||veilOn()){if(!veil.doc){veil.doc=true;veil.gs=prev;}return;}   // GARDEN.known has what it reads; the rest once the veil lifts
  drawDoc(prev);
}
/* What a farm document changes on the screen: the transitions from the gardeners before it (prev), the plaque, the Porch and
   its pips, the hots, the cursor's tree and its opening framing, the signs, the beacons, the outline and a frame. */
function drawDoc(prev){
  NB.legend(nbStyles,me);
  model=buildModel();var hs=CURNAV.homeSpec(world,model,slots,barnCounts());   // the new field stands placed first (28.12): a walk leaves its gardener's stand, a fly-back aims at it
  ANIM.update(prev,gspots,{level:level,view:WORLD.viewRect(cam,vp,.25),reduce:reduce,cut:camShift?camShift.cut:null});landGardeners();   // the transitions (web/transitions.js, design section 7)
  counts();   // the plaque, the Porch and its pips from the new gardeners[]
  buildWorldHots();rebuildTree(hs);
  jumpIdx=null;if(!jumpEl.hidden)runJump(jumpQ.value);
  cam=WORLD.clampCam(cam,worldBox);if(!navOpen&&vp.w)openCursor();else if(camShift){if(camShift.moved)camChanged();reframe();}   // the opening cursor, framed; then the cursor keeps to its Thing (spec 3.4)
  syncHots(true);refreshSigns();syncBeacons();buildOutline();queueRender();   // the signs follow the cursor's Thing, its words refilled
  camShift=null;welcomeOnce();
}
/* ---------- the camera across a re-layout (homestead-world-design.md 29.11) ----------
   The anchor is the biome held still on the screen: the cursor's when its Thing meets the view, else the one whose region holds
   the view's centre, by its farm's top-left, which its own counts never move. */
function anchorOf(){
  if(!world||!vp.w)return null;
  var bn=navOpen&&nav.tree&&nav.tree.nodes[nav.path[0]],n=navOpen&&nav.node(),b=null,c=WORLD.toWorld(cam,vp,vp.w/2,vp.h/2);
  if(bn&&bn.kind==='biome'&&n&&n.rect&&WORLD.hit(WORLD.tiles(n.rect),WORLD.viewRect(cam,vp)))b=world.index[bn.id];
  if(!b)b=world.biomes.filter(function(o){return WORLD.hit(WORLD.tiles(o.region),{x:c.x,y:c.y,w:1,h:1});})[0];
  return b&&b.home?{id:b.id,x:b.home.x,y:b.home.y}:null;
}
/* The camera, its target, a running fly's (restarted from here), signFr and fitAt's camera move by the anchor's change, with no
   ease. camShift tells drawDoc whether it moved and the old feet of the biomes that slid or moved (ANIM.update's cut). */
function anchorCam(an,prev){
  var b=an&&world.index[an.id],dx=b&&b.home?(b.home.x-an.x)*T:0,dy=b&&b.home?(b.home.y-an.y)*T:0,cut=camShift?camShift.cut:[],mv=function(c){return c&&{cx:c.cx+dx,cy:c.cy+dy,z:c.z};};
  if(prev)prev.biomes.forEach(function(o){var nb=world.index[o.id];if(nb&&nb.kind==='biome'&&(nb.region.x!==o.region.x||nb.region.y!==o.region.y||nb.grid.g!==o.grid.g))cut.push(WORLD.tiles(o.foot));});
  camShift={cut:cut,moved:!!(dx||dy||(camShift&&camShift.moved))};   // two documents under the veil: both
  if(!dx&&!dy)return;
  cam=mv(cam);camTo=mv(camTo);signFr=mv(signFr);if(fitAt)fitAt.cam=mv(fitAt.cam);
  if(anim&&anim.to)flyTo(mv(anim.to),240);
}
/* A camera that is still the page's own framing eases to the new one: frameCursor's while not looked, else fitAt's nodes fitted
   again. A drag or a pinch (ptrs), or a camera the viewer moved, keeps the anchor only. */
function reframe(){
  if(Object.keys(ptrs).length)return;
  if(!looked){frameCursor('');return;}
  var at=anim&&camTo?camTo:cam,f=fitAt,r=f&&homeIn(f.p,f.n);
  if(!r||Math.abs(f.cam.cx-at.cx)>=.5||Math.abs(f.cam.cy-at.cy)>=.5||Math.abs(f.cam.z/at.z-1)>=.001)return;
  var to=fitHome({cx:at.cx,cy:at.cy,z:f.z},r);
  if(Math.abs(to.cx-at.cx)<.5&&Math.abs(to.cy-at.cy)<.5&&Math.abs(to.z/at.z-1)<.001)return;
  f.cam=to;camTo=signFr=to;flyTo(to,240);
}
/* ---------- the veil: a panel's deep link on a phone (gd-N5BigisFtS) ----------
   Under 900 px a Workbench panel covers the map from the bar to the tab bar (garden.css .bv). While one opened from the
   address loads, the first farm document is only laid out (above); the World draws nothing, and the cursor, the signs and the
   Porch wait, until the frame after the panel has drawn its document (workbench:landed), the first input, a GARDEN call that
   moves the map, or VEIL_MS. Run sooner, that work held the panel's paint, its LCP, behind it on a throttled phone. */
var VEIL_MS=6000,veil=null,panelLanded=false,narrowMq=window.matchMedia?window.matchMedia('(max-width:899px)'):null;
function veilOn(){
  if(navOpen||panelLanded||!narrowMq||!narrowMq.matches||typeof LAZY==='undefined'||!LAZY.panelIn(LAZY.opened))return false;
  veil={doc:false,farm:false,gs:null,timer:setTimeout(lift,VEIL_MS)};return true;
}
/* Lift it: what was held, at once. */
function lift(){
  if(!veil)return;
  var v=veil;veil=null;clearTimeout(v.timer);
  if(v.doc)drawDoc(v.gs);else if(v.farm)farmChanged();
  queueRender();
}
document.addEventListener('workbench:landed',function(){panelLanded=true;if(veil)raf(function(){setTimeout(lift,0);});});
['pointerdown','keydown','wheel'].forEach(function(n){document.addEventListener(n,lift,true);});
window.addEventListener('popstate',lift);onMq(narrowMq,lift);
/* Per document and as each transition lands: a gardener still on its way has no link and no barn slot (design rule 7.2.1). */
function landGardeners(){
  var hs=gspots?gspots.homeSig+ANIM.sig():'';if(hs!==homeSig){homeSig=hs;farmChanged();}
  gardenerHots=(gspots?gspots.hots.filter(function(g){return !ANIM.moving(g.id);}):[]).concat(gspots?gspots.esc.hots:[],sst?sst.hots:[],world?TRUCK.hots(world):[]).map(function(g){return worldHot(g.kind,g.id,g.rect,g.lv,g);});   // and each release truck's bay (web/truck.js)
  STIP.land(gardenerHots);
}
TRUCK.wire({toast:toast});document.addEventListener('trucks',function(){if(world){landGardeners();syncHots(true);refreshSigns();queueRender();}});   // a release gone live says so; the goldens' fed releases re-place the trucks' links
ANIM.onLand(function(){landGardeners();syncHots(true);queueRender();});BIRDS.onWake(function(){if(level>=2)queueRender();});GOATS.onWake(function(){if(level>=2)queueRender();});   // a call lifts a bird only where birds fly (Plan, Bead); a goat's next step, where goats graze
/* Weeds and fog (gd-ytTNc3Fu9M): the lint in web/weeds.js, placed on this world. Re-run on each document and once a minute,
   since wilt, long frost and dormancy come with age alone; the minute refills the hot links' names and repaints. */
var weedIx=null,edgeIx=null;   // edgeIx: the cross-project blocked_by edges (web/edges.js, gd-CNhEh30Ykf), per document
function relint(){weedIx=world&&farmDoc?WEEDS.index(WEEDS.lint(farmDoc,Date.now()/1000),world):null;}
/* The weeds per plot and per region (a dormant region's fog counts as one), for the signs' status lines (spec 6.4). */
function weedCounts(){var c={};if(weedIx){[weedIx.plots,weedIx.biomes].forEach(function(m){for(var k in m)c[k]=m[k].length;});for(var k in weedIx.fog)c[k]=(c[k]||0)+1;}return c;}
setInterval(function(){if(world&&!document.hidden){relint();syncHots(true);queueRender();}},60000);
function buildWorldHots(){
  biomeHots=[];var now=Date.now()/1000;
  world.biomes.forEach(function(b){
    biomeHots.push(worldHot('biome',b.id,b.foot,1));
    biomeHots.push(worldHot('sign',b.id,b.sign,2|4|8));
    b.plots.forEach(function(p){
      biomeHots.push(worldHot('plot',p.id,p,2|4,{plot:p}));
      biomeHots.push(worldHot('plotsign',p.id,{x:p.x,y:p.y,w:p.w,h:1},8,{plot:p}));
      p.beds.forEach(function(bd){bd.cells.forEach(function(c){var ask=c.task.type==='decision'||c.task.type==='escalation';biomeHots.push(worldHot('task',c.id,c,ask?4|8:8,{task:c.task,plot:p,bed:bd}));});});
    });
    if(b.overflow)biomeHots.push(worldHot('overflow',b.overflow.id,b.overflow,2|4|8,{over:b.overflow,biome:b}));
    b.orchard.concat(b.meadow,b.nursery,b.graveyard).forEach(function(it){biomeHots.push(worldHot(it.kind,it.id,it,8,{it:it}));});
  });
  void now;
}
/* A bead's address inside the garden (gd-BdhucNqv0i): this page with the bead view open. web/workbench.js catches the click and
   opens the panel in place; a new tab, a reload or a shared link opens the same panel from ?bead=. Same as render.go beadHref. */
function beadHref(x){return base+'/garden?bead='+encodeURIComponent(x.id);}
/* Label and link for a world hot. */
function fillWorldHot(node,h){
  var k=h.kind,b=k==='plot'||k==='plotsign'?h.plot.plan:k==='task'?h.task:k==='tree'?h.it.plan:k==='flower'?h.it.task:k==='seed'?h.it.seed:k==='stone'?h.it.wish:null;   // a headstone opens its wish's head bead
  if(b){node.href=beadHref(b);h.href=node.href;}   // the overflow crate is a button (openPlansDrawer)
  else if(k==='gardener'||k==='gesc'||k==='steward'){node.href=base+h.g.href;h.href=node.href;if(k!=='steward')node.dataset.glass=WORLD.glassView(h.g).key;}   // a gardener's Glass state: Prompt and Error pulse (GL-4)
  else if(k==='crate'&&/^home:/.test(h.node.open)){node.href=base+'/garden?queue=escalations';h.href=node.href;}   // the Escalations' "+N": every escalation (28.11)
  else if(k==='crate'||k==='escalation'){node.href=beadHref({id:h.node.open});h.href=node.href;}   // an escalation's cell: its bead (28.13)
  else if(k==='truck'){node.href=TRUCK.href(h.tr,base);h.href=node.href;}   // the release's changelog (web/release.js)   // "+N phases" and "+N more steps" open the plan or the phase
  if(k==='plot')node.classList.toggle('w-staked',!!h.plot.staked);   // a staked plot wears its seed's tip (web/stip.js) until it is planted
  cardData(h);
  node.setAttribute('aria-label',CURNAV.sentences(k==='gardener'&&gspots?WORLD.figureAria(gspots,h.g,false):h.tipTitle+': '+h.tipText,HERALDRY.hotAria(h)+WEEDS.aria(weedIx,h)+EDGES.aria(edgeIx,h)+BIRDS.aria(h)));   // a plot figure says where its click goes (H-D21)
}
/* The one-line summary of a world hot: tipTitle and tipText, the link's accessible name. Never the id (titles, not ids:
   gd-9yOATV2qCs); the bead view's Copy id has it. */
function cardData(h){
  var k=h.kind,now=Date.now()/1000,t,pl;
  if(k==='biome'||k==='sign'){
    var b=world.index[h.id],c=b.counts;
    h.tipTitle=b.name;h.tipText=plural(c.plans,'plan','plans')+' being built'+(c.seeds?', '+plural(c.seeds,'seed','seeds'):'')+(c.closed?', '+plural(c.closed,'harvested plan','harvested plans'):'')+(c.meadow?', '+plural(c.meadow,'loose bead','loose beads'):'')+(b.needs?'. '+plural(b.needs,'decision or escalation needs','decisions or escalations need')+' you.':'.')+' Open to fly in.';
  }else if(k==='seed'||(k==='plot'||k==='plotsign')&&h.plot.staked){var pt=STIP.potTip(k==='seed'?h.it.seed:h.plot.seed||{},k!=='seed');h.tipTitle=pt[0];h.tipText=pt[1];   // a Nursery pot or a staked plot: what it waits on (web/stip.js)
  }else if(k==='plot'||k==='plotsign'){
    pl=h.plot.plan;t=PLANS.tile(pl,now);
    var phs=phaseNames(h.plot);
    h.tipTitle=pl.title;h.tipText=t.done+' of '+t.total+' steps closed'+(t.counts.in_progress?', '+t.counts.in_progress+' growing':'')+(t.counts.blocked?', '+t.counts.blocked+' blocked':'')+(PLANS.waitLine(pl)?'. '+cap(PLANS.waitLine(pl))+'.':'.')+(phs?' '+phs+'.':'')+WORLD.whoWords(gspots,h.plot)+' Operator: '+NB.describe(NB.styleOf(nbStyles,pl),me)+(pl.project?'. '+pl.project:'');
  }else if(k==='task'){
    var tk=h.task;
    h.tipTitle=tk.title;h.tipText=(tk.state==='closed'?ART.harvestWords(tk).join(' · '):stateWord(tk.state))+(tk.type&&tk.type!=='task'?' '+tk.type:'')+(tk.assignee?' · '+tk.assignee:'')+(h.bed.title?' · '+h.bed.title:'')+' · '+ago(tk.updated)+' ago';
  }else if(k==='gardener'||k==='gesc'||k==='steward'||k==='gask'||k==='gmore'){var gt=WORLD.gardenerTip(h,gspots,world,base,ago);h.tipTitle=gt.title;h.tipText=gt.text;
  }else if(k==='truck'){var tc=TRUCK.card(h.tr,base,ago);h.tipTitle=tc.title;h.tipText=tc.tip.slice(tc.title.length+2);
  }else if(k==='overflow'){
    h.tipTitle='+'+h.over.n+' more plans';h.tipText='This biome shows its first '+WORLD.SLOT_CAP+' plots; the rest are listed in the Plans drawer. Open it.';
  }else if(k==='tree'){
    var cp=h.it.plan;
    h.tipTitle=cp.title;h.tipText='Harvested plan ('+(cp.state||'complete')+') · '+ago(cp.updated)+' ago';
  }else if(k==='stone'){var cw=h.it.wish,why=GOATS.reasonWords(cw.reason),np=(cw.produce||[]).length+(cw.produce_more||0);h.tipTitle=cw.title;h.tipText='Closed wish, '+GOATS.outcomeWord(cw.outcome).toLowerCase()+(why?': '+why:'')+' · closed '+ago(cw.closed)+' ago'+(np?' · its kin merged '+plural(np,'merge request','merge requests'):'');
  }else if(k==='flower'){
    var mt=h.it.task;
    h.tipTitle=mt.title;h.tipText='Loose bead, not in any plan: '+stateWord(mt.state)+(mt.assignee?' · '+mt.assignee:'')+' · '+ago(mt.updated)+' ago';
  }else if(k==='escalation'&&h.node){var ew=h.node.ref.wait,ef=ESCKIND.from(ew,escLin),en=ESCKIND.name(ew,Date.now())+(ef?', from '+ef:'')+'.',ec=en.indexOf(': ');   // its accessible name is ESCKIND's and where it came from (28.11); its tip is STIP's
    h.tipTitle=en.slice(0,ec);h.tipText=en.slice(ec+2);
  }else if(h.node){   // a cell only the cursor world has: its title and status line (spec 6.4); a Barn says its own project's account whole (28.6)
    var bc=k==='zone'&&h.node.zone==='barn'&&barnCounts(),g=bc&&bc['project:'+(h.node.key||'')];
    h.tipTitle=h.node.title;h.tipText=g?WORLD.barnWords(g)+'.'+ANIM.finished():h.node.status||h.node.title;
  }
}
/* ---------- outline: the accessible model (design section 8) ----------
   The cursor's tree (CURNAV.outline, so its order is the grids' reading order, spec 3.6), a home cell ending in its Gardeners,
   always in the DOM; off-screen-clipped until it has focus or the Menu's Outline item opens it. One tab stop (WAI-ARIA tree, roving
   tabindex): Up/Down move, Right/Left open and close, Home/End jump, Enter or Space is a cursor jump to the entry and hands focus
   to its link. A branch renders its children on first open and drops them on close, so a 10x farm costs its project rows. */
var outEl=document.getElementById('outline'),treeEl=document.getElementById('tree'),outBtn=document.getElementById('cam-outline');
var outOpen={},outCurKey='';
function outSub(n){var dw=n.kind==='gardener'&&WORLD.doingWord(gspots,n.id);return n.sub!==undefined?n.sub:n.kind==='gardener'?WORLD.gardenerWord(n).toLowerCase()+(dw?' · '+dw:'')+' · '+n.plot:'';}   // what it is doing in words, never the pose alone (4c)
function outKey(n){return n.kind==='gardener'?'gardener:'+n.id:n.hot||n.kind+':'+n.id;}   // gardeners behind one "+N" share its hot
function outRow(n,depth){
  var li=el('li'),row=el('div','orow'),k=outKey(n),kids=n.children&&n.children.length;
  li.setAttribute('role','treeitem');li.setAttribute('aria-level',depth);li.setAttribute('aria-selected','false');li.tabIndex=k===outCurKey?0:-1;li._n=n;li.dataset.key=k;
  row.appendChild(el('span','tw'+(kids?'':' leaf')));
  row.appendChild(el('span','ot',n.title));
  var sub=outSub(n);if(sub)row.appendChild(el('span','os',sub));
  li.appendChild(row);
  if(kids){li.setAttribute('aria-expanded',outOpen[k]?'true':'false');if(outOpen[k])li.appendChild(outGroup(n.children,depth+1));}
  return li;
}
function outGroup(list,depth){var ul=el('ul');ul.setAttribute('role','group');list.forEach(function(c){ul.appendChild(outRow(c,depth));});return ul;}
function outItems(){return Array.prototype.slice.call(treeEl.querySelectorAll('[role=treeitem]'));}
function outCurrent(){return treeEl.querySelector('[role=treeitem][tabindex="0"]');}
/* Rebuild the tree, a ledger patch's too (a home cell may change in any biome, 28.6). Open branches and the current row survive by key. */
function buildOutline(){
  if(!treeEl||!nav.tree)return;
  var gr=gspots&&world&&farmDoc&&(farmDoc.gardeners||[]).length?WORLD.gardenerOutline(world,farmDoc.gardeners,gspots[2],gspots[3]):null;   // in each home cell (28.7)
  var rows=CURNAV.outline(nav.tree,gr),had=treeEl.contains(document.activeElement);
  treeEl.textContent='';rows.forEach(function(r){treeEl.appendChild(outRow(r,1));});
  var cur=outCurrent();if(!cur){cur=treeEl.firstElementChild;if(cur){cur.tabIndex=0;outCurKey=cur.dataset.key;}}
  if(had&&cur&&!treeEl.contains(document.activeElement))cur.focus({preventScroll:true});
}
function outFocus(li){if(!li)return;var c=outCurrent();if(c&&c!==li)c.tabIndex=-1;li.tabIndex=0;outCurKey=li.dataset.key;li.focus({preventScroll:true});li.scrollIntoView({block:'nearest'});}
function outToggle(li,open){
  var n=li._n,k=li.dataset.key;if(!n||!n.children||!n.children.length)return false;
  if(open){if(outOpen[k])return false;outOpen[k]=1;li.setAttribute('aria-expanded','true');li.appendChild(outGroup(n.children,+li.getAttribute('aria-level')+1));}
  else{if(!outOpen[k])return false;delete outOpen[k];li.setAttribute('aria-expanded','false');var g=li.lastElementChild;if(g&&g.tagName==='UL')g.remove();}
  return true;
}
/* A cursor jump to the entry, focus on its link. A gardener on a plot: the cursor goes into the plot, focus with it, as Go to
   plot does (3b R13). */
function outSelect(li){
  var n=li._n;if(!n)return;
  outItems().forEach(function(x){x.setAttribute('aria-selected',x===li?'true':'false');});
  if(n.kind==='gardeners')return;   // the group itself: Right opens it
  var s=n.kind==='gardener'&&gspots&&gspots[3].spots[n.id];
  if(s&&s.plot){if(standGo(world.index[s.plot],true))focusCursor();return;}
  outPin(false);
  if(!cursorTo(n.kind==='gardener'?n.id:n.hot,true))toast(n.title+(n.barn?' is in its field, outside your scope. Switch to Everyone to see it.':' is not on the map.'));
}
/* Pinned open (the Menu's Outline item) or shown only while it has focus (Tab). A hand-off to the map unpins it. */
function outPin(open){if(!outEl)return;outEl.classList.toggle('open',open);outBtn.setAttribute('aria-expanded',open?'true':'false');}
function outShow(open){
  outPin(open);
  if(open){var c=outCurrent()||treeEl.firstElementChild;if(c)outFocus(c);}
}
function outClose(back){outShow(false);if(back){try{menuBtn.focus({preventScroll:true});}catch(e){menuBtn.focus();}}}   // the Outline item is in the closed Menu
if(treeEl){
  outBtn.onclick=function(){outShow(!outEl.classList.contains('open'));};
  document.getElementById('outline-x').onclick=function(){outClose(true);};
  treeEl.addEventListener('click',function(e){
    var li=e.target.closest?e.target.closest('[role=treeitem]'):null;if(!li)return;
    e.stopPropagation();
    if(e.target.classList&&e.target.classList.contains('tw')){outFocus(li);outToggle(li,li.getAttribute('aria-expanded')!=='true');return;}
    outFocus(li);outSelect(li);
  });
  treeEl.addEventListener('focusin',function(e){var li=e.target.closest?e.target.closest('[role=treeitem]'):null;if(li&&li.dataset.key!==outCurKey){var c=outCurrent();if(c)c.tabIndex=-1;li.tabIndex=0;outCurKey=li.dataset.key;}});
  treeEl.addEventListener('keydown',function(e){
    if(e.ctrlKey||e.metaKey||e.altKey)return;
    var li=e.target.closest?e.target.closest('[role=treeitem]'):null;if(!li)return;
    var items=outItems(),i=items.indexOf(li),k=e.key,to=null;
    if(k==='ArrowDown')to=items[i+1];
    else if(k==='ArrowUp')to=items[i-1];
    else if(k==='Home')to=items[0];
    else if(k==='End')to=items[items.length-1];
    else if(k==='ArrowRight'){if(li.getAttribute('aria-expanded')==='false')outToggle(li,true);else if(li.getAttribute('aria-expanded')==='true')to=items[i+1];}
    else if(k==='ArrowLeft'){if(li.getAttribute('aria-expanded')==='true')outToggle(li,false);else{var p=li.parentNode.closest('[role=treeitem]');if(p)to=p;}}
    else if(k==='Enter'||k===' '){outSelect(li);}
    else if(k==='Escape'){outClose(true);}
    else return;
    e.preventDefault();
    if(to)outFocus(to);
  });
}
onMq(reduceMq,function(){reduce=!!reduceMq.matches;if(reduce)stopAnim();queueRender();});
onMq(coarseMq,function(){coarse=!!coarseMq.matches;syncHots(true);});
document.addEventListener('farmdoc',function(e){try{onFarmDoc(e.detail);}catch(err){if(window.console)console.error('world layout failed',err);}});['farmactivity','farmdoing'].forEach(function(n){document.addEventListener(n,function(){if(level>=2&&gspots)queueRender();});});   // a doing pose changes (4c), or comes into view (4g), on a plot or at a barn (5b)
/* ---------- beacons (gd-gtr7qBCn9z, web/beacons.js): an edge arrow for each plan, or a project's Mailbox, that needs you off-screen,
   one with a count where several share a direction; activating one jumps the cursor there at its natural depth (spec section 8).
   What they keep off is measured at most once a second at rest (a forced layout), and at once on a resize, or a drawer toggle. ---------- */
var bcnEl=document.getElementById('bcns'),bcnTg=[],bcnSrc=[],bcnAt=null,bcnNow=[];   // bcnNow: the arrows on screen, which a tip keeps off (web/stip.js)
function syncBeacons(){
  if(!bcnEl)return;
  if(bcnAt&&Date.now()-bcnAt.t>1000&&!(anim||(drag&&drag.on)||pinch))bcnAt=null;
  BEACONS.render(bcnEl,bcnNow=bcnLayout(cam),goBeacon);
}
/* The arrows at camera c: what the beacons show, and what the cursor's framing keeps off (fitClear). */
function bcnLayout(c){
  if(!bcnEl)return [];   // the first resize() runs before this section has
  if(bcnSrc[0]!==world||bcnSrc[1]!==model){bcnSrc=[world,model];bcnTg=BEACONS.targets(world,model?model.decision:[],T);}   // the loose asks' arrows: each project's Mailbox (28.6)
  if(!bcnAt||bcnAt.w!==vp.w||bcnAt.h!==vp.h){bcnAt=BEACONS.measure(board,hdr);bcnAt.w=vp.w;bcnAt.h=vp.h;bcnAt.t=Date.now();}
  var gp=navOpen&&nav.tree&&CURSOR.gatePlot(nav.tree,nav.path),gr=gp&&HERALDRY.gateRect(gp.ref,T,WORLD.BED.off[0],c,vp,night);   // the cursor plot's arms, which an arrow slides off like a control (gd-rA31ZIfHdv)
  var bk=bcnAt.blocks.concat(BEACONS.keepOff(world,c,vp,T,bcnAt.top),gr?[gr]:[]);   // and the farmstead cells' items in view (gd-WzYgtbwMT7)
  return vp.w?BEACONS.layout(bcnTg,c,vp,{size:coarse?BEACONS.SIZE_COARSE:BEACONS.SIZE,top:bcnAt.top,blocks:bk}):[];
}
function goBeacon(t,keyed){cursorTo(t.id,keyed);}
document.addEventListener('toggle',function(){bcnAt=null;syncBeacons();},true);   // a drawer opened or closed: re-measure what to keep off
/* ---------- the chore tour (gd-S2Cn2dTsP0, web/tour.js): Tend walks the five most neglected things, one stop at a time ----------
   A cursor walk (spec 8): a stop or Next is a cursor jump to the stop's Thing, framed around the card (camVp); the summary,
   Done and Esc leave the cursor on the last stop and re-frame it without the card. */
var tourCard=TOUR.mount(tourEl,document.getElementById('cam-tend'),{post:post,mail:function(zid){paneTo('mail',zid);},href:function(id){return beadHref({id:id});},toast:toast,back:menuBtn,
  stops:function(){return world?TOUR.stops({doc:farmDoc,ix:weedIx,targets:bcnTg,world:world,asks:model?readSec('decision'):[],now:Date.now()/1000,me:me,ro:B.dataset.ro==='1',at:function(id){return WORLD.atFor(world,null,id);}}):[];},
  show:function(s){bcnAt=null;if(!s||!(cursorTo(s.cur)||cursorTo(s.at)))frameCursor('to');}});   // bcnAt: the card is one more thing the arrows keep off
/* The welcome tour (step 9.14b, gardenhub welcome.go): once the map is up, on the Farm; it ends at the land's steward, which
   greets in #general (commonsHref). Finished or dismissed, POST /garden/welcome records it. */
var welcomeDue=B.dataset.welcome||'';
function welcomeOnce(){
  if(!welcomeDue||!world||!navOpen||!tourCard)return;
  var land=welcomeDue;welcomeDue='';
  tourCard.start({name:'Welcome',last:'Open the steward',
    stops:TOUR.welcome({land:land,role:B.dataset.welcomeRole||'',gate:B.dataset.welcomeGate==='1',at:WORLD.atFor(world,null,'project:'+land)}),
    done:function(){location.assign(base+'/garden/c/s/'+encodeURIComponent(land));},
    ended:function(){post('/garden/welcome',{}).catch(function(){});}});
}
welcomeOnce();
/* The board px under the floating header, with a 4 px margin: the top the tour's card keeps below. */
function hdrBottom(){var o=board.getBoundingClientRect(),hb=hdr?hdr.getBoundingClientRect():null;return Math.max(4,hb&&hb.height?hb.bottom-o.top+4:4);}

/* ---------- deep links: /garden?at=<project>[/<id>] (section 7, spec 3.5): at= names the cursor's Thing; z= is ignored ---------- */
/* After the cursor settles (no drag, pinch or flight for 400 ms) the address bar names its Thing, so a view can be shared; the
   write drops z=. An untouched page keeps its address. */
function scheduleURL(){
  clearTimeout(urlTimer);
  if(auto&&(urlHold||WORLD.queryGet(location.search,'at')===null))return;
  urlHold=false;urlTimer=setTimeout(writeURL,400);
}
function writeURL(){
  if(anim||drag||pinch||pendingAt){scheduleURL();return;}
  if(!history.replaceState||!navOpen||(auto&&urlHold))return;
  var search=WORLD.linkQuery(location.search,nav.at()||null,null),url=location.pathname+search+location.hash;
  if(url!==location.pathname+location.search+location.hash)try{history.replaceState(history.state,'',url);}catch(e){}   // keeps web/workbench.js's wbDeep
}
/* ?land= (Lands 8.8): with no at=, the camera opens on the land, as at=<land> would. */
(function(){
  var at=WORLD.queryGet(location.search,'at');
  if(at===null||!String(at).trim())at=WORLD.queryGet(location.search,'land');
  if(at===null||!String(at).trim())return;
  pendingAt={at:String(at).trim()};
})();

/* ---------- the '/' jump: type a project, plan title or bead id, arrow to a match, Enter to fly there (section 7) ---------- */
/* Cmd-K and Ctrl-K open it too (web/search.js), and its last row searches every bead the viewer may see in the Workbench's
   search panel (?q=, Lands WB-5): the map's matches first, then that row, which is the first when the map has none. */
var KIND_WORD={stand:'gardener',home:'home',pen:'pen',goat:'open wish',biome:'project',plot:'plan',task:'bead',tree:'harvested',flower:'loose bead',seed:'seed',graveyard:'graveyard',stone:'closed wish',open:'bead',search:'search'};
function openJump(q){
  if(screen!=='farm')showScreen('farm');
  jumpEl.hidden=false;jumpQ.value=q||'';runJump(jumpQ.value);
  jumpQ.focus();
}
function closeJump(refocus){
  if(jumpEl.hidden)return;
  jumpEl.hidden=true;jumpList.textContent='';jumpHits=[];
  if(refocus!==false)try{board.focus({preventScroll:true});}catch(e){board.focus();}
}
function runJump(q){
  if(!jumpIdx)jumpIdx=WORLD.searchIndex(world,gspots);   // the field stands too, by agent name (28.12)
  var b=navOpen&&nav.tree&&nav.path.length?nav.tree.nodes[nav.path[0]]:null;
  jumpHits=WORLD.search(jumpIdx,q,8,b&&b.kind==='biome'&&b.key?'home:'+b.key:'home:');   // the empty list leads with the home Home goes to
  var raw=String(q||'').trim(),lower=raw.toLowerCase();
  if(/^[a-z]{1,6}-[A-Za-z0-9]{4,}$/i.test(raw)&&!jumpHits.some(function(e){return String(e.id).toLowerCase()===lower||String(e.bead||'').toLowerCase()===lower;}))
    jumpHits.push({kind:'open',id:raw,title:'Open '+raw,project:'not on the map: closed, or past a cap'});
  if(raw&&bench('q'))jumpHits.push({kind:'search',id:raw,title:'Search every bead for “'+raw+'”'});
  jumpSel=0;renderJump();
}
function renderJump(){
  jumpList.textContent='';
  if(!jumpHits.length){jumpList.appendChild(el('li','jump-none','No match. Try a project, a plan title or a bead id.'));jumpQ.removeAttribute('aria-activedescendant');return;}
  jumpHits.forEach(function(e,i){
    var sub=e.kind==='stand'&&!jumpQ.value.trim(),li=el('li','jump-it'+(i===jumpSel?' sel':'')+(sub?' jump-sub':''));li.id='jump-'+i;li.setAttribute('role','option');li.setAttribute('aria-selected',i===jumpSel?'true':'false');   // the empty list's stands sit under their home row, the field's, which names the project (gd-a628FpJRO5)
    li.appendChild(el('span','jk',KIND_WORD[e.kind]||e.kind));li.appendChild(el('span','jt',e.title));
    /* the id in the meta only when an id is what was typed; always the row's tooltip (titles, not ids: gd-9yOATV2qCs) */
    var m=[],byId=/^[a-z]{1,6}-/i.test(jumpQ.value.trim());var bare=e.kind==='biome'||e.kind==='home'||e.kind==='pen'||e.kind==='graveyard';if(byId&&!bare&&e.kind!=='open'&&e.kind!=='search')m.push(e.bead||e.id);if(!bare&&e.kind!=='search')li.title=e.bead||e.id;if(e.project&&e.kind!=='biome'&&!sub)m.push(e.project);if(e.plan)m.push(clip(e.plan,30));if(e.state)m.push(stateWord(e.state));if(e.note)m.push(e.note);
    if(m.length)li.appendChild(el('span','jm',m.join(' · ')));
    li.addEventListener('mousedown',function(ev){ev.preventDefault();});   // keep focus in the input
    li.addEventListener('click',function(){jumpSel=i;goJump();});
    li.addEventListener('mousemove',function(){if(jumpSel!==i){jumpSel=i;renderJump();}});
    jumpList.appendChild(li);
  });
  jumpQ.setAttribute('aria-activedescendant','jump-'+jumpSel);
  var s=jumpList.children[jumpSel];if(s&&s.scrollIntoView)try{s.scrollIntoView({block:'nearest'});}catch(e){}
}
function goJump(){
  var e=jumpHits[jumpSel];if(!e)return;
  closeJump(false);
  if(e.kind==='open'){if(typeof BEAD!=='undefined')BEAD.open(e.id);return;}   // not on the map: the bead view still reads it
  if(e.kind==='search'){benchOpen('q',e.id);return;}   // every bead, in the search panel
  if(e.kind==='stand'&&gspots&&gspots.byId[e.bead]){fieldGo(gspots.byId[e.bead]);return;}   // its field figure, else its field behind the "+N"
  if(!cursorTo(e.id,true)){toast('That is no longer on the map.');board.focus();}   // a strip, a pen, a goat: the cursor's own nodes (28.7)
}
jumpQ.addEventListener('input',function(){runJump(jumpQ.value);});
jumpQ.addEventListener('keydown',function(e){
  var k=e.key;
  if(k==='ArrowDown'||k==='ArrowUp'){e.preventDefault();e.stopPropagation();if(jumpHits.length){jumpSel=(jumpSel+(k==='ArrowDown'?1:jumpHits.length-1))%jumpHits.length;renderJump();}}
  else if(k==='Enter'){e.preventDefault();e.stopPropagation();goJump();}
  else if(k==='Escape'){e.preventDefault();e.stopPropagation();closeJump();}
  else if(k==='Home'||k==='End'){if(jumpQ.value)return;e.preventDefault();e.stopPropagation();if(jumpHits.length){jumpSel=k==='Home'?0:jumpHits.length-1;renderJump();}}
  else e.stopPropagation();   // typing never pans the map
});
jumpEl.addEventListener('pointerdown',function(e){e.stopPropagation();});
jumpEl.addEventListener('wheel',function(e){e.stopPropagation();},{passive:true});
document.addEventListener('pointerdown',function(e){if(!jumpEl.hidden&&!jumpEl.contains(e.target))closeJump(false);});
document.getElementById('jump-close').onclick=function(){closeJump();};

function refreshFarm(){
  farmQueued=false;
  if(veil){veil.farm=true;return;}
  model=buildModel();
  rebuildTree();   // each strip's fill and its links
  syncHots(true);refreshSigns();buildOutline();syncBeacons();queueRender();   // a live patch keeps the focused link and refills the signs' words (gd-Z1YadeeIFf)
}
function farmChanged(){
  if(farmQueued)return;
  farmQueued=true;(window.requestAnimationFrame||setTimeout)(refreshFarm);
}
function buildLegend(){
  var ul=document.getElementById('legend'),rows=[
    ['sprout','Growing: in progress'],['fruit','Fruiting: a report is attached'],['wilt','Wilted: no update for 12 hours'],
    ['blocked','Weeds and a rock: waiting on a decision'],['bug','Beetle: a bug'],['post','Signpost: a decision is ready to harvest'],
    ['working','Hoe: a gardener at work, editing a file or running a command'],['idle','Gardener resting'],['starting','Sprout: a gardener starting up'],['crashed','Scarecrow: the agent is down; its card says why'],['ask','Speech bubble: waiting on a human; click it to answer in the Mailbox'],['asleep','Zzz: asleep until its wake condition'],['gwilt','Wilting: no progress for an hour or more'],['truck','Truck: a gardener on its way to a plot'],['release','Red truck: a release by its project’s sign, a crate per plan, its PR count on the door and over the cab where it runs or its status, and since when; it drives out when the release goes live, and a click opens its changelog'],['steward','Steward: minds its land by the sign; hover or tap it for what it is doing'],['thread','Pink shirt: a thread agent, answering a Commons thread; at the barn it says which'],['do-read','Kneeling with a magnifier: reading files'],['do-test','Watering can, pouring: running tests'],['do-build','Kneeling at a pot on the fire: building'],['do-git','Striding with a crate: git'],['do-beads','Clipboard held up: writing beads'],['do-deploy','Arm up on a flagpole: deploying'],['do-web','Spyglass: on the web'],['do-delegate','Both arms up, calling the hawk: a subagent is out'],['do-think','Bubble with three dots: thinking'],['do-waiting','Leaning hard on the hoe: waiting on a tool for 5 s or more'],
    ['thistle','Thistle: a weed to tend; its card says why'],['fog','Fog: a project with no activity for 14 days'],['channel','Irrigation channel: a step waits on a bead in another project; the gate is at the end that waits']];
  rows.forEach(function(r){
    var li=el('li'),c=document.createElement('canvas');c.width=24;c.height=28;c.setAttribute('aria-hidden','true');
    li.appendChild(c);li.appendChild(el('span','',r[1]));ul.appendChild(li);r.push(c);
  });
  /* always the day palette on the day grass (garden.css .legend canvas): the night tint darkens the glyphs to one blob on the
     sheet, and a key is read, not lit (gd-8YEjli6gpl) */
  rows.forEach(function(r){var x=r[2].getContext('2d');if(r[0]==='channel')EDGES.drawLegend(x,false);else if(r[0]==='thistle'||r[0]==='fog')WEEDS.drawLegend(x,r[0],ART.palette(false));else ART.drawLegend(x,r[0],false);});
}
/* The sky (web/sky.js, gd-SwRqFcjWc6) flipped: repaint all painted from a palette, the chunks and the Porch. */
var bSky=document.getElementById('cam-sky');SKY.label(bSky);bSky.onclick=function(){SKY.cycle();};   // the Menu's Sky item (3b R10)
SKY.on(function(){var n=SKY.night();if(n===night)return;night=n;queueRender();if(gspots)porch();});

var bMine=document.getElementById('b-mine');
if(!me){bMine.disabled=true;bMine.title='Sign in to see only your own';}
bMine.onclick=function(){setScope('mine');};
document.getElementById('b-all').onclick=function(){setScope('all');};
Array.prototype.forEach.call(document.querySelectorAll('#tabbar button'),function(b){b.onclick=function(){goPane(b.dataset.go);};});
/* A desktop's Porch and Mailbox buttons (3b R5): each opens the drawer on its pane, as the tab bar does on a phone; a press on the
   one already open closes the drawer. */
Array.prototype.forEach.call(document.querySelectorAll('.dbtn'),function(b){b.onclick=function(){
  var dd=document.getElementById('dw-dock');
  if(!dd.hidden&&screen===b.dataset.pane){dd.hidden=true;showScreen('farm');return;}
  goPane(b.dataset.pane);
};});
document.addEventListener('keydown',function(e){
  if(e.key!=='Escape')return;
  var a=document.activeElement;
  if(outEl&&outEl.contains(a)&&!treeEl.contains(a)){outClose(true);return;}   // the tree handles its own Escape
  var dw=a&&a.closest&&a.closest('details.dw[open]');if(dw)dw.open=false;   // its toggle hands focus to the Menu
});
document.addEventListener('visibilitychange',function(){
  if(document.hidden){hiddenAt=Date.now();if(es){es.close();es=null;}status();}
  else{connect();}
});
setInterval(function(){times();status();if(es&&Date.now()-lastMsg>45000)connect();if(level>=2&&gspots)queueRender();if(BIRDS.stale()||GOATS.stale()||TRUCK.stale())refreshSigns();},5000);   // the repaint: a call out 5 s is waiting, a doing gone stale is the hoe (4c); a busy project's sign re-read (4b)
/* The plots and the strips animate at the Bead level while one is on screen; further out they hold still (the rings there went, gd-GgKjBgsc0U). */
setInterval(function(){
  if(reduce||document.hidden||!world||level<3)return;
  var v=WORLD.viewRect(cam,vp,0),vis=WORLD.visible(world,v,level);
  if(!vis.plots.length&&!vis.biomes.some(function(b){return WORLD.hit(WORLD.tiles(b.home),v);}))return;
  frame++;queueRender();
},600);
/* Hooks for web/workbench.js, the in-garden bead view: what the page already knows about a bead (so the panel paints before its
   fetch returns), whether the map shows it, a flight there, and the Farm view the panel lives in. */
window.GARDEN={
  known:function(id){
    var li=document.getElementById('i-'+id),a=li&&li.querySelector('a.t'),ws=world&&world.index['wish:'+id],it=world&&(world.index[id]||(ws&&ws.kind==='stone'?ws:null)),o={id:id,onMap:false};   // a closed wish is on the map as its headstone, wish:<id> (an open one's goat wanders: below); a strip's letter, plant or figure is in world.index only where WORLD.homeFill placed it, never past its zone's +N
    if(a){o.title=a.textContent;o.status=li.dataset.st||'';o.type=li.dataset.ty||'';o.project=li.dataset.pj||'';o.updated=+li.dataset.u||0;o.act=li.querySelector('.act');if(li.dataset.arms)o.arms={canonical:li.dataset.arms,n:+li.dataset.an||1,name:li.dataset.aname||''};}
    if(it&&it.kind!=='biome'){
      var x=it.staked?it.seed:it.kind==='plot'?it.plan:it.kind==='task'?it.task:it.kind==='tree'?it.plan:it.kind==='flower'?it.task:it.kind==='seed'?it.seed:it.kind==='stone'?it.wish:null;
      if(x){o.title=o.title||x.title;o.status=o.status||x.state||(it.kind==='stone'?'closed':'');o.type=o.type||x.type||(it.staked||it.kind==='seed'?'seed':it.kind==='plot'||it.kind==='tree'?'plan':'');o.project=o.project||x.project||'';o.updated=o.updated||x.updated||x.activity||0;}
      if(it.kind==='seed'||it.staked){o.seed=it.seed;o.staked=!!it.staked;}   // the bead view's Nursery section: the pot's (or staked plot's) state and its decision's card (web/stip.js potCard)
      if(it.kind==='plot'&&!it.staked){o.plan=it.plan;o.needs=it.needs;o.who=WORLD.plotWho(gspots,it);o.operator=NB.describe(NB.styleOf(nbStyles,it.plan),me);}   // needs: the plot's "!N" ("needs you"); who and operator: the bead view's sections (spec 6.4)
      var wh={kind:it.kind,id:id};o.weeds=WEEDS.lines(weedIx,wh,beadHref);o.edges=EDGES.lines(edgeIx,wh,beadHref);   // why each weed here is one, and its edges to other projects
      o.onMap=true;
    }
    if(!it&&weedIx&&weedIx.wishes[id])o.weeds=WEEDS.lines(weedIx,{kind:'wish',id:id},beadHref);   // an untended wish: its goat wanders, so it is no Thing on the map
    if(gspots)WORLD.gardenerKnown(o,gspots,id);
    return o;
  },
  /* The Glass's Enter and its "Go to its plot" (web/glass.js, H-D21): close the panel and fly to the gardener's plot figure at
     full zoom; false, and nothing moves, for one on no plot. */
  plot:function(id){var s=gspots&&gspots[3].spots[id];if(!s||!s.plot||!world.index[s.plot])return false;lift();if(typeof WORKBENCH!=='undefined')WORKBENCH.close();porchGo(id,true);return true;},
  /* "Show on the map": a cursor jump to the bead; a gardener on a plot puts the cursor inside its plot, where it stands. */
  fly:function(id){lift();var gs=gspots&&gspots[3].spots[id];return gs&&gs.plot?standGo(world.index[gs.plot],false):cursorTo(id,false);},
  /* The bead view opens over the map. On a phone the Mailbox or Porch screen hides the map (and the panel with it), so
     leave it for the farm and return the screen left, which toScreen restores when the panel closes (gd-e3LWfAZczq). On
     desktop the dock sits beside the map and its screen is only the pane the drawer shows, so it stays. */
  toFarm:function(){
    closeJump(false);
    var was=screen;
    if(mapShown())return '';
    showScreen('farm');return was;
  },
  toScreen:function(s){if((s==='mail'||s==='porch')&&screen!==s)showScreen(s);},
  tabs:markTabs,   // the tab bar marks the tab on screen again (web/workbench.js)
  /* The farm document's cost (cost.projects[], farm_cost.go), or null before it lands or without a spend source: the Costs
     panel's header figures (web/costs.js), so the numbers and the charts sit together. */
  cost:function(){return farmDoc&&farmDoc.cost||null;},farm:function(){return farmDoc;},   // the farm document, for a transcript's gardeners and their plots' arms (web/transcript.js)
  /* The project the cursor is in (its biome's key), or '' in the Commons and over every project: the Menu's Costs item's scope. */
  here:function(){var n=navOpen&&nav.node();return n&&n.key||'';},
  /* The map the signs use (signVp: board px, its HUD insets): a gardener's card keeps inside it too (web/wall.js, gd-LWjSUInmGI). */
  map:signVp,
  /* The '/' jump, opened on q: Cmd-K's way in (web/search.js). */
  find:function(q){lift();openJump(q);},
  /* Snap with another file cap: the browser test's way onto the DPR 1 redraw (gd-GbF2JS04h5). */
  snap:function(max){lift();return snapFarm(max||SNAP.MAX);},
  /* The Menu's Postcard, with another file cap: the browser test's way onto it (gd-WxRuyfdUtR). */
  postcard:function(max){lift();return postcard(max||POSTCARD.MAX);},
  /* The camera at c ({cx, cy, z}, world px; a missing one keeps the camera's) as a cut (or a fly of ms), the cursor where it is:
     the goldens' way onto a framing the cursor never rests at. c.on ('cursor' or plot ids): centred in camVp when it fits, the
     signs placed again at a new zoom (gd-ISIukGDl4w). It holds until the cursor is framed again (looked). */
  look:function(c,ms){
    lift();c=c||{};var to={cx:'cx' in c?c.cx:cam.cx,cy:'cy' in c?c.cy:cam.cy,z:c.z||cam.z},r=null,n=navOpen&&nav.node(),T=WORLD.TILE,on=false;
    if(c.on==='cursor')r=n&&n.rect;else if(c.on&&world)c.on.forEach(function(id){var p=world.index[id];if(p)r=WORLD.union(r,{x:p.x,y:p.y,w:p.w,h:p.h});});
    if(r){var v=camVp(),l=v.left||0,iw=v.w-l-(v.right||0),ih=v.h-v.top-(v.bottom||0);
      if(r.w*T*to.z<=iw&&r.h*T*to.z<=ih){to.cx=(r.x+r.w/2)*T-(l+iw/2-v.w/2)/to.z;to.cy=(r.y+r.h/2)*T-(v.top+ih/2-v.h/2)/to.z;on=true;}}
    looked=true;flyTo(to,ms||0);if(on&&!(signFr&&signFr.z===cam.z)){signFr=WORLD.clampCam(to,worldBox);signsSoon();}
    return {cx:cam.cx,cy:cam.cy,z:cam.z};
  },
  /* The camera and its level (a WORLD.LEVEL_NAMES index): the tests' read of the zoom band, which the farm no longer shows (3b R2). */
  cam:function(){return {level:level,z:cam.z,cam:cam,vp:vp,ghots:gardenerHots,safe:EDGES.safeView(signView,cam,vp),w:world,anim:!!anim};}   // safe: where a bridge's name is kept, for the bridge walk; w and anim: the camera walk's (camgrow_browser.js)
};
Array.prototype.forEach.call(document.querySelectorAll('.item'),tag);
buildLegend();times();refreshFarm();setScope(scope);
showScreen(location.hash==='#mail'?'mail':location.hash==='#porch'?'porch':'farm');
window.addEventListener('hashchange',function(){var h=location.hash;if(h==='#mail'||h==='#porch')goPane(h.slice(1));else if(h===''&&screen!=='farm')showScreen('farm');});
connect();
})();
