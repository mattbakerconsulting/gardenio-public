/* SNAP: the farm canvas as a PNG posted into the Commons, with what the view frames (gd-GbF2JS04h5), moved out of garden.js.
   The picture is #cv as drawn (its device-pixel backing store), never the labels or links over it: the context the
   Commons keeps beside it (snapContext, fields.snap) names what is framed and links the view. A PNG over the Commons'
   10 MB file cap (a big board at DPR 2-3) is drawn again at DPR 1, unsmoothed, so the pixel art stays crisp.
   Signed-in viewers only, since posting needs one; hidden for good once the Commons says a snapshot can go nowhere.
   garden.js passes its state in at the press (wire's frame()): the canvas, viewport, camera, level, world, model, the hot
   rects it may show, the page path, scope and night. snapContext, snapName and snapItem are pure and run under node
   (snap_test.go); the rest is browser only.
   Where it goes (gd-ZeiSZV7j2Y, the band it was dropped into is gone): Snap posts it to the land's #general (the page's
   ?land=, else the viewer's steward's land, GET /garden/c/s) and opens its node in the workbench, where the words follow
   as replies. The Menu's To the steward (steward view S2, gd-LisW87zrnf) posts it there with @steward and opens the
   steward's view on that thread. A postcard (web/postcard.js) posts through post() to its plan's thread. */
