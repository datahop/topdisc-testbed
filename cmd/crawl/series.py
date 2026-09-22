import json, collections, sys
# per-10-minute series from the event log: sweeps + pong/miss + rtt quantiles
out = {"sweeps": [], "bins": []}
t0 = None
bins = collections.defaultdict(lambda: {"pongs": 0, "misses": 0, "rtt": [], "seen": 0, "up": 0, "down": 0})
for l in open(sys.argv[1]):
    o = json.loads(l)
    t0 = t0 or o["t"]
    b = int((o["t"] - t0) / 600000)
    ev = o["ev"]
    if ev == "sweep":
        out["sweeps"].append({"t": (o["t"] - t0) / 3600000, "known": o["known"], "everUp": o["everUp"], "up": o["up"], "new": o.get("new", 0), "dead": o.get("dead", 0)})
    elif ev == "pong":
        bins[b]["pongs"] += 1
        if len(bins[b]["rtt"]) < 20000:
            bins[b]["rtt"].append(o.get("rtt", 0))
    elif ev == "miss":
        bins[b]["misses"] += 1
    elif ev in ("seen", "up", "down"):
        bins[b][ev] += 1
for b in sorted(bins):
    r = sorted(bins[b]["rtt"])
    q = lambda p: r[int(len(r) * p)] if r else 0
    out["bins"].append({"t": b / 6, "pongs": bins[b]["pongs"], "misses": bins[b]["misses"], "seen": bins[b]["seen"], "up": bins[b]["up"], "down": bins[b]["down"], "rtt50": q(.5), "rtt90": q(.9), "rtt99": q(.99)})
json.dump(out, open(sys.argv[2], "w"))
print("sweeps", len(out["sweeps"]), "bins", len(out["bins"]))
