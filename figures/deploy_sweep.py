#!/usr/bin/env python3
"""Partial-deployment sweep: discovery and load against the share of every
service that runs TopDisc, split by node type (TopDisc, plain discv5).

Usage:
    deploy_sweep.py --out DIR [--by-class] SHARE=RUN_DIR [SHARE=RUN_DIR ...]

SHARE is the TopDisc share in % (0 = all plain, 100 = all TopDisc). Each run is
read through pairfigs.Run (metrics.json, oh.json, series.json, run.log), plus
nodes.json (first_capable_ms per node) and assignments/ (node type and topic,
so topic size classes count every member and stay the same across shares).
A TopDisc searcher is scored against the TopDisc members of its topic, a
plain one against every member (pkg/coordinator/metrics.go). With --by-class
every metric is also split by the size of the node's own topic. With one run
it is that run's per-type report.

Writes deploy_sweep.md and deploy_sweep.png into DIR.
"""
import argparse
import glob
import os

import numpy as np

import pairfigs
from pairfigs import Run, fmt_mb, fmt_s, load, pct
from figures import SIZE_CLASSES

TYPES = ("TopDisc", "plain")
COLORS = {"TopDisc": pairfigs.COLORS[0], "plain": pairfigs.COLORS[1]}
LOAD_TYPES = ("REGTOPIC/v5", "TOPICQUERY/v5")
CLASSES = [name.replace(" registrants", " nodes") for _, name in SIZE_CLASSES]


def topic_index(name):
    try:
        return int(name.removeprefix("topic-"))
    except ValueError:
        return 0


def size_class(members):
    for (lo, _), name in zip(SIZE_CLASSES, CLASSES):
        if members >= lo:
            return name
    return CLASSES[-1]


def med_inf(v):
    """Median with unreached values as inf; nan when nothing to take."""
    return float(np.median([x if x is not None else np.inf for x in v])) if v else float("nan")


def stats_for(run, rs_all, nodes, typ, id_idx, capable, regs):
    """Every metric of one node type over the given searchers and nodes."""
    rs = [r for r in rs_all if r.get("target") and run.tgt(r)]
    s = {"nodes": len(nodes), "searchers": len(rs_all), "scorable": len(rs)}
    rec = [min(len(run.found_at(r)) / run.tgt(r), 1.0) for r in rs]
    s["recall_p50"], s["recall_p10"] = pct(rec, 50), pct(rec, 10)
    for f, k in ((None, "t_first"), (0.5, "t_50"), (0.9, "t_90")):
        v = [(float(t[0]) if len(t := run.found_at(r)) else None) if f is None else run.time_to(r, f) for r in rs]
        s[k] = med_inf(v)
    cap = [capable[i] for i in nodes if capable.get(i, -1) >= 0]
    s["capable_n"] = len(cap)
    s["capable_p50"], s["capable_p90"] = pct(np.asarray(cap) / 1000.0, 50), pct(np.asarray(cap) / 1000.0, 90)
    if typ == "TopDisc":
        s["contacted"] = pct([r["searchStats"]["contacted"] for r in rs if r.get("searchStats")], 50)
        ops = [op.get("nodes", 0) for n in run.oh if n["idx"] in nodes for op in n.get("ops") or [] if op.get("msg") == "REGTOPIC/v5"]
        s["reg_nodes"] = pct(ops, 50)
        load_v = [sum(n["byType"].get(t, {}).get("rxMsgs", 0) for t in LOAD_TYPES) for n in run.oh if n["idx"] in nodes and n.get("byType")]
        s["load_p50"], s["load_p99"], s["load_max"] = pct(load_v, 50), pct(load_v, 99), (max(load_v) if load_v else float("nan"))
        lat = [(ns - regs["start"][rid]) / 1e9 for rid, ns in regs["first"].items() if id_idx.get(rid) in nodes and rid in regs["start"]]
        s["reg_p50"], s["reg_p90"] = pct(lat, 50), pct(lat, 90)
    else:
        q = [v for r in rs if (v := run.search_requests(r)) is not None]
        walks = [len(r.get("lookupQueries") or []) or 1 for r in rs if run.search_requests(r) is not None]
        s["contacted"] = pct([a / b for a, b in zip(q, walks)], 50)
    counted = [n for n in run.oh if n["idx"] in nodes and n.get("byType")]
    s["coverage"] = len(counted) / max(len(nodes), 1)
    tx = [n.get("txBytes", 0) for n in counted]
    s["tx_p50"], s["tx_p95"] = pct(tx, 50) / 1e6, pct(tx, 95) / 1e6
    return s