var SNAP = (function () {
  "use strict";
  var MAX=10<<20;
  function dec(s){try{return decodeURIComponent(s);}catch(e){return s;}}
  /* The context the server keeps as fields.snap (internal/commons/snap.go, which bounds it again). o: {path (base + /garden),
     search, at, level, who, night, items: [{kind (a hot kind), id, title}] nearest the centre first, vp: {w, h}, now: Date}. A
     hot becomes a project (a biome with a name), the wild's biome, a plan or a bead; anything else (the mailbox, the barn) is
     not framed. One entry per thing (a plot and its sign are one plan), SNAP_FRAMED at most. */
  var SNAP_FRAMED=24,SNAP_KIND={biome:'project',sign:'project',plot:'plan',plotsign:'plan',tree:'plan',task:'bead',flower:'bead',seed:'bead',bead:'bead'};
  function snapContext(o){
    var framed=[],seen={};
    (o.items||[]).forEach(function(it){
      var k=SNAP_KIND[it.kind],id=String(it.id||'');
      if(!k||!id||framed.length>=SNAP_FRAMED)return;
      if(k==='project'&&(id==='~'||id==='project:'))k='biome';
      if(seen[k+':'+id])return;
      seen[k+':'+id]=1;
      framed.push({kind:k,id:id,title:String(it.title||'').slice(0,120)});
    });
    var at=o.at||'',search=o.search||'';
    var q=String(search).replace(/^\?/,'').split('&').filter(function(kv){var k=kv.split('=')[0];return kv&&k!=='at'&&k!=='z';});
    if(at){q.push('at='+at);if(o.level!==null&&o.level!==undefined)q.push('z='+o.level);}
    var c={kind:'farm',url:o.path+(q.length?'?'+q.join('&'):''),at:at};
    if(o.level!==null&&o.level!==undefined)c.z=o.level;
    c.who=o.who==='mine'?'mine':'all';
    c.night=!!o.night;
    c.framed=framed;
    c.vp=[Math.round(o.vp.w),Math.round(o.vp.h)];
    c.taken_at=new Date(Math.floor(o.now.getTime()/1000)*1000).toISOString().replace('.000Z','Z');
    return c;
  }
  /* farm-<land>-<at or map>-<YYYYMMDD-HHMM>.png, in the viewer's own clock; each part kept to letters, digits, . _ - */
  function snapName(land,at,d){
    var seg=function(s,n){return dec(String(s||'')).replace(/^~\/?/,'').replace(/[^A-Za-z0-9._-]+/g,'-').replace(/^[-.]+|-+$/g,'').slice(0,n);};
    var p=function(n){return(n<10?'0':'')+n;};
    return 'farm-'+(seg(land,40)||'land')+'-'+(seg(at,60)||'map')+'-'+d.getFullYear()+p(d.getMonth()+1)+p(d.getDate())+'-'+p(d.getHours())+p(d.getMinutes())+'.png';
  }
  /* The PNG blob, named name, posted with its context ctx and words text: into thread to.thread (a reply), else to.land's
     channel to.channel (#general by default). Resolves the new message's id; rejects with the reason, .gone when attaching
     is not enabled here. */
  function post(base,to,blob,ctx,text,name){
    var e=encodeURIComponent,url=to.thread?base+'/garden/c/t/'+e(to.thread)+'/reply':base+'/garden/c/'+e(to.land)+'/'+e(to.channel||'general')+'/say',body=new FormData();
    body.append('text',text||'');body.append('file',new File([blob],name,{type:'image/png'}),name);body.append('context',JSON.stringify(ctx));
    return fetch(url,{method:'POST',credentials:'same-origin',body:body,headers:{'X-Commons':'fragment'}}).then(function(r){
      return r.text().then(function(t){
        if(r.ok){var li=new DOMParser().parseFromString(t,'text/html').querySelector('[data-id]');if(li)return li.dataset.id;throw new Error('The snapshot was posted, but its message could not be found.');}
        var x=new Error(r.status===401?'Your session ended. Reload the page to sign in again.':r.status===403?'You may not post there.':t.trim().length&&t.trim().length<200?t.trim():'The snapshot was not posted ('+r.status+').');
        x.gone=r.status===415;throw x;
      });
    },function(){throw new Error('The garden could not be reached.');});
  }
  function canvasPNG(c){return new Promise(function(ok,no){try{c.toBlob(function(b){if(b)ok(b);else no(new Error('The map could not be drawn as a picture.'));},'image/png');}catch(e){no(e);}});}
  /* cv as a PNG, drawn again at the viewport's CSS size (DPR 1) when it is over max. */
  function png(cv,vp,max){
    return canvasPNG(cv).then(function(b){
      if(b.size<=max||cv.width<=vp.w)return b;
      var c=document.createElement('canvas'),x;c.width=Math.max(1,vp.w);c.height=Math.max(1,vp.h);x=c.getContext('2d');
      x.imageSmoothingEnabled=false;x.drawImage(cv,0,0,c.width,c.height);
      return canvasPNG(c);
    });
  }
  /* The things the view frames, nearest its centre first: the hot rects inside the viewport at this level. */
  function items(f){
    var cam=f.cam,vis=WORLD.nearest(WORLD.cull(f.hots,WORLD.viewRect(cam,f.vp,0)),cam.cx,cam.cy,200);
    return vis.map(function(h){return snapItem(h,f.world,f.model);});
  }
  /* snapItem(h, world, model) -> {kind, id, title}: what one hot frames. A strip's bead is its model row; a field stand
     (28.12) is its gardener's bead, titled by its line ("<agent> — working: <task>"), as the barn's figures were. */
  function snapItem(h,world,model){
    var k=h.kind,b,it;
    if(k==='biome'||k==='sign'){b=world&&world.index[h.id];return{kind:k,id:b?(b.key===''?'~':b.key):'',title:b?b.name:''};}
    if(k==='bead'){it=model&&model.items[h.id];return it&&it.kind==='stand'?{kind:k,id:it.g.id,title:it.line||it.title}:{kind:k,id:h.id,title:it?it.title:''};}
    it=k==='plot'||k==='plotsign'?h.plot.plan:k==='task'?h.task:k==='tree'?h.it.plan:k==='flower'?h.it.task:k==='seed'?h.it.seed:k==='stone'?h.it.wish:null;
    return{kind:k,id:it?it.id:'',title:it?it.title:''};
  }
  /* wire shows btn when shown and returns snap(max), which the button, the S key and GARDEN.snap call. frame() is garden.js's
     state at the press, or null when the farm is not the view; toast(msg) tells the viewer why a snapshot did not go. */
  function wire(btn,shown,frame,toast,swBtn){
    var snapping=false,base=document.body.dataset.base||'';
    btn.hidden=!shown;
    if(swBtn)swBtn.hidden=!shown;
    function snap(max,o){
      o=o||{};
      var f=snapping?null:frame();
      if(!f)return Promise.resolve(null);
      snapping=true;btn.disabled=true;
      var cam=f.cam,loc=WORLD.locate(f.world,cam.cx,cam.cy,f.level);   // a home strip is its biome's (28.7)
      var ctx=snapContext({path:f.path,search:location.search,at:loc?loc.at:'',level:loc?f.level:null,who:f.who,night:f.night,items:items(f),vp:f.vp,now:new Date()});
      return Promise.all([png(f.cv,f.vp,max),o.to?Promise.resolve(o.to):landOf(base).then(function(l){return{land:l};})]).then(function(got){
        var to=got[1];
        return post(base,to,got[0],ctx,o.text||'',snapName(to.land,ctx.at,new Date(ctx.taken_at))).then(function(id){
          location.assign(o.steward?base+'/garden/c/s/'+encodeURIComponent(o.steward)+'?thread='+encodeURIComponent(id):base+'/garden/c/n/'+encodeURIComponent(id));
          return ctx;
        });
      }).catch(function(e){toast(e&&e.message||'The map could not be drawn as a picture.');if(e&&e.gone){btn.hidden=true;if(swBtn)swBtn.hidden=true;}return null;})
        .then(function(r){snapping=false;btn.disabled=false;return r;});
    }
    btn.onclick=function(){snap(MAX);};
    if(swBtn)swBtn.onclick=function(){steward(snap,base,toast);};
    return snap;
  }
  /* The land a snapshot goes to: the page's ?land=, else the viewer's steward's (GET /garden/c/s). */
  function landOf(base){
    var land=new URLSearchParams(location.search).get('land')||'';
    if(land)return Promise.resolve(land);
    return fetch(base+'/garden/c/s',{credentials:'same-origin',headers:{'X-Commons':'fragment'}}).then(function(r){
      if(!r.ok)throw new Error(r.status===404?'There is no land to send a snapshot to.':'The steward could not be found ('+r.status+').');
      return r.text();
    },function(){throw new Error('The garden could not be reached.');}).then(function(t){
      var n=new DOMParser().parseFromString(t,'text/html').querySelector('[data-kind=steward]'),to=n&&n.dataset.land;
      if(!to)throw new Error('The steward could not be found.');
      return to;
    });
  }
  /* To the steward: the snapshot into its land's #general with @steward in the words, then the steward's view on that thread. */
  function steward(snap,base,toast){
    return landOf(base).then(function(to){return snap(MAX,{to:{land:to,channel:'general'},text:'@steward ',steward:to});},function(e){toast(e.message);return null;});
  }
  return { MAX: MAX, png: png, items: items, snapItem: snapItem, wire: wire, steward: steward, snapContext: snapContext, snapName: snapName, post: post };
})();
if (typeof module !== "undefined") module.exports = SNAP;
