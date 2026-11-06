/* The World drawn from a farm document and a camera (LW.3 gd-VEbAvsTNDe): no me, PLANS, Workbench, Commons or fetch, so a land
   window draws what /garden draws. garden.js keeps the hots and the input. */
var SCENE = (function () {
var T=WORLD.TILE,NB=NEIGHBOURS;
var CHUNK_LRU=64,BAKE_PER_FRAME=2;   // cached native chunks; chunks baked in one frame. A native bake blits ~1-2k tile sprites from the cache below (it was ~100k fills before the cache, design 6.4); measured under node at 2 per frame a settle frame stays near a third of the 24 ms bake budget, so a jump into a 35-plot biome sharpens in ~14 frames
/* scene(cv): a canvas and its caches. draw(o) paints a frame from o.cam, o.vp {w, h, dpr} and what lay() returns, with optional
   level, night, frame, reduce, coarse, mine (the Mine handle), safe (EDGES.safeView) and the page's hooks
   (keep, bcns, fill: WORLD.homeFill by biome id); true asks for another frame. */
function scene(cv){
var ctx=cv.getContext('2d'),chunkCache=WORLD.lru(CHUNK_LRU),bmps={};   // native chunks only: under World and Project the terrain is one flat grass fill
/* Tile sprites (grass variants, fence, soil) painted once per (map, palette, scheme) and blitted per tile by the native chunk
   bake; art.js keeps the cache DOM-free by taking the canvas factory from here. */
var sprites=ART.spriteCache(function(w,h){var c=document.createElement('canvas');c.width=w;c.height=h;return c;});
var op,vp,cam,level,night,frame,reduce,coarse,world,gspots,sst,edgeIx,weedIx,nbStyles,mine,again;
function queueRender(){again=true;}
/* One 512x512 bitmap of native terrain: the wild grass, and the ground, fences and beds of the biomes under it. */
function chunkBitmap(i,j){
  var key=(night?'n':'d')+i+','+j,c=chunkCache.get(key);
  if(c)return c;
  c=document.createElement('canvas');c.width=c.height=WORLD.CHUNK_PX;
  var x=c.getContext('2d'),span=WORLD.CHUNK_PX,n=span/WORLD.TILE,rect={x:i*span,y:j*span,w:span,h:span};
  x.translate(-i*span,-j*span);   // draw in world px; the chunk's own origin lands at the bitmap's corner
  // the biomes under this chunk; their ground paints over the wild grass, so that grass is skipped (a native bake is the costliest unit of frame work)
  var bs=world?world.biomes.filter(function(b){return WORLD.hit(WORLD.tiles(b.foot),rect);}):[];
  ART.drawWild(x,night,i*n,j*n,n,n,true,bs.length?function(tx,ty){for(var k=0;k<bs.length;k++)if(WORLD.inFoot(bs[k],tx,ty))return true;return false;}:null,sprites);
  bakeBiomes(x,rect,bs,i*n,j*n,n);
  chunkCache.set(key,c);return c;
}
/* Static biome terrain into a native chunk: ground in the biome palette over the footprint with the lanes in it (29.3), each
   plot's hedge (29.4) and soil beds, and the farmstead's base; no fence round the region (29.4). Only chunks under a biome whose
   footprint or lanes changed are rebuilt (garden.js's biomeSig). */
function bakeBiomes(x,rect,bs,tx0,ty0,n){
  var pal=ART.palette(night);
  bs.forEach(function(b){
    var inside=function(tx,ty){return WORLD.inFoot(b,tx,ty);},pi=WORLD.plotInside(b,rect),ls=b.lanes||[],bp=ART.biomePalette(night,b.palette);
    var lane=function(tx,ty){for(var k=0;k<ls.length;k++){var l=ls[k];if(tx>=l.x&&ty>=l.y&&tx<l.x+l.w&&ty<l.y+l.h)return true;}return false;};
    ART.drawBiomeGround(x,night,b.palette,tx0,ty0,n,n,inside,sprites,function(tx,ty){return lane(tx,ty)||!!(pi&&pi(tx,ty));});   // no grass under a lane, nor a plot's paths and beds
    ART.drawLanes(x,bp,ls,b.home,tx0,ty0,n,inside,sprites);
    b.plots.forEach(function(p){
      if(!WORLD.hit(WORLD.tiles(p),rect))return;
      ART.drawPlotBase(x,pal,p.x*T,p.y*T,p.beds.map(function(bd){return {x:bd.x*T,y:bd.y*T,w:bd.w*T,h:bd.h*T};}),sprites,!!p.staked,bp);   // hedged in the biome's dark green; a staked plot: stakes and string, pale turned soil
    });
    if(WORLD.hit(WORLD.tiles(b.home),rect)){var l=homeLook(b);l.part='base';ART.drawHome(x,b.home,l,bp);}   // its farmstead's still base (28.8, 29.7)
  });
}
/* Drop the native chunks under a rect (world px). */
function dropChunks(rect){
  var c=chunkCache,s=WORLD.CHUNK_PX;
  c.keys().forEach(function(k){var ij=k.slice(1).split(','),i=+ij[0],j=+ij[1];if(WORLD.hit({x:i*s,y:j*s,w:s,h:s},rect))c.del(k);});
}
/* A native chunk that is not baked yet: flat grass, tinted flat where a biome footprint crosses it, so the view reads right
   until the next frame bakes it. */
function drawCoarse(ch,span,x0,y0,w,h){
  ctx.fillStyle=ART.palette(night).g;ctx.fillRect(x0,y0,w,h);
  if(!world)return;
  var rect={x:ch.i*span,y:ch.j*span,w:span,h:span},d=vp.dpr,z=cam.z;
  world.biomes.forEach(function(b){
    var f=WORLD.tiles(b.foot);if(!WORLD.hit(f,rect))return;
    var ix=Math.max(f.x,rect.x),iy=Math.max(f.y,rect.y),ix1=Math.min(f.x+f.w,rect.x+rect.w),iy1=Math.min(f.y+f.h,rect.y+rect.h);
    ctx.fillStyle=ART.biomePalette(night,b.palette).g;
    ctx.fillRect(Math.round(((ix-cam.cx)*z+vp.w/2)*d),Math.round(((iy-cam.cy)*z+vp.h/2)*d),Math.round((ix1-ix)*z*d),Math.round((iy1-iy)*z*d));
  });
}
/* Draw the native chunks under the view. They bake at most BAKE_PER_FRAME per frame; the rest draw coarse and the frame asks
   for another, so a jump into a dense biome sharpens over a few frames instead of stalling one. A view wider than the cache (a
   4K screen at the Plan level's far end) keeps its uncached chunks coarse rather than thrashing the LRU every frame. */
function drawChunks(view){
  var d=vp.dpr,span=WORLD.CHUNK_PX,z=cam.z,list=WORLD.chunks(view,1),budget=list.length>CHUNK_LRU?0:BAKE_PER_FRAME,more=false;
  list.forEach(function(ch){
    // integer device edges, shared by neighbours, so no hairline seams appear between chunks
    var x0=Math.round(((ch.i*span-cam.cx)*z+vp.w/2)*d),x1=Math.round((((ch.i+1)*span-cam.cx)*z+vp.w/2)*d);
    var y0=Math.round(((ch.j*span-cam.cy)*z+vp.h/2)*d),y1=Math.round((((ch.j+1)*span-cam.cy)*z+vp.h/2)*d);
    var c=chunkCache.get((night?'n':'d')+ch.i+','+ch.j);
    if(!c){
      if(budget>0){budget--;c=chunkBitmap(ch.i,ch.j);}
      else{drawCoarse(ch,span,x0,y0,x1-x0,y1-y0);if(list.length<=CHUNK_LRU)more=true;return;}
    }
    ctx.drawImage(c,x0,y0,x1-x0,y1-y0);
  });
  if(more)queueRender();
}
function draw(o){
  op=o;vp=o.vp;cam=o.cam;level=o.level==null?WORLD.lodFor(cam.z):o.level;night=!!o.night;frame=o.frame||0;reduce=!!o.reduce;coarse=!!o.coarse;
  world=o.world;gspots=o.gs;sst=o.sst;edgeIx=o.edgeIx;weedIx=o.weedIx;nbStyles=o.nb||{};mine=o.mine||'';again=false;
  var z=cam.z,v=WORLD.viewRect(cam,vp,0);
  ctx.setTransform(1,0,0,1,0,0);
  ctx.imageSmoothingEnabled=z<1;     // pixel art stays crisp at and above native size; below it the terrain is downsampled
  var vis=WORLD.visible(world,v,Math.max(level,1));   // the World level draws what the Project level draws (4b.2)
  /* World and Project: flat grass under the biome blobs, one fill (the coarse mips' chunks were the same grass, blitted scaled
     under blobs that cover most of the view: ~1 screen of scaled bitmaps a frame, gd-fsuxCGGbZr) */
  homeCheck(vis.biomes);
  if(level<=1){ctx.fillStyle=ART.palette(night).g;ctx.fillRect(0,0,cv.width,cv.height);drawBlobs(vis);}else drawChunks(v);
  drawWorldLayer(vis);
  worldTransform();if(ANIM.draw(ctx,{level:level,night:night,sp:sprites,cam:cam,vp:vp}))queueRender();ctx.setTransform(1,0,0,1,0,0);   // a running transition repaints every frame
  BIRDS.words(ctx);   // the birds' tool words over the figures, their bubbles and the transitions (web/birds.js, 4n)
  return again;
}
/* A world-px drawing transform: everything below draws in world px and the canvas scales it. */
function worldTransform(){var d=vp.dpr,z=cam.z;ctx.setTransform(z*d,0,0,z*d,(vp.w/2-cam.cx*z)*d,(vp.h/2-cam.cy*z)*d);}
/* World and Project levels: each biome is a filled blob in its palette (the native ground is under a pixel there), its lanes
   crossing it flat in the path colour out to the blob's rim (29.6, 29.7): the overview's lines, at least a pixel wide. */
function drawBlobs(vis){
  worldTransform();
  vis.biomes.forEach(function(b){
    var bp=ART.biomePalette(night,b.palette),f=b.foot;
    ctx.fillStyle=bp.d;ctx.fillRect((f.x+1)*T,(f.y+1)*T,(f.w-2)*T,(f.h-2)*T);
    ctx.fillStyle=bp.g;ctx.fillRect((f.x+2)*T,(f.y+2)*T,(f.w-4)*T,(f.h-4)*T);
    ctx.fillStyle=bp.P;(b.lanes||[]).forEach(function(l){var x0=Math.max(l.x,f.x+1),y0=Math.max(l.y,f.y+1),x1=Math.min(l.x+l.w,f.x+f.w-1),y1=Math.min(l.y+l.h,f.y+f.h-1),m=1/(cam.z*vp.dpr);if(x1>x0&&y1>y0)ctx.fillRect(x0*T,y0*T,Math.max((x1-x0)*T,m),Math.max((y1-y0)*T,m));});
  });
  ctx.setTransform(1,0,0,1,0,0);
}
/* A small bitmap cache for sprites drawn many times per frame (plants, pots, flowers, trees). */
function bmp(key,w,h,paint){
  var k=(night?'n':'d')+key,c=bmps[k];
  if(c)return c;
  c=document.createElement('canvas');c.width=w;c.height=h;paint(c.getContext('2d'),ART.palette(night));
  return bmps[k]=c;
}
/* Plot p's arms at its gate (web/heraldry.js), their box into gates for the badges to keep off. */
function drawGate(p,gates){var gb=HERALDRY.drawPlot(ctx,ART.drawArms,p,T,WORLD.BED.off[0],cam.z,vp.dpr,night);if(gb)gates.push(gb);}
/* A farmstead (homestead-world-design.md 28.8, 29.7): bakeBiomes paints its base (homeLook) with the terrain, homeCheck drops the
   chunks under one whose look or cell rects (a re-shape, 29.10) changed; drawStrip paints it flat at Project, and its live part from Plan, moving at Bead. */
var homeSig={};
function homeLook(b){var fill=op.fill&&op.fill[b.id],ez=b.home.zones.escalation;   // the farm's letters count its escalations too: they are the Escalations cell's (28.13)
  return {seed:b.seed,sp:sprites,night:night,fill:fill,release:TRUCK.has(b.key),letters:(b.home.counts.letters|0)-ez.total>0||!!(fill&&fill.mailbox&&fill.mailbox.items.length),asks:ez.items.some(function(it){return it.wait.human;})};}
function homeCheck(bs){bs.forEach(function(b){var l=homeLook(b),c=b.home.cells,k=(l.release?1:0)+','+(l.letters?1:0)+','+(l.asks?1:0)+Object.keys(c).map(function(z){return ','+c[z].x+':'+c[z].w;}).join('');if(homeSig[b.id]!==undefined&&homeSig[b.id]!==k)dropChunks(WORLD.tiles(b.home));homeSig[b.id]=k;});}
function drawStrip(b,f,view){
  if(!WORLD.hit(WORLD.tiles(b.home),view))return;
  var l=homeLook(b),bp=ART.biomePalette(night,b.palette);
  if(level===1){l.part='base';l.flat=true;}else{l.part='live';l.frame=level===3?f:0;}
  ART.drawHome(ctx,b.home,l,bp);
  var gr=level>=2&&l.fill?l.fill.growing:null;   // each field figure's task arms on its hat, in the 32 px rows' gap (5.9, 28.12, gd-7UDNpFoHWw); not over a bubble
  if(gr)gr.items.forEach(function(h){var it=h.item,g=h.kind==='stand'&&!it.blocked&&it.g,a=g&&!/^(waiting|crashed)$/.test(g.state)&&g.task_arms;if(a)HERALDRY.badge(ctx,ART.drawArms,a.canonical,(h.x+h.w-1)*T,h.y*T+4,cam.z,vp.dpr,night,false);});
}
/* A plot's rollup stamp (cursor-grid spec 6.6: the phase cells and the bar, over its plate and operator fence), baked into a small
   canvas in screen px at the device scale and blitted unsmoothed, so a still frame at the project depth is one drawImage per plot.
   Kept per plot id; a new layout (new data), size or theme bakes it again, into the same canvas. */
var ROLLUP_CAP=96,rollups=WORLD.lru(ROLLUP_CAP);
function drawRollup(p,ro){
  var d=vp.dpr,side=p.w*T*cam.z,n=Math.ceil(side*d),key=n+'|'+(night?1:0),e=rollups.get(p.id);
  if(!e||e.p!==p||e.key!==key){
    var c=e?e.cv:document.createElement('canvas');c.width=c.height=n;
    var x=c.getContext('2d');x.setTransform(d,0,0,d,0,0);x.clearRect(0,0,n,n);ART.drawRollupStamp(x,ART.palette(night),0,0,side,1,ro);
    rollups.set(p.id,e={p:p,key:key,cv:c});
  }
  var w=n/d/cam.z,sm=ctx.imageSmoothingEnabled;ctx.imageSmoothingEnabled=false;ctx.drawImage(e.cv,p.x*T,p.y*T,w,w);ctx.imageSmoothingEnabled=sm;
}
/* A ring that says "needs you": gold on dark, a constant screen width, pulsing by frame unless motion is reduced (no caller
   pulses it now: at World and Project the stamps' "!N" and heads and the boards say it, gd-GgKjBgsc0U). */
function drawRing(r,pulse){
  var z=cam.z,t=3/z,o=(pulse&&!reduce&&frame%2)?2/z:0,x=r.x*T-o-t*2,y=r.y*T-o-t*2,w=r.w*T+2*(o+t*2),h=r.h*T+2*(o+t*2);
  ctx.fillStyle='#2a1a14';ctx.fillRect(x,y,w,t*2);ctx.fillRect(x,y+h-t*2,w,t*2);ctx.fillRect(x,y,t*2,h);ctx.fillRect(x+w-t*2,y,t*2,h);
  ctx.fillStyle='#ffe680';ctx.fillRect(x+t/2,y+t/2,w-t,t);ctx.fillRect(x+t/2,y+h-t*1.5,w-t,t);ctx.fillRect(x+t/2,y+t/2,t,h-t);ctx.fillRect(x+w-t*1.5,y+t/2,t,h-t);
}
/* A plot at the World level when the stamps would not fit their cache: a block in its crop's colour, the share of its steps
   closed filled along its foot, and a still ring when it needs you (the pulse went at World and Project, gd-GgKjBgsc0U); the
   project boards carry the rest. */
function drawPlotMark(pal,p,now){
  var t=WORLD.tile(p.plan,now),x=p.x*T,y=p.y*T,w=p.w*T,h=p.h*T,bh=Math.min(h/3,5/cam.z);
  ctx.fillStyle=pal[ART.CROP_COL[t.crop]||'m'];ctx.fillRect(x,y,w,h);
  if(t.pct){ctx.fillStyle=pal.l;ctx.fillRect(x,y+h-bh,w*t.pct/100,bh);}
  if(p.needs>0)drawRing(p,false);
}
/* The dynamic layer (design section 6): signs and rings at every level; plots as rollup stamps (World and Project: owner
   decision gd-H6ZPz5V262 dropped the World level's plotless blobs for the Project stamps, under the project boards), flat beds
   with state squares (Plan) or sprites (Bead); band items as dots (Plan) or sprites (Bead). Redrawn with the camera and on data. */
/* The band items and the pen's goats in view a gardener may stand beside (WORLD.standItem): what drawPlotGardeners draws after the plots. */
function standsInView(vis){var v=WORLD.viewRect(cam,vp,0),goats=[];vis.biomes.forEach(function(b){if(b.pen)b.pen.items.forEach(function(it){if(WORLD.hit(WORLD.tiles(it),v))goats.push(it);});});return vis.items.concat(goats);}
function drawWorldLayer(vis){
  if(!world)return;   // with no biome in view (5b) the view cut and the barn's birds still run; the rest draws nothing
  worldTransform();
  var pal=ART.palette(night),now=Date.now()/1000,f=reduce?0:frame,view=WORLD.viewRect(cam,vp,0),edges=function(){EDGES.draw(ctx,edgeIx,{level:level,z:cam.z,T:T,night:night,view:WORLD.viewRect(cam,vp,0)});};
  if(level>0)edges();   // irrigation channels between regions, under the signs and plots (web/edges.js); at the World level a channel joins two regions' middles, so it goes over their plots
  vis.biomes.forEach(function(b){
    if(level>=1){
      drawStrip(b,f,view);
      ART.drawSign(ctx,pal,b.sign.x*T,b.sign.y*T,b.sign.w*T);
    }
    if(level===0&&mine&&!NB.biomeMine(b,mine))NB.dim(ctx,WORLD.tiles(b.foot));
  });
  var crowd=level===0&&vis.plots.length>ROLLUP_CAP,gates=[];   // more stamps in view than their cache holds (a farm of hundreds of plans)
  vis.plots.forEach(function(p){
    var gp=gspots&&gspots[3].plots[p.id],lk=gp&&level<=1?gp.ids.map(function(id){return gspots.look[id];}):0;
    if(crowd){drawPlotMark(pal,p,now);if(lk)ART.drawRollupMarks(ctx,pal,p.x*T,p.y*T,p.w*T*cam.z,1/cam.z,WORLD.plotHeads(lk));return;}
    var t=WORLD.tile(p.plan,now),ro=level<=1?WORLD.plotRollup(p,lk):null;
    if(level>=3)EDGES.drawField(ctx,edgeIx,p.id,{T:T,night:night,frame:f});   // a step's blockers as narrow channels (gd-Y4gXDBblIG), under the plants at Bead (gd-hX9BRyBq1G)
    if(level<=1)ART.drawRollupPlate(ctx,pal,p.x*T,p.y*T,p.w*T*cam.z,1/cam.z,t.crop);   // the rollup stamp's plate (cursor-grid spec 6.6); the stamp goes over the fence
    else if(level===2){
      var cells=[];p.beds.forEach(function(bd){bd.cells.forEach(function(c){if(c.task.state!=='closed')cells.push({x:c.x*T,y:c.y*T,state:c.task.state,type:c.task.type});});});   // a closed step is bare at this level, like the stubble
      ART.drawPlotFlat(ctx,pal,p.x*T,p.y*T,t.crop,p.beds.map(function(bd){return {x:bd.x*T,y:bd.y*T,w:bd.w*T,h:bd.h*T};}),cells);
    }else if(p.staked){
      p.beds.forEach(function(bd){bd.cells.forEach(function(c){ctx.drawImage(bmp('stepstake',16,16,function(x,pl){ART.drawStepStake(x,pl,0,0,false);}),c.x*T,c.y*T);});});   // a stake marker per step, where a plant will grow
    }else{
      p.beds.forEach(function(bd){
        var stub=function(){return bmp('stub',16,16,function(x,pl){ART.drawBead(x,pl,{t:'task',s:'done',wild:true},0,0,0);});};
        bd.cells.forEach(function(c){if(c.task.state==='closed'){var hb=ART.harvestBead(c.task,cam.z>=2&&!coarse),hz=hb.wide?32:16;ctx.drawImage(bmp('h|'+hb.key,hz,hz,function(x,pl){ART.drawHarvest(x,pl,hb,0,0);}),c.x*T,c.y*T,T,T);return;}var b=ART.taskBead(c.task,now),key='b|'+ART.beadKey(b,f);ctx.drawImage(bmp(key,16,16,function(x,pl){ART.drawBead(x,pl,b,0,0,f);}),c.x*T,c.y*T);});   // a listed closed step is a harvest cell: its base and the badges it has room for (harvest spec 4)
        for(var i=0;i<bd.stubble;i++){var sa=WORLD.cellAt(bd.stubbleAt+i);ctx.drawImage(stub(),(bd.x+sa.c)*T,(bd.y+sa.r)*T);}(bd.gone||[]).forEach(function(gi){var ga=WORLD.cellAt(gi);ctx.drawImage(stub(),(bd.x+ga.c)*T,(bd.y+ga.r)*T);});if(bd.crate)ctx.drawImage(bmp('crate',16,16,function(x,pl){ART.drawBead(x,pl,{t:'task',s:'open',wild:true,orphan:true},0,0,0);}),bd.crate.x*T,bd.crate.y*T);
      });
    }
    if(level===2)EDGES.drawField(ctx,edgeIx,p.id,{T:T,night:night,frame:f});   // at Plan over the beds
    if(level>=2)ART.drawPlotMarks(ctx,pal,p,sprites);   // the beds' stakes and the "+N phases" crate (cursor-grid spec 5.3)
    var pr={x:p.x*T,y:p.y*T,w:p.w*T,h:p.h*T};if(!p.staked)NB.fence(ctx,pr,NB.styleOf(nbStyles,p.plan),cam.z,T);if(level>=2)drawGate(p,gates);if(ro)drawRollup(p,ro);if(mine&&!NB.mine(p.plan,mine))NB.dim(ctx,pr);if(p.needs>0&&level===3)ART.drawBubble(ctx,pal,p.x*T+p.w*T/2-7,p.y*T-14,f,1,'?');   // a staked plot's string is its fence until it is planted
    if(p.needs>0&&level===2)drawRing(p,false);   // at Project the rollup's "!N" and heads say it (gd-GgKjBgsc0U)
    if(ro)ART.drawRollupMarks(ctx,pal,p.x*T,p.y*T,p.w*T*cam.z,1/cam.z,ro);   // "!N" and the gardeners' heads ride the fence and the ring
    if(level>=1&&TRUCK.shipped(p.plan))ART.drawShippedMark(ctx,pal,p.x*T,p.y*T,p.w*T*cam.z,1/cam.z);   // its newest merged PR shipped in a release (web/truck.js)
  });
  if(level<2&&op.gate)vis.plots.forEach(function(p){if(p.id===op.gate)drawGate(p,gates);});   // the cursor's plot keeps its arms at Project art, where a short map frames a plan (gd-rA31ZIfHdv): over the stamp and the plots around it
  if(level===0)edges();
  if(TRUCK.draw(ctx,{level:level,night:night,view:WORLD.viewRect(cam,vp,0),world:world,reduce:reduce}))queueRender();   // each project's release truck at its strip's bay, loading, or driving out as a release goes live (web/truck.js)
  GOATS.draw(ctx,{level:level,z:cam.z,night:night,view:WORLD.viewRect(cam,vp,0),world:world,gs:gspots,reduce:reduce,sp:sprites});   // open wishes as goats (web/goats.js), under the birds and the figures, grazing clear of both; the pen's fence from Project in
  if(level>=2&&gspots&&BIRDS.draw(ctx,{level:level,z:cam.z,night:night,view:WORLD.viewRect(cam,vp,0),gs:gspots,world:world,reduce:reduce,moving:ANIM.moving,vp:vp,keep:op.keep&&op.keep(),bcns:op.bcns}))queueRender();   // tool calls as birds (web/birds.js), under the figures, their plates placed clear of the sign's tip and the beacons (drawn by BIRDS.words in render); a figure at home flies them from its own barn (5b, WORLD.homeTile)
  var stands=level>=2&&gspots?standsInView(vis):[],badge=function(id,x,y,lift){var g=gspots.byId[id],a=g&&g.task_arms;if(a)HERALDRY.badge(ctx,ART.drawArms,a.canonical,x,y,cam.z,vp.dpr,night,lift,gates);};   // task arms over each head (5.9)
  if(level>=2&&gspots)vis.plots.concat(stands).forEach(function(p){var gl=gspots[level],pl=gl.plots[p.id];(p.beds||[]).forEach(function(bd){HERALDRY.shun(ctx,gates,bd.x*T-2,bd.y*T-2,bd.w*T+4,bd.h*T+4,1);if(!bd.other)HERALDRY.shun(ctx,gates,(bd.x-1)*T+1,(bd.y-1)*T,14,T,2);});if(pl)pl.shown.forEach(function(id){var s=gl.spots[id];HERALDRY.shun(ctx,gates,s.x+1,s.y+2,14,22,1);});});   // the badges keep clear of the beds (and the cursor ring round one), their signs and the figures, beside an item too (gd-M0IjVGTzSP)
  if(level>=2&&gspots)ART.drawPlotGardeners(ctx,pal,gspots,level,vis.plots,level===3?f:0,night,sprites,ANIM.hides,badge);   // still at Plan, the hoe swings at Bead
  if(level>=2&&sst&&sst.list.length)ART.drawStewards(ctx,sst.list,level===3?f:0,night,sprites);   // each land's steward by its biome's sign (Lands 9.8)
  if(level>=2&&gspots&&gspots.esc.list.length)ART.drawStewards(ctx,gspots.esc.list,level===3?f:0,night,sprites);   // each escalation's executor beside its icon (WORLD.escStands), never hidden by its ride: it does not walk
  vis.items.forEach(function(it){
    if(level<=2){ART.drawDot(ctx,pal,it.kind,it.x*T+(it.w-1)*T/2,it.y*T+(it.h-1)*T/2,level<=1?16:6);return;}
    if(it.kind==='tree')ctx.drawImage(bmp('tree',16,16,function(x,pl){ART.drawMiniTree(x,pl,0,0);}),it.x*T,it.y*T);
    else if(it.kind==='stone'){var fw=it.wish.outcome==='done';ctx.drawImage(bmp('stone'+(fw?1:0),16,16,function(x,pl){ART.drawHeadstone(x,pl,0,0,fw);}),it.x*T,it.y*T);}   // a closed wish's headstone (gd-s9KzRk6boh): a flower when done
    else if(it.kind==='flower'){var v=WORLD.h32(it.id,5)%4;ctx.drawImage(bmp('fl'+v,16,16,function(x,pl){ART.drawFlower(x,pl,0,0,v);}),it.x*T,it.y*T);}
    else{var fr=it.seed.state==='frosted';ctx.drawImage(bmp('pot'+(fr?1:0),48,48,function(x,pl){ART.drawPot(x,pl,0,0,fr);}),it.x*T,it.y*T);}
  });
  if(stands.length)ART.drawPlotGardeners(ctx,pal,gspots,level,stands,level===3?f:0,night,sprites,ANIM.hides,badge);   // beside their wildflower, pot, tree, headstone or goat (gd-6FCoWelnx2), over the items, so a flower never paints over a figure
  vis.overflow.forEach(function(o){
    if(level>=2)ART.drawOverflow(ctx,pal,o.x*T,o.y*T);
    else{ctx.fillStyle=pal.k;ctx.fillRect(o.x*T,o.y*T,o.w*T,o.h*T);ctx.fillStyle=pal.c;ctx.fillRect(o.x*T+T/2,o.y*T+T/2,o.w*T-T,o.h*T-T);}
  });
  WEEDS.draw(ctx,{ix:weedIx,vis:vis,level:level,z:cam.z,pal:pal,T:T,night:night,bmp:bmp});   // thistles and fog (web/weeds.js)
  EDGES.drawNames(ctx,edgeIx,{level:level,z:cam.z,T:T,night:night,dpr:vp.dpr,view:WORLD.viewRect(cam,vp,0),safe:op.safe});   // a bridge's name over everything, the fog too, slid along its channel onto the map the HUD leaves (web/edges.js)
  ctx.setTransform(1,0,0,1,0,0);
}
return {draw:draw,drop:dropChunks};
}
/* A farm document laid out for draw, as garden.js keeps it. */
function lay(doc,memo){var w=WORLD.layout(doc,memo||null,{readonly:true}),gs=WORLD.gardenerState(w,doc.gardeners);return {world:w,gs:gs,sst:WORLD.stewardState(w,doc.cost,doc.gardeners,gs),edgeIx:EDGES.index(doc,w),weedIx:WEEDS.index(WEEDS.lint(doc,Date.now()/1000),w),nb:NB.assign(doc)};}
return {scene:scene,lay:lay};
})();
if(typeof module!=='undefined')module.exports=SCENE;
