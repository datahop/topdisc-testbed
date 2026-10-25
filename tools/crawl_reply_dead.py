#!/usr/bin/env python3
"""Dead share of the records in FINDNODE replies of a crawl run with -sample.

Usage: crawl_reply_dead.py <events.jsonl> [--exclude-last 15m] [--out FILE]

A record is alive when its node answered a ping at any time in the log (an
up or pong event); every other returned record is dead. Replies in the last
--exclude-last of the log are left out, since the nodes they returned had no
time to be probed. Chains are derived as in cmd/crawl/sessions.py.

Prints a summary and writes JSON (default: reply_dead.json next to the log).
"""
import argparse
import collections
import json
import os
import re


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


def analyze(path, exclude_s):
    chain, alive, replies = {}, set(), []
    t_end = None
    with open(path) as f:
        for line in f:
            if not line.strip():
                continue
            o = json.loads(line)
            t_end = o["t"] if t_end is None else max(t_end, o["t"])
            ev = o["ev"]
            if ev in ("seen", "enr"):
                chain[o["id"]] = chain_of(o)
            elif ev in ("up", "pong"):
                alive.add(o["id"])
            elif ev == "reply":
                replies.append(o)
    cutoff = (t_end or 0) - exclude_s * 1000
    used = [r for r in replies if r["t"] <= cutoff]

    errs = collections.Counter(r["err"] for r in used if r.get("err"))
    per_reply = collections.defaultdict(list)
    per_peer = collections.defaultdict(list)
    rec_entries = collections.defaultdict(lambda: [0, 0])
    rec_unique = collections.defaultdict(set)
    by_dist = collections.defaultdict(lambda: [0, 0])
    empty = 0
    for r in used:
        nodes = r.get("nodes") or []
        if not nodes:
            empty += not r.get("err")
            continue
        dead = [n not in alive for n in nodes]
        share = sum(dead) / len(nodes)
        pc = chain.get(r["from"], "none")
        per_reply[pc].append(share)
        per_peer[r["from"]].append(share)
        for n, d in zip(nodes, dead):
            c = chain.get(n, "none")
            rec_entries[c][0] += 1
            rec_entries[c][1] += d
            rec_unique[c].add(n)
            k = by_dist[logdist(r["from"], n)]
            k[0] += 1
            k[1] += d

    peer_by_chain = collections.defaultdict(list)
    for p, xs in per_peer.items():
        peer_by_chain[chain.get(p, "none")].append(sum(xs) / len(xs))
    order = sorted(per_reply, key=lambda c: -len(per_reply[c]))
    return {
        "events": path,
        "exclude_last_s": exclude_s,
        "replies": len(replies),
        "replies_used": len(used),
        "replies_with_nodes": sum(len(v) for v in per_reply.values()),
        "replies_empty": empty,
        "reply_errors": dict(errs.most_common()),
        "alive_ids": len(alive),
        "per_reply": {"all": dist_stats([x for v in per_reply.values() for x in v]),
                      "by_peer_chain": {c: dist_stats(per_reply[c]) for c in order}},
        "per_peer": {"all": dist_stats([x for v in peer_by_chain.values() for x in v]),
                     "by_peer_chain": {c: dist_stats(peer_by_chain[c]) for c in order}},
        "records_by_chain": {c: {"entries": e[0], "dead_share": e[1] / e[0], "unique": len(rec_unique[c]),
                                 "unique_dead_share": sum(n not in alive for n in rec_unique[c]) / len(rec_unique[c])}
                             for c, e in sorted(rec_entries.items(), key=lambda kv: -kv[1][0])},
        "records_by_distance": {str(d): {"entries": e[0], "dead_share": e[1] / e[0]} for d, e in sorted(by_dist.items())},
    }


def fmt(s):
    if not s["n"]:
        return "n 0"
    return "n %d  mean %.3f  p10 %.3f  p50 %.3f  p90 %.3f" % (s["n"], s["mean"], s["p10"], s["p50"], s["p90"])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("events")
    ap.add_argument("--exclude-last", type=parse_duration, default=parse_duration("15m"),
                    help="replies this close to the end of the log are left out (e.g. 15m, 0m)")
    ap.add_argument("--out", default=None)
    args = ap.parse_args()
    res = analyze(args.events, args.exclude_last)
    out = args.out or os.path.join(os.path.dirname(os.path.abspath(args.events)), "reply_dead.json")
    with open(out, "w") as f:
        json.dump(res, f, indent=1)

    print("replies %d  used %d  with nodes %d  empty %d  errors %s  alive ids %d" % (
        res["replies"], res["replies_used"], res["replies_with_nodes"], res["replies_empty"],
        res["reply_errors"] or "{}", res["alive_ids"]))
    print("dead share per FINDNODE request (dedup records): " + fmt(res["per_reply"]["all"]))
    print("dead share per peer:                             " + fmt(res["per_peer"]["all"]))
    for c, s in list(res["per_reply"]["by_peer_chain"].items())[:8]:
        print("  peer %-16s request %s | peer %s" % (c, fmt(s), fmt(res["per_peer"]["by_peer_chain"][c])))
    print("returned records by chain:")
    for c, e in list(res["records_by_chain"].items())[:8]:
        print("  %-16s entries %6d dead %.3f  unique %6d dead %.3f" % (
            c, e["entries"], e["dead_share"], e["unique"], e["unique_dead_share"]))
    print("returned records by distance: " + "  ".join(
        "%s:%d/%.3f" % (d, e["entries"], e["dead_share"]) for d, e in res["records_by_distance"].items()))
    print("wrote " + out)


if __name__ == "__main__":
    main()
