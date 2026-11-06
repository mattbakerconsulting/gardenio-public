import json,subprocess,sys
IDS=[ # (id, category)
 ("gd-pQZO9ie11D","plan"),("gd-2RdeAx8nnW","plan"),("gd-UZKZ9H25UD","plan"),("gd-ZsV0W4qKLy","plan"),
 ("gd-EJGA3zDGCk","plan"),("gd-N6I3Oa7uDg","plan"),("gd-X07JyIHEzl","plan"),
 ("gd-jGv3zMSMUk","phase"),("gd-ucrYuPOljl","phase"),("gd-efsF2kfrGk","phase"),("gd-T3dRz02upp","phase"),("gd-mnDfHWSfHR","phase"),("gd-sA2jOarxCh","phase"),
 ("gd-RcmmFKWytW","task (inherits)"),("gd-53IHFkfPLk","task (inherits)"),("gd-veuI2XnkA1","task (inherits)"),("gd-6vGD02rgka","task (inherits)"),("gd-kVKuWqIK1v","task (inherits)"),
 ("gd-eThga7vg3I","decision"),("gd-Ui9jl00KfA","project"),
]
cache={}
def show(i):
  if i not in cache: cache[i]=json.loads(subprocess.check_output(["kd","show",i,"--json"]))
  return cache[i]
out=[]
for i,cat in IDS:
  b=show(i); cur=b; arms=""; frm=""; seen=set()
  while cur and cur["id"] not in seen:
    seen.add(cur["id"])
    h=(cur.get("fields") or {}).get("heraldry") or cur.get("heraldry") or ""
    if h: arms=h; frm=cur["id"]; break
    par=[d["depends_on_id"] for d in (cur.get("dependencies") or []) if d["type"]=="parent-child"]
    cur=show(par[0]) if par else None
  out.append(dict(id=b["id"],category=cat,type=b["type"],title=b["title"],short_title=b.get("short_title",""),prose=b.get("description","") or "",arms=arms,arms_from=frm))
  print(b["id"],cat,repr(arms),frm,"|",b.get("short_title",""),file=sys.stderr)
json.dump(out,open(sys.argv[1],"w"),ensure_ascii=False,indent=1)
