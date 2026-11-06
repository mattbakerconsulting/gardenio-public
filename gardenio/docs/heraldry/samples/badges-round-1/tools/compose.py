import json,sys,os
from PIL import Image,ImageDraw,ImageFont
D=sys.argv[1]; beads=json.load(open(f"{D}/beads.json")); res={(r["ID"],r["Model"],r["Style"]):r for r in json.load(open(f"{D}/results.json"))}
F="/usr/share/fonts/truetype/noto/"
reg=lambda s:ImageFont.truetype(F+"NotoSans-Regular.ttf",s); bold=lambda s:ImageFont.truetype(F+"NotoSans-Bold.ttf",s)
emo=ImageFont.truetype(F+"NotoColorEmoji.ttf",109)
COLS=[("sonnet","line-art-bold","Sonnet 5.5\nbold"),("opus","line-art-bold","Opus 5.5\nbold"),("sonnet","poster","Sonnet 5.5\nposter"),("opus","poster","Opus 5.5\nposter")]
BG=(246,244,239); INK=(34,34,34); MUTED=(110,110,110); RED=(180,40,40)
def run(arms):
  t=[x for x in arms.split() if x not in(".",)]
  if t and t[0].isascii(): t=t[1:]
  return t
def emoji_strip(chars,h):
  ims=[]
  for c in chars:
    im=Image.new("RGBA",(160,136),(0,0,0,0)); ImageDraw.Draw(im).text((0,0),c,font=emo,embedded_color=True)
    im=im.crop(im.getbbox() or (0,0,1,1)); r=h/im.height; ims.append(im.resize((max(1,int(im.width*r)),h),Image.LANCZOS))
  return ims
def wrap(d,text,font,w):
  words=text.split(); lines=[]; cur=""
  for x in words:
    t=(cur+" "+x).strip()
    if d.textlength(t,font=font)<=w: cur=t
    else: lines.append(cur); cur=x
  if cur: lines.append(cur)
  return lines
def page(rows,size,out,title):
  pad=max(16,size//8); lw=380 if size<=128 else 560; colw=size+pad; head=130
  rowh=size+pad+(40 if size<=128 else 20)
  W=lw+len(COLS)*colw+pad; H=head+len(rows)*rowh+pad
  im=Image.new("RGB",(W,H),BG); d=ImageDraw.Draw(im)
  d.text((pad,18),title,font=bold(30),fill=INK)
  for j,(_,_,lab) in enumerate(COLS): d.multiline_text((lw+j*colw,58),lab,font=bold(18 if size<=128 else 26),fill=INK)
  fs=17 if size<=128 else 26
  for i,b in enumerate(rows):
    y=head+i*rowh
    ch=run(b["arms"]); yy=y
    d.text((pad,yy),f'{b["category"]} · {b["id"]}',font=reg(fs-2),fill=MUTED); yy+=fs+8
    if ch:
      x=pad
      for e in emoji_strip(ch,fs+14): im.paste(e,(x,yy),e); x+=e.width+6
      if b["arms_from"]!=b["id"]: d.text((x+6,yy+4),f'from {b["arms_from"]}',font=reg(fs-3),fill=MUTED)
      yy+=fs+22
    else: d.text((pad,yy),"no arms in its line",font=reg(fs-2),fill=MUTED); yy+=fs+14
    for ln in wrap(d,b["short_title"] or b["title"][:60],bold(fs),lw-2*pad)[:3]: d.text((pad,yy),ln,font=bold(fs),fill=INK); yy+=fs+6
    for j,(m,s,_) in enumerate(COLS):
      x=lw+j*colw; p=f'{D}/png/{m}/{s}/{b["id"]}-{size}.png'
      if os.path.exists(p): t=Image.open(p).convert("RGBA"); im.paste(t,(x,y),t)
      r=res.get((b["id"],m,s))
      if r and r["Tier"]!="tier2": d.text((x,y+size+4),"fell back to Tier-0",font=reg(14 if size<=128 else 20),fill=RED)
  im.save(out,optimize=True); print(out,im.size,os.path.getsize(out)//1024,"KB")
os.makedirs(f"{D}/pages",exist_ok=True)
page(beads[:10],128,f"{D}/pages/badges-r1-p1-128.png","Badges round 1 · 128 px · beads 1–10")
page(beads[10:],128,f"{D}/pages/badges-r1-p2-128.png","Badges round 1 · 128 px · beads 11–20")
for k in range(4):
  page(beads[k*5:(k+1)*5],512,f"{D}/pages/badges-r1-p{k+3}-512.png",f"Badges round 1 · 512 px · beads {k*5+1}–{k*5+5}")