def load_run(share, rd, by_class):
    run = Run(f"{share} %", rd, 0)
    asg = {}
    for p in glob.glob(os.path.join(rd, "assignments", "node*.json")):
        a = load(p)
        asg[a["idx"]] = a
    legacy_of = {i: bool(a.get("legacy")) for i, a in asg.items()}
    topic_of = {i: topic_index(a["topics"][0]) for i, a in asg.items()}
    members = {}
    for t in topic_of.values():
        members[t] = members.get(t, 0) + 1
    cls_of = {i: size_class(members[t]) for i, t in topic_of.items()}
    capable = {n["idx"]: n.get("first_capable_ms", -1) for n in load(os.path.join(rd, "nodes.json"), []) or []}
    id_idx = {n["id"]: n["idx"] for n in run.oh}
    first = {}
    for per in (run.m.get("registrationTimingNs") or {}).values():
        for rid, ns in per.items():
            first[rid] = min(ns, first.get(rid, ns))
    regs = {"first": first, "start": run.m.get("registrationStartNs") or {}}
    raw = run.m.get("results") or []
    groups = [("all", lambda i: True)] + ([(c, lambda i, c=c: cls_of[i] == c) for c in CLASSES] if by_class else [])
    out = {"share": share, "dir": rd, "notes": list(run.notes), "fork": run.p.get("build.fork", "?"), "classes": {}}
    for cname, keep in groups:
        per = {}
        for typ in TYPES:
            nodes = {i for i in asg if legacy_of[i] == (typ == "plain") and keep(i)}
            if not nodes:
                continue
            rs = [r for r in raw if r["nodeIdx"] in nodes]
            per[typ] = stats_for(run, rs, nodes, typ, id_idx, capable, regs)
        if per:
            topics = {t for i, t in topic_of.items() if keep(i)}
            out["classes"][cname] = {"topics": len(topics), "types": per}
    return out


def has(s, k):
    v = s.get(k)
    return v is not None and v == v


def t_cell(v):
    return fmt_s(v) if v != np.inf else "not reached by half"


ROWS = [
    ("nodes", lambda s: f"{s['nodes']:,}"),
    ("searchers / scorable", lambda s: f"{s['searchers']:,} / {s['scorable']:,}"),
    ("recall at end, median / p10", lambda s: f"{100 * s['recall_p50']:.1f} / {100 * s['recall_p10']:.1f} %" if s["scorable"] else "–"),
    ("time to first provider, median", lambda s: t_cell(s["t_first"])),
    ("time to 50 %, median", lambda s: t_cell(s["t_50"])),
    ("time to 90 %, median", lambda s: t_cell(s["t_90"])),
    ("first TopDisc-capable peer, p50 / p90 (saw one)", lambda s: f"{fmt_s(s['capable_p50'])} / {fmt_s(s['capable_p90'])} ({100 * s['capable_n'] / max(s['nodes'], 1):.0f} %)"),
    ("registrars contacted per search, median (plain: FINDNODE per walk)", lambda s: f"{s['contacted']:,.0f}" if has(s, "contacted") else "–"),
    ("REGTOPIC nodes asked per registration, median", lambda s: f"{s['reg_nodes']:,.0f}" if has(s, "reg_nodes") else "–"),
    ("REGTOPIC + TOPICQUERY received per registrar, median / p99 / max", lambda s: f"{s['load_p50']:,.0f} / {s['load_p99']:,.0f} / {s['load_max']:,.0f}" if has(s, "load_p50") else "–"),
    ("first registration latency, median / p90", lambda s: f"{fmt_s(s['reg_p50'])} / {fmt_s(s['reg_p90'])}" if has(s, "reg_p50") else "–"),
    ("sent per node, whole run, p50 / p95 (counter coverage)", lambda s: f"{fmt_mb(s['tx_p50'])} / {fmt_mb(s['tx_p95'])} ({100 * s['coverage']:.0f} %)" if s["coverage"] else "–"),
]


