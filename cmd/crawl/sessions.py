#!/usr/bin/env python3
"""Fold a crawl event log into node sessions.

Usage: sessions.py <events.jsonl> [--out DIR] [--initial MIN] [--down-after MIN]

Writes nodes.csv (id, chain, first seen, keys), sessions.csv (id, chain,
start, end, initial, index) and enr_changes.csv, and prints a summary.
Times are seconds since the first event. A node is "initial" when it was
seen within the first --initial minutes.

Sessions are derived from the raw pong/miss events, not from the run-time
up/down events: a session starts at a node's first answer and ends at its
last answer before a gap of --down-after minutes without one; the next
answer after such a gap starts a new session (a rejoin). Logs without
pong events (older runs) fall back to the up/down events.

Chains are the ENR identifiers themselves (eth2 fork digest, eth fork hash,
opstack chain id).
"""
import argparse
import collections
import csv
import json
import os


def chain_of(o):
    if o.get("eth2"):
        return "eth2:" + o["eth2"]
    if o.get("eth"):
        return "eth:" + o["eth"]
    if o.get("opstack"):
        return "op:%d" % o["opstack"]
    return "none"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("events")
    ap.add_argument("--out", default=None)
    ap.add_argument("--initial", type=float, default=15.0, help="minutes: seen before this counts as initially present")
    ap.add_argument("--down-after", type=float, default=10.0, help="minutes without an answer that end a session")
    ap.add_argument("--merge-endpoint", type=float, default=0, help="minutes: a node whose last session ended is the same node as one first answering at the same ip:port within this time (key rotation); 0 = nodes are ids")
    args = ap.parse_args()
    gap = args.down_after * 60
    out = args.out or os.path.dirname(os.path.abspath(args.events))

    nodes, enr_changes, sessions, open_at, sweeps = {}, [], [], {}, []
    last_pong, have_pongs = {}, False
    endpoint = {}
    t0 = None
    with open(args.events) as f:
        for line in f:
            o = json.loads(line)
            t = o["t"] / 1000.0
            if t0 is None:
                t0 = t
            rel = t - t0
            ev, nid = o["ev"], o.get("id")
            if ev == "seen":
                nodes[nid] = {"id": nid, "chain": chain_of(o), "first_seen": rel, "keys": " ".join(o.get("keys") or []),
                              "ip": o.get("ip", ""), "src": o.get("src", "")}
                endpoint[nid] = (o.get("ip", ""), o.get("udp", 0))
            elif ev == "enr":
                n = nodes.get(nid)
                new = chain_of(o)
                enr_changes.append({"id": nid, "t": rel, "seq": o["seq"], "chain": new, "chain_before": n["chain"] if n else ""})
                if n:
                    n["chain"] = new
            elif ev in ("up", "pong"):
                if ev == "pong":
                    have_pongs = True
                prev = last_pong.get(nid)
                if prev is not None and rel - prev > gap:
                    sessions.append({"id": nid, "start": open_at.pop(nid), "end": prev})
                open_at.setdefault(nid, rel)
                last_pong[nid] = rel
            elif ev == "down" and not have_pongs:
                start = open_at.pop(nid, None)
                if start is not None:
                    sessions.append({"id": nid, "start": start, "end": rel})
            elif ev == "sweep":
                sweeps.append({"t": rel, **{k: o[k] for k in ("n", "known", "everUp", "up", "pings", "pongs")}})
    t_end = (sweeps[-1]["t"] if sweeps else rel)
    for nid, start in open_at.items():
        # A node silent for the gap at the end of the log has left; one
        # answering within the gap is still up.
        if have_pongs and t_end - last_pong[nid] > gap:
            sessions.append({"id": nid, "start": start, "end": last_pong[nid]})
        else:
            sessions.append({"id": nid, "start": start, "end": ""})
    if args.merge_endpoint > 0:
        # Chain a node whose last session ended to the next id that first
        # answered at the same endpoint within the window: one machine.
        by_ep = collections.defaultdict(list)
        first = {}
        for s in sessions:
            first.setdefault(s["id"], s["start"])
        for nid, t in first.items():
            by_ep[endpoint.get(nid)].append((t, nid))
        last_end = {}
        for s in sessions:
            last_end[s["id"]] = s["end"] if s["end"] not in (None, "") else float("inf")
        alias = {}
        for ep, lst in by_ep.items():
            if not ep or not ep[0]:
                continue
            lst.sort()
            for (t_a, a), (t_b, b) in zip(lst, lst[1:]):
                if last_end[a] != float("inf") and 0 <= t_b - last_end[a] <= args.merge_endpoint * 60:
                    alias[b] = alias.get(a, a)
        for s in sessions:
            s["id"] = alias.get(s["id"], s["id"])
        print(f"merged {len(alias)} rotated ids into their predecessors")
    sessions.sort(key=lambda s: (s["id"], s["start"]))
    idx = collections.Counter()
    for s in sessions:
        n = nodes.get(s["id"], {})
        s["chain"] = n.get("chain", "none")
        s["initial"] = int(n.get("first_seen", 1e9) <= args.initial * 60)
        s["index"] = idx[s["id"]]
        idx[s["id"]] += 1

    with open(os.path.join(out, "nodes.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=["id", "chain", "first_seen", "ip", "src", "keys"])
        w.writeheader(); w.writerows(nodes.values())
    with open(os.path.join(out, "sessions.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=["id", "chain", "start", "end", "initial", "index"])
        w.writeheader(); w.writerows(sessions)
    with open(os.path.join(out, "enr_changes.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=["id", "t", "seq", "chain_before", "chain"])
        w.writeheader(); w.writerows(enr_changes)

    by_chain = collections.Counter(n["chain"] for n in nodes.values())
    alive_chain = collections.Counter(s["chain"] for s in sessions if s["index"] == 0)
    print(f"span {t_end/3600:.2f} h  records {len(nodes)}  ever alive {sum(alive_chain.values())}  sessions {len(sessions)}  "
          f"open at end {sum(1 for s in sessions if s['end']=='')}  rejoins {sum(1 for s in sessions if s['index']>0)}  enr changes {len(enr_changes)}")
    print(f"{'chain':22s} {'records':>8s} {'alive':>7s} {'sessions':>9s} {'rejoins':>8s} {'median session':>15s}")
    for chain, n in by_chain.most_common(15):
        ss = [s for s in sessions if s["chain"] == chain]
        closed = sorted(s["end"] - s["start"] for s in ss if s["end"] != "")
        med = f"{closed[len(closed)//2]/60:.0f} min" if closed else "-"
        print(f"{chain:22s} {n:8d} {alive_chain[chain]:7d} {len(ss):9d} {sum(1 for s in ss if s['index']>0):8d} {med:>15s}")


if __name__ == "__main__":
    main()
