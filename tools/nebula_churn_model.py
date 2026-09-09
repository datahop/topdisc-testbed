#!/usr/bin/env python3
"""Derive a churn model for the testbed from Nebula discv5 crawls.

Endpoint via NEBULA_CH_URL / NEBULA_CH_USER env; password on stdin. Read-only. Output: a model JSON with the age-conditioned
survival curve, the gap (unavailable-time) distribution, the always-on share
and the crawl cadence — no source details.

  echo -n '<password>' | python3 nebula_churn_model.py check
  echo -n '<password>' | python3 nebula_churn_model.py extract 2026-08-01 2026-09-01 model.json
"""
import json, os, sys, urllib.request, collections, math, ssl
try:
    import certifi; CTX=ssl.create_default_context(cafile=certifi.where())
except ImportError:
    CTX=ssl.create_default_context(cafile='/etc/ssl/cert.pem')
HOST=os.environ["NEBULA_CH_URL"]; USER=os.environ["NEBULA_CH_USER"]; DB=os.environ.get("NEBULA_CH_DB","nebula_discv5")
PW=sys.stdin.read().strip()

def q(sql, fmt="TSVWithNames"):
    req=urllib.request.Request(HOST+"/?database="+DB, data=(sql+f" FORMAT {fmt}").encode(),
        headers={"X-ClickHouse-User":USER,"X-ClickHouse-Key":PW})
    try:
        with urllib.request.urlopen(req, timeout=600, context=CTX) as r:
            return r.read().decode()
    except urllib.error.HTTPError as e:
        sys.exit("clickhouse error: " + e.read().decode()[:400])

def check():
        print(q("SELECT network_id, toStartOfMonth(created_at) m, count() crawls, min(created_at), max(created_at) FROM crawls WHERE state='succeeded' AND created_at>='2026-06-01' GROUP BY network_id, m ORDER BY network_id, m"))

def extract(frm, to, out):
    # one row per (peer, crawl) where the peer answered FINDNODE; crawl index = rank of crawl start
    NET="ETHEREUM_CONSENSUS"
    crawls=q(f"SELECT id, created_at FROM crawls WHERE state='succeeded' AND network_id='{NET}' AND created_at>='{frm}' AND created_at<'{to}' ORDER BY created_at").splitlines()[1:]
    times=[c.split('\t')[1] for c in crawls]; n=len(crawls)
    print(f"crawls: {n} from {times[0]} to {times[-1]}", file=sys.stderr)
    # crawl -> 0-based index inside ClickHouse; per peer, the sorted set of indices it answered in
    rows=q(f"""WITH c AS (SELECT id, rowNumberInAllBlocks() AS idx FROM (SELECT id FROM crawls
                  WHERE state='succeeded' AND network_id='{NET}' AND created_at>='{frm}' AND created_at<'{to}' ORDER BY created_at))
               SELECT v.peer_id, arraySort(groupUniqArray(c.idx)) AS present
               FROM visits v INNER JOIN c ON v.crawl_id = c.id
               WHERE v.visit_started_at>='{frm}' AND v.visit_started_at<'{to}' AND v.crawl_error IS NULL
               GROUP BY v.peer_id""").splitlines()[1:]
    print(f"peers with >=1 responsive visit: {len(rows)}", file=sys.stderr)
    sessions=[]; censored=[]; gaps=[]; always=0; peers=0
    for r in rows:
        pid,cs=r.split('\t'); present=json.loads(cs)
        if not present: continue
        peers+=1
        if len(present)==n: always+=1; continue
        runs=[]; s=present[0]; p=present[0]
        for i in present[1:]:
            if i==p+1: p=i; continue
            runs.append((s,p)); gaps.append(i-p-1); s=p=i
        runs.append((s,p))
        for a,b in runs:
            L=b-a+1
            (censored if a==0 or b==n-1 else sessions).append(L)
    hist=lambda v: dict(sorted(collections.Counter(v).items()))
    # Kaplan-Meier over all sessions (censored = right-censored at their observed length)
    ev=collections.Counter(sessions); ce=collections.Counter(censored)
    at_risk=len(sessions)+len(censored); S=1.0; km=[]
    for t in sorted(set(ev)|set(ce)):
        d=ev.get(t,0); S*=1-d/at_risk if at_risk else 1; km.append([t,round(S,5)]); at_risk-=d+ce.get(t,0)
    model={"source":"nebula discv5 crawl","window":[times[0],times[-1]],"crawls":n,"cadence_hours":2,
           "peers":peers,"always_on_frac":round(always/peers,4),
           "session_hist_crawls":hist(sessions),"session_censored_hist_crawls":hist(censored),
           "km_survival":km,"gap_hist_crawls":hist(gaps)}
    json.dump(model,open(out,"w"),indent=1)
    print(f"peers={peers} always_on={always} ({100*always/peers:.1f}%) sessions={len(sessions)} censored={len(censored)} gaps={len(gaps)} -> {out}", file=sys.stderr)

cmd=sys.argv[1]
check() if cmd=="check" else extract(sys.argv[2],sys.argv[3],sys.argv[4])
