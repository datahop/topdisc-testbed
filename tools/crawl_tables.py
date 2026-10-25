#!/usr/bin/env python3
"""Per-node discv5 tables from a crawl run with -sample-full.

Usage: crawl_tables.py <events.jsonl> [--exclude-last 15m] [--out FILE]

A read is a run of reply events from one peer whose dist lists are disjoint;
a reply asking a distance already in the run starts a new one. A read is
complete when its dist lists together contain 240..256 and none of its
replies at 240..256 failed; an error below 240 only ends the read there. For each peer the last complete read whose replies are all
in the log before the last --exclude-last is kept, and its records are put
in their bucket: max(logdist to the peer, 240), so "240" is geth's bucket 0.
The log is read twice, first for its end time. An entry is alive when its node
answered a ping at any time in the log (an up or pong event), else dead.
Peers that replied but have no such read are counted in peers_partial.
The client reads at most 5 NODES packets per FINDNODE, so a full bucket of
records averaging above ~300 bytes would arrive truncated without an error;
mainnet ENRs (p99 262, max 292 bytes) keep this out of reach.

bucket_fill per distance: n = read peers with at least one entry there,
mean = entries there per read peer.

Prints a summary and writes JSON (default: tables.json next to the log).
"""
import argparse
import array
import collections
import json
import os
import re

FULL = frozenset(range(240, 257))


def chain_of(o):
    if o.get("eth2"):
        return "eth2:" + o["eth2"]
    if o.get("eth"):
        return "eth:" + o["eth"]
    if o.get("opstack"):
        return "op:%d" % o["opstack"]
    return "none"


def parse_duration(s):
    m = re.fullmatch(r"(\d+(?:\.\d+)?)([smh]?)", s.strip())
    if not m:
        raise argparse.ArgumentTypeError("bad duration %r" % s)
    return float(m.group(1)) * {"s": 1, "m": 60, "h": 3600, "": 60}[m.group(2)]


def logdist(a, b):
    return (int(a, 16) ^ int(b, 16)).bit_length()


def dist_stats(xs):
    xs = sorted(xs)
    if not xs:
        return {"n": 0}
    q = lambda p: xs[min(len(xs) - 1, int(p * len(xs)))]
    return {"n": len(xs), "mean": sum(xs) / len(xs), "p10": q(0.1), "p50": q(0.5), "p90": q(0.9)}


class Peer:
    __slots__ = ("dists", "nodes", "t", "ok", "best")

    def __init__(self):
        self.best = None  # (t, nodes) of the last complete read before the cutoff
        self.start()

    def start(self):
        self.dists, self.nodes, self.t, self.ok = set(), array.array("I"), 0, True

    # A read ends when the next one starts or the log ends: in the order
    # 256..224, distances below 240 still arrive after it covers 240..256.
    def finish(self, cutoff):
        if self.ok and FULL <= self.dists and self.t <= cutoff:
            self.best = (self.t, self.nodes)


def analyze(path, exclude_s):
    t_end = None
    with open(path) as f:
        for line in f:
            if line.strip():
                t = json.loads(line)["t"]
                t_end = t if t_end is None else max(t_end, t)
    cutoff = (t_end or 0) - exclude_s * 1000

    index, names = {}, []

    def ix(s):
        i = index.get(s)
        if i is None:
            i = index[s] = len(names)
            names.append(s)
        return i

    chain, alive, peers = {}, set(), {}
    with open(path) as f:
        for line in f:
            if not line.strip():
                continue
            o = json.loads(line)
            ev = o["ev"]
            if ev in ("seen", "enr"):
                chain[ix(o["id"])] = chain_of(o)
            elif ev in ("up", "pong"):
                alive.add(ix(o["id"]))
            elif ev == "reply":
                p = peers.get(o["from"])
                if p is None:
                    p = peers[o["from"]] = Peer()
                dist = o.get("dist") or []
                if p.dists.intersection(dist):
                    p.finish(cutoff)
                    p.start()
                p.dists.update(dist)
                p.t = max(p.t, o["t"])
                if o.get("err"):
                    p.ok = p.ok and not FULL.intersection(dist)
                elif p.ok:
                    p.nodes.extend(ix(n) for n in o.get("nodes") or [])

    nodes, partial = {}, 0
    fill = collections.defaultdict(lambda: [0, 0])
    entries_all, dead_all, shares = 0, 0, []
    for pid in sorted(peers):
        p = peers[pid]
        p.finish(cutoff)
        if p.best is None:
            partial += 1
            continue
        t, ids = p.best
        buckets = collections.defaultdict(list)
        for i in dict.fromkeys(ids):
            buckets[max(logdist(pid, names[i]), 240)].append({"id": names[i], "alive": i in alive})
        n = sum(len(v) for v in buckets.values())
        dead = sum(not e["alive"] for v in buckets.values() for e in v)
        nodes[pid] = {"t": t, "chain": chain.get(index.get(pid), "none"), "entries": n, "dead": dead,
                      "buckets": {str(d): buckets[d] for d in sorted(buckets, reverse=True)}}
        entries_all += n
        dead_all += dead
        if n:
            shares.append(dead / n)
        for d, v in buckets.items():
            fill[d][0] += 1
            fill[d][1] += len(v)
    read = len(nodes)
    return {
        "t_end": t_end,
        "exclude_last_s": exclude_s,
        "alive_ids": len(alive),
        "peers_read": read,
        "peers_partial": partial,
        "nodes": nodes,
        "summary": {
            "entries_mean": entries_all / read if read else 0,
            "dead_share_entries": dead_all / entries_all if entries_all else 0,
            "per_node_dead_share": dist_stats(shares),
            "bucket_fill": {str(d): {"n": fill[d][0], "mean": fill[d][1] / read if read else 0}
                            for d in range(256, 239, -1)},
        },
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("events")
    ap.add_argument("--exclude-last", type=parse_duration, default=parse_duration("15m"),
                    help="reads ending this close to the end of the log are left out (e.g. 15m, 0m)")
    ap.add_argument("--out", default=None)
    args = ap.parse_args()
    res = analyze(args.events, args.exclude_last)
    out = args.out or os.path.join(os.path.dirname(os.path.abspath(args.events)), "tables.json")
    with open(out, "w") as f:
        json.dump(res, f)

    s = res["summary"]
    print("peers read %d  partial %d  alive ids %d" % (res["peers_read"], res["peers_partial"], res["alive_ids"]))
    print("entries per table mean %.1f  dead share of entries %.3f" % (s["entries_mean"], s["dead_share_entries"]))
    d = s["per_node_dead_share"]
    print("dead share per node: " + ("n 0" if not d["n"] else "n %d  mean %.3f  p10 %.3f  p50 %.3f  p90 %.3f" % (
        d["n"], d["mean"], d["p10"], d["p50"], d["p90"])))
    print("bucket fill (distance:peers/mean): " + "  ".join(
        "%s:%d/%.1f" % (k, v["n"], v["mean"]) for k, v in s["bucket_fill"].items()))
    print("wrote " + out)


if __name__ == "__main__":
    main()
