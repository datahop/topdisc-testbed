#!/usr/bin/env python3
"""Per topic-size class summary of a simnet run: recall, time to recall, load, settled search bucket."""
import json, sys, collections
import numpy as np
m = json.load(open(sys.argv[1]))
oh = json.load(open(sys.argv[2])) if len(sys.argv) > 2 else []
pt = {r["topic"]: r for r in m["perTopic"]}
import os
churn = None
cpath = os.path.join(os.path.dirname(os.path.abspath(sys.argv[1])), "churn.json")
if os.path.exists(cpath):
    d = json.load(open(cpath)); churn = (d.get("window", 0.0), {int(k): [(e["DownAt"], e["UpAt"]) for e in v] for k, v in (d.get("nodes") or {}).items()})
ad_life = ((m.get("deadResults") or {}).get("adLifetimeMs") or 900000) / 1000.0
def away_before(iv, t): return sum(max(0.0, min(u, t) - max(dn, 0.0)) for dn, u in iv)
short_lived = set()
if churn:
    for r in m["results"]:
        if r.get("nodeId") and r.get("nodeIdx") in churn[1] and churn[0] - away_before(churn[1][r["nodeIdx"]], churn[0]) < ad_life:
            short_lived.add(r["nodeId"][:16])
cov = m["registrationCoverage"]["byTopic"]
classes = [(1000, "1000+"), (100, "100-999"), (10, "10-99"), (0, "<10")]
def cls(t):
    n = pt[t]["target"]
    for lo, name in classes:
        if n >= lo: return name
res = collections.defaultdict(list)
for r in m["results"]: res[r["topic"]].append(r)
tmap = {h: int(i) for h, i in (m.get("topicIds") or {}).items()}
load = collections.defaultdict(lambda: collections.defaultdict(int))  # class -> node idx -> requests
for n in oh:
    for h, tl in (n.get("topicLoad") or {}).items():
        t = tmap.get(h)
        if t is None: continue
        c = cls(t)
        load[c][n.get("idx")] += (tl.get("topicQuery") or {}).get("rxMsgs", 0)
        load["all"][n.get("idx")] += (tl.get("topicQuery") or {}).get("rxMsgs", 0)
def tfrac(ts, need):
    return ts[need-1]/1000 if need > 0 and len(ts) >= need else None
if short_lived: print(f"{len(short_lived)} registrants online for under an ad lifetime are left out of the coverage column")
print("class | topics | registrants | searchers | median recall | searchers 100% | reg found by >=1 | median t50 (s) | t90 | t99 | t100 | median queries/searcher | TOPICQUERY received per registrar median / p99 / max | settled bucket (median over searchers)")
for lo, name in classes:
    ts = [t for t in pt if cls(t) == name]
    regs = sum(pt[t]["target"] for t in ts); srch = sum(len(res[t]) for t in ts)
    rec = []; full = 0; tt = {0.5: [], 0.9: [], 0.99: [], 1.0: []}; q = []; settled = []
    for t in ts:
        tg = max(pt[t]["target"], 1)
        for r in res[t]:
            u = r.get("uniqueFoundAtMs") or []
            f = len(u)/tg; rec.append(f); full += f >= 0.9999
            for frac in tt:
                v = tfrac(u, int(np.ceil(frac*tg)))
                if v is not None: tt[frac].append(v)
            st = r.get("searchStats") or {}
            q.append(st.get("queries", 0))
            tr = st.get("activeTrace") or []
            if tr:
                tr = sorted(tr, key=lambda p: p["atMs"]); last = tr[-1]["atMs"]
                tail = [p["bucket"] for p in tr if p["atMs"] >= 0.8*last] or [tr[-1]["bucket"]]
                settled.append(np.median(tail))
    found1 = total = 0
    for t in ts:
        if pt[t]["target"] == 0:
            continue  # single-member topic: nobody can find its registrant
        fan = set(k[:16] for k in cov.get(str(t), {}).get("byRegistrant", {})) - short_lived
        cnt = collections.Counter()
        for r in res[t]:
            for fid in (r.get("foundRegistrantIds") or r.get("foundIds") or []): cnt[fid[:16]] += 1
        total += len(fan); found1 += sum(1 for f in fan if cnt.get(f, 0) > 0)
    med = lambda l: f"{np.median(l):.0f}" if l else "-"
    lv = np.array([v for v in load[name].values() if v > 0]) if load[name] else np.array([])
    ls = f"{np.median(lv):.0f} / {np.percentile(lv,99):.0f} / {lv.max():.0f}" if lv.size else "-"
    print(f"{name} | {len(ts)} | {regs} | {srch} | {np.median(rec):.0%} | {100*full/max(len(rec),1):.0f}% | {100*found1/max(total,1):.1f}% | {med(tt[0.5])} | {med(tt[0.9])} | {med(tt[0.99])} | {med(tt[1.0])} | {med(q)} | {ls} | {med(settled) if settled else '-'}")
lv = np.array([v for v in load["all"].values() if v > 0]) if load["all"] else np.array([])
if lv.size: print(f"all topics: TOPICQUERY received per registrar median {np.median(lv):.0f} / p99 {np.percentile(lv,99):.0f} / max {lv.max():.0f} over {lv.size} nodes")