def write_md(runs, out):
    L = ["# Partial-deployment sweep", "",
         "Share = TopDisc share of every service; plain = discv5 built on the fork. TopDisc searchers are scored against the TopDisc members of their topic, plain searchers against every member; under churn, against those up at the end of the search.", "",
         "## Across shares", "",
         "| share | type | " + " | ".join(name for name, _ in ROWS) + " |",
         "|---:|---|" + "---:|" * len(ROWS)]
    for r in runs:
        for typ, s in r["classes"]["all"]["types"].items():
            L.append(f"| {r['share']} % | {typ} | " + " | ".join(f(s) for _, f in ROWS) + " |")
    for r in runs:
        L += ["", f"## {r['share']} % TopDisc", "", f"`{r['dir']}` ({r['fork']})", ""]
        L += [f"- {n}" for n in r["notes"]] + ([""] if r["notes"] else [])
        for cname, c in r["classes"].items():
            if cname != "all":
                L += [f"### {cname} ({c['topics']} topics)", ""]
            types = list(c["types"])
            L += ["| metric | " + " | ".join(types) + " |", "|---|" + "---:|" * len(types)]
            L += [f"| {name} | " + " | ".join(f(c["types"][t]) for t in types) + " |" for name, f in ROWS]
            L.append("")
    path = os.path.join(out, "deploy_sweep.md")
    with open(path, "w") as f:
        f.write("\n".join(L) + "\n")
    return path


def plot(runs, out):
    import matplotlib.pyplot as plt
    panels = [
        ("recall at end, median (%)", lambda s: 100 * s["recall_p50"]),
        ("time to 50 % of the topic, median (s)", lambda s: s["t_50"]),
        ("first TopDisc-capable peer, p50 (s)", lambda s: s["capable_p50"]),
        ("contacted per search (plain: FINDNODE per walk)", lambda s: s["contacted"]),
        ("REGTOPIC+TOPICQUERY per registrar, p50 / p99", lambda s: s.get("load_p50", np.nan)),
        ("sent per node, whole run, p50 (MB)", lambda s: s["tx_p50"] if s["coverage"] else np.nan),
    ]
    fig, axes = plt.subplots(2, 3, figsize=(15, 8), constrained_layout=True)
    xs = [r["share"] for r in runs]
    for ax, (title, f) in zip(axes.flat, panels):
        for typ in TYPES:
            y = [f(r["classes"]["all"]["types"][typ]) if typ in r["classes"]["all"]["types"] else np.nan for r in runs]
            y = [v if v != np.inf else np.nan for v in y]
            if not all(np.isnan(y)):
                ax.plot(xs, y, "o-", color=COLORS[typ], label=typ)
        ax.set_title(title, fontsize=10)
        ax.set_xlabel("TopDisc share (%)")
        ax.set_xticks(xs)
        ax.grid(alpha=0.3)
    ax = axes.flat[4]
    ax.plot(xs, [r["classes"]["all"]["types"].get("TopDisc", {}).get("load_p99", np.nan) for r in runs], "s--", color=COLORS["TopDisc"], label="TopDisc p99")
    axes.flat[0].legend()
    ax.legend()
    fig.suptitle("Partial deployment: discovery and load against the TopDisc share")
    path = os.path.join(out, "deploy_sweep.png")
    fig.savefig(path, dpi=130)
    return path


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--by-class", action="store_true", help="also split every metric by the size of the node's topic")
    ap.add_argument("runs", nargs="+", help="SHARE=RUN_DIR, SHARE the TopDisc share in %%")
    a = ap.parse_args()
    runs = []
    for spec in a.runs:
        share, rd = spec.split("=", 1)
        runs.append(load_run(int(share), rd, a.by_class))  # one run in memory at a time
    runs.sort(key=lambda r: r["share"])
    os.makedirs(a.out, exist_ok=True)
    md = write_md(runs, a.out)
    plot(runs, a.out)
    print(open(md).read())


if __name__ == "__main__":
    main()
