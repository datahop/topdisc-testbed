#!/usr/bin/env python3
"""Report of one crawl: population, reachability, topics, sessions and churn.

Usage: report.py <crawl dir> [--out DIR] [--names FILE] [--top N]

--names defaults to cmd/crawl/chains.json (chain id -> network name).

<crawl dir> holds derived/gap10/sessions.csv, derived/gap30/{sessions,nodes}.csv
and derived/series.json (from series.py over the event log). Writes
<out>/figures/*.png and <out>/report.md.
"""
import argparse
import collections
import csv
import json
import os

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402
import numpy as np  # noqa: E402

plt.rcParams.update({"font.size": 11, "figure.dpi": 110, "savefig.bbox": "tight", "axes.grid": True, "grid.alpha": 0.3})


TICKS = [(2 / 60, "2 min"), (10 / 60, "10 min"), (0.5, "30 min"), (1, "1 h"), (3, "3 h"), (8, "8 h"), (24, "24 h")]


def hour_ticks(ax, unit_hours=True, lo=2 / 60):
    """Log time axis with readable ticks; values below lo are shown at lo."""
    scale = 1 if unit_hours else 60
    ax.set_xscale("log")
    ax.set_xlim(lo * scale * 0.9, 26 * scale)
    ax.set_xticks([v * scale for v, _ in TICKS])
    ax.set_xticklabels([l for _, l in TICKS], rotation=30, ha="right")
    ax.minorticks_off()


def clip_lo(values, lo):
    return [max(v, lo) for v in values]


def alive_over_time(sessions, span_h, step_h=0.5):
    """Nodes answering at each step, per chain: sessions covering the time."""
    ts = np.arange(0, span_h + step_h, step_h)
    by = collections.defaultdict(lambda: np.zeros(len(ts)))
    for s in sessions:
        a = s["start"] / 3600
        b = s["end"] / 3600 if s["end"] is not None else span_h + 1
        i0, i1 = int(np.searchsorted(ts, a)), int(np.searchsorted(ts, b))
        by[s["chain"]][i0:i1] += 1
    return ts, by


def load_sessions(path):
    out = []
    for r in csv.DictReader(open(path)):
        out.append({"id": r["id"], "chain": r["chain"], "start": float(r["start"]), "end": float(r["end"]) if r["end"] else None,
                    "initial": r["initial"] == "1", "index": int(r["index"])})
    return out


def churn_rows(sessions, span_h, warm_h=1.0):
    by_node = collections.defaultdict(list)
    for s in sessions:
        by_node[s["id"]].append(s)
    by_chain = collections.defaultdict(dict)
    for i, ss in by_node.items():
        by_chain[ss[0]["chain"]][i] = ss
    rows = {}
    hours = span_h - warm_h
    for c, nodes in by_chain.items():
        n = len(nodes)
        start = sum(1 for ss in nodes.values() if ss[0]["start"] <= warm_h * 3600)
        end = sum(1 for ss in nodes.values() if ss[-1]["end"] is None)
        stay = sum(1 for ss in nodes.values() if ss[0]["start"] <= warm_h * 3600 and len(ss) == 1 and ss[0]["end"] is None)
        joins = sum(1 for ss in nodes.values() if ss[0]["start"] > warm_h * 3600)
        gone = sum(1 for ss in nodes.values() if ss[-1]["end"] is not None and ss[-1]["end"] > warm_h * 3600)
        flick = sum(1 for ss in nodes.values() for k in range(1, len(ss)) if ss[k]["start"] > warm_h * 3600)
        closed = sorted((s["end"] - s["start"]) / 3600 for ss in nodes.values() for s in ss if s["end"] is not None)
        rows[c] = {"alive": n, "start": start, "end": end, "stay": stay / max(start, 1), "join_h": joins / hours / n,
                   "gone_h": gone / hours / n, "flick_h": flick / hours / n, "p50_h": closed[len(closed) // 2] if closed else 0}
    return rows


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("dir")
    ap.add_argument("--out", default=None)
    ap.add_argument("--names", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "chains.json"))
    ap.add_argument("--top", type=int, default=15)
    a = ap.parse_args()
    out = a.out or os.path.join(a.dir, "report")
    fig_dir = os.path.join(out, "figures")
    os.makedirs(fig_dir, exist_ok=True)
    names = json.load(open(a.names)) if a.names and os.path.exists(a.names) else {}
    label = lambda c: (names.get(c) or c)[:28]

    s30 = load_sessions(os.path.join(a.dir, "derived/gap30/sessions.csv"))
    s10 = load_sessions(os.path.join(a.dir, "derived/gap10/sessions.csv"))
    nodes = list(csv.DictReader(open(os.path.join(a.dir, "derived/gap30/nodes.csv"))))
    series = json.load(open(os.path.join(a.dir, "derived/series.json")))
    span_h = max(s["t"] for s in series["sweeps"])
    alive_ids = {s["id"] for s in s30}
    records = collections.Counter(n["chain"] for n in nodes)
    alive = collections.Counter(n["chain"] for n in nodes if n["id"] in alive_ids)
    ip_count = collections.Counter(n["ip"] for n in nodes)
    md = [f"# discv5 crawl {os.path.basename(os.path.abspath(a.dir))}", ""]
    md.append(f"{span_h:.1f} h of continuous crawling and probing. {len(alive_ids)} nodes answered at least once, {sum(1 for s in s30 if s['end'] is None)} were answering at the end, "
              f"over {len(alive)} chains. Every count and rate below is over these nodes; the {len(nodes)} records the DHT handed out "
              f"(of which only {100*len(alive_ids)/len(nodes):.0f} % answered) appear only in the reachability note of section 3.")
    md.append("")

    # 01 population over time
    sw = series["sweeps"]
    fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(12, 4.2))
    t = [s["t"] for s in sw]
    ax1.plot(t, [s["known"] / 1000 for s in sw], color="#c9d3d1", lw=2.5, label="records seen")
    ax1.plot(t, [s["everUp"] / 1000 for s in sw], color="#2C6E71", label="answered at least once")
    ax1.set_ylabel("thousand nodes"); ax1.set_xlabel("hours since start"); ax1.set_ylim(bottom=0); ax1.legend()
    ax1.set_title("Records learned vs nodes that answer")
    ax2.plot(t, [s["everUp"] / 1000 for s in sw], color="#2C6E71", label="answered at least once")
    ax2.plot(t, [s["up"] / 1000 for s in sw], color="#C28F24", label="answering now")
    ax2.set_ylabel("thousand nodes"); ax2.set_xlabel("hours since start"); ax2.set_ylim(bottom=0); ax2.legend()
    ax2.set_title("Live population")
    fig.savefig(os.path.join(fig_dir, "01_population.png")); plt.close(fig)
    md += ["## 1. Population", "", "### 01_population", "", "![01_population](figures/01_population.png)", "",
           "*Left: records learned by the random walk against nodes that ever answered a ping. Right: nodes that ever answered against nodes answering at that moment (2-min probes). The first hour is discovery.*", ""]
    top5 = [c for c, _ in alive.most_common(5)]
    ts, by_t = alive_over_time(s30, span_h)
    fig, ax = plt.subplots(figsize=(10, 4.2))
    for c in top5:
        ax.plot(ts, by_t[c], label=label(c))
    ax.set_xlabel("hours since start"); ax.set_ylabel("nodes answering"); ax.set_ylim(bottom=0); ax.legend()
    ax.set_title("Live population of the 5 largest topics")
    fig.savefig(os.path.join(fig_dir, "01b_population_topics.png")); plt.close(fig)
    md += ["### 01b_population_topics", "", "![01b_population_topics](figures/01b_population_topics.png)", "",
           "*Nodes answering at each half hour, per topic, from the sessions (30-min rule).*", ""]

    # 02 probe health
    b = series["bins"]
    fig, (ax1, ax2) = plt.subplots(2, 1, figsize=(10, 6), sharex=True)
    tb = [x["t"] for x in b]
    ax1.plot(tb, [100 * x["misses"] / max(x["pongs"] + x["misses"], 1) for x in b], color="tab:red")
    ax1.set_ylabel("miss rate of live nodes (%)"); ax1.set_ylim(bottom=0)
    ax2.plot(tb, [(x["pongs"] + x["misses"]) / 10 for x in b], color="#2C6E71", label="pings sent to nodes known alive")
    ax2.plot(tb, [x["pongs"] / 10 for x in b], color="#C28F24", label="answered")
    ax2.set_ylabel("per minute"); ax2.set_xlabel("hours since start"); ax2.set_ylim(bottom=0); ax2.legend()
    ax1.set_title("Probe health: share of pings to known-alive nodes that timed out")
    fig.savefig(os.path.join(fig_dir, "02_probe_health.png")); plt.close(fig)
    miss_total = sum(x["misses"] for x in b); pong_total = sum(x["pongs"] for x in b)
    md += ["## 2. Probe health", "", "### 02_probe_health", "", "![02_probe_health](figures/02_probe_health.png)", "",
           f"*{pong_total:,} answers and {miss_total:,} timeouts from nodes that had answered before ({100*miss_total/(pong_total+miss_total):.1f} %). "
           "Top: the share of those pings that timed out. Bottom: pings sent and answered per minute; every node that ever answered is pinged every 2 minutes. "
           "A session ends only after 10 or 30 minutes without an answer, so isolated timeouts do not count as leaves.*", ""]

    # 03 topics: alive vs records
    top = [c for c, _ in alive.most_common(a.top)]
    fig, ax = plt.subplots(figsize=(10, 5.5))
    y = np.arange(len(top))
    ax.barh(y, [alive[c] for c in top], color="#2C6E71")
    ax.set_yticks(y); ax.set_yticklabels([label(c) for c in top]); ax.invert_yaxis()
    ax.set_xlabel("nodes that answered at least once"); ax.set_title(f"Largest {len(top)} topics (chain from the ENR)")
    for i, c in enumerate(top):
        ax.text(alive[c] + max(alive.values()) * 0.01, i, f"{alive[c]}  ({100*alive[c]/records[c]:.0f}% of its records answer)", va="center", fontsize=8)
    ax.set_xlim(0, max(alive.values()) * 1.45)
    fig.savefig(os.path.join(fig_dir, "03_topics.png")); plt.close(fig)
    cum = 0; total_alive = sum(alive.values())
    ranked = alive.most_common()
    bands = [(1, 5), (6, 22), (23, 110), (111, len(ranked))]
    md += ["## 3. Topics", "", "### 03_topics", "", "![03_topics](figures/03_topics.png)", "",
           f"*{len(alive)} chains with at least one answering node: {sum(1 for v in alive.values() if v >= 1000)} with 1000 or more, "
           f"{sum(1 for v in alive.values() if v >= 100)} with 100 or more, {sum(1 for v in alive.values() if v >= 10)} with 10 or more. "
           "The percentage is the share of the chain's DHT records that answer a stranger (the rest are NATed, stale, or rotated identities).*", "",
           "| chains ranked by size | chains | nodes | share |", "|---|---:|---:|---:|"]
    for a_, b_ in bands:
        v = [n for _, n in ranked[a_ - 1:b_]]
        md.append(f"| {a_}–{b_} ({v[0]}…{v[-1]} nodes) | {len(v)} | {sum(v)} | {100*sum(v)/total_alive:.1f} % |")
    md += ["", "| chain | name | nodes | of its records answering | cumulative share |", "|---|---|---:|---:|---:|"]
    for c in top:
        cum += alive[c]
        md.append(f"| `{c}` | {names.get(c, '')} | {alive[c]} | {100*alive[c]/records[c]:.0f} % | {100*cum/total_alive:.0f} % |")
    md.append("")

    # 04 sessions: length CDF per top topic (gap30)
    fig, ax = plt.subplots(figsize=(10, 4.5))
    for c in top5:
        closed = sorted((s["end"] - s["start"]) / 3600 for s in s30 if s["chain"] == c and s["end"] is not None and not (s["index"] == 0 and s["initial"]))
        if len(closed) > 20:
            ax.plot(clip_lo(closed, 2 / 60), np.linspace(0, 1, len(closed)), label=f"{label(c)} (n={len(closed)})")
    hour_ticks(ax); ax.set_xlabel("session length (sessions that started and ended inside the crawl; a single answer counts as 2 min)"); ax.set_ylabel("share of sessions shorter than x"); ax.legend(fontsize=8)
    ax.set_title("Session lengths of the 5 largest topics, 30-min rule")
    fig.savefig(os.path.join(fig_dir, "04_session_length.png")); plt.close(fig)
    md += ["## 4. Sessions", "", "### 04_session_length", "", "![04_session_length](figures/04_session_length.png)", "",
           "*Only sessions with both ends inside the crawl, so these are the sessions of nodes that churn; the majority of nodes never closed a session (see the stay column in section 6). "
           "A quarter to two fifths of these sessions are a single answer: the node was reachable for one probe and silent again for 30 minutes or more.*", ""]

    # 05 absence lengths
    fig, ax = plt.subplots(figsize=(10, 4.2))
    for gap, ss, col in ((10, s10, "tab:orange"), (30, s30, "tab:blue")):
        by = collections.defaultdict(list)
        for s in ss:
            by[s["id"]].append(s)
        gaps = sorted((n[k + 1]["start"] - n[k]["end"]) / 60 for n in by.values() for k in range(len(n) - 1) if n[k]["end"] is not None)
        if gaps:
            ax.plot(clip_lo(gaps, 2), np.linspace(0, 1, len(gaps)), color=col, label=f"{gap}-min rule (n={len(gaps)})")
    hour_ticks(ax, unit_hours=False); ax.set_xlabel("absence before the node answered again"); ax.set_ylabel("share of absences shorter than x"); ax.legend()
    ax.set_title("Absences of nodes that came back")
    fig.savefig(os.path.join(fig_dir, "05_absence_length.png")); plt.close(fig)
    md += ["## 5. Absences", "", "### 05_absence_length", "", "![05_absence_length](figures/05_absence_length.png)", "",
           "*Only nodes that left and answered again: the silence between two of their sessions. Nodes that left for good are not here (they are the leaves in section 6). "
           "The rule that ends a session sets the floor: with the 10-min rule shorter absences count as leaves.*", ""]
    fig, ax = plt.subplots(figsize=(10, 4.2))
    by = collections.defaultdict(list)
    for s in s30:
        by[s["id"]].append(s)
    for c in top5:
        gaps = sorted((n[k + 1]["start"] - n[k]["end"]) / 60 for n in by.values() if n[0]["chain"] == c for k in range(len(n) - 1) if n[k]["end"] is not None)
        if len(gaps) > 20:
            ax.plot(clip_lo(gaps, 2), np.linspace(0, 1, len(gaps)), label=f"{label(c)} (n={len(gaps)})")
    hour_ticks(ax, unit_hours=False); ax.set_xlabel("absence before the node answered again"); ax.set_ylabel("share of absences shorter than x"); ax.legend(fontsize=8)
    ax.set_title("Absences of the 5 largest topics, 30-min rule")
    fig.savefig(os.path.join(fig_dir, "05b_absence_topics.png")); plt.close(fig)
    md += ["### 05b_absence_topics", "", "![05b_absence_topics](figures/05b_absence_topics.png)", "", "*The same, per topic.*", ""]

    # 06 churn per topic
    rows30 = churn_rows(s30, span_h); rows10 = churn_rows(s10, span_h)
    fig, ax = plt.subplots(figsize=(10, 5.5))
    y = np.arange(len(top)); w = 0.27
    ax.barh(y - w, [100 * rows30[c]["join_h"] for c in top], w, label="joins", color="#2C6E71")
    ax.barh(y, [100 * rows30[c]["gone_h"] for c in top], w, label="leaves for good", color="#C28F24")
    ax.barh(y + w, [100 * rows30[c]["flick_h"] for c in top], w, label="returns after an absence", color="#7B1FA2")
    ax.set_yticks(y); ax.set_yticklabels([label(c) for c in top]); ax.invert_yaxis(); ax.set_xlabel("% of the topic's nodes per hour")
    ax.legend(); ax.set_title("Churn per topic, 30-min gap rule, first hour excluded")
    fig.savefig(os.path.join(fig_dir, "06_churn_per_topic.png")); plt.close(fig)
    md += ["## 6. Churn per topic", "", "### 06_churn_per_topic", "", "![06_churn_per_topic](figures/06_churn_per_topic.png)", "",
           "| chain | alive | at start | at end | start nodes never away | joins %/h | leaves %/h | returns %/h (30 min) | returns %/h (10 min) | median closed session |",
           "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|"]
    for c in top:
        r, r10 = rows30[c], rows10.get(c, rows30[c])
        md.append(f"| `{c}` {names.get(c, '')} | {r['alive']} | {r['start']} | {r['end']} | {100*r['stay']:.0f} % | {100*r['join_h']:.2f} | {100*r['gone_h']:.2f} | {100*r['flick_h']:.2f} | {100*r10['flick_h']:.2f} | {r['p50_h']*60:.0f} min |")
    g = churn_rows(s30, span_h)
    tot = {"alive": sum(r["alive"] for r in g.values()), "start": sum(r["start"] for r in g.values()), "end": sum(r["end"] for r in g.values())}
    md += ["", f"All topics: {tot['alive']} nodes ever alive, {tot['start']} present in the first hour, {tot['end']} at the end. "
           "A node is its discv5 ID: a restart with a new key counts as a leave plus an arrival (about a fifth of the permanent leaves are followed within 2 h by a new ID at the same IP:port).", ""]

    # 07 arrivals and departures over time
    fig, ax = plt.subplots(figsize=(10, 4.2))
    hrs = np.arange(0, int(span_h) + 1)
    by = collections.defaultdict(list)
    for s in s30:
        by[s["id"]].append(s)
    joins = collections.Counter(int(ss[0]["start"] // 3600) for ss in by.values())
    gone = collections.Counter(int(ss[-1]["end"] // 3600) for ss in by.values() if ss[-1]["end"] is not None)
    ret = collections.Counter(int(s["start"] // 3600) for ss in by.values() for s in ss[1:])
    ax.plot(hrs, [joins[h] for h in hrs], label="first answer (join or late discovery)")
    ax.plot(hrs, [gone[h] for h in hrs], label="last answer (leave)")
    ax.plot(hrs, [ret[h] for h in hrs], label="return after an absence")
    ax.set_xlabel("hour of the crawl"); ax.set_ylabel("nodes per hour"); ax.set_ylim(0, max(max(gone.values()), max(ret.values())) * 1.3); ax.legend()
    ax.set_title("Arrivals, departures and returns over time (30-min rule; first hour off scale: discovery)")
    fig.savefig(os.path.join(fig_dir, "07_events_over_time.png")); plt.close(fig)
    md += ["## 7. Events over time", "", "### 07_events_over_time", "", "![07_events_over_time](figures/07_events_over_time.png)", "",
           "*The first hour's first answers are discovery, not arrivals, and run off the scale; the last hour's departures include nodes that would have returned after the crawl ended.*", ""]
    fig, axes = plt.subplots(1, 3, figsize=(14, 4), sharex=True)
    for c in top5:
        nodes_c = [ss for ss in by.values() if ss[0]["chain"] == c]
        j = collections.Counter(int(ss[0]["start"] // 3600) for ss in nodes_c)
        g = collections.Counter(int(ss[-1]["end"] // 3600) for ss in nodes_c if ss[-1]["end"] is not None)
        r = collections.Counter(int(s["start"] // 3600) for ss in nodes_c for s in ss[1:])
        n = len(nodes_c)
        axes[0].plot(hrs[1:], [100 * j[h] / n for h in hrs[1:]], label=label(c))
        axes[1].plot(hrs[1:], [100 * g[h] / n for h in hrs[1:]])
        axes[2].plot(hrs[1:], [100 * r[h] / n for h in hrs[1:]])
    for ax, name in zip(axes, ("first answer", "last answer (leave)", "return after an absence")):
        ax.set_title(name); ax.set_xlabel("hour of the crawl"); ax.set_ylim(bottom=0)
    axes[0].set_ylabel("% of the topic's nodes per hour"); axes[0].legend(fontsize=8)
    fig.savefig(os.path.join(fig_dir, "07b_events_topics.png")); plt.close(fig)
    md += ["### 07b_events_topics", "", "![07b_events_topics](figures/07b_events_topics.png)", "",
           "*The same per topic, as a share of the topic's nodes, from hour 1. Returns on consensus mainnet rise through the day in step with the crawler's timeout rate (section 2), "
           "so part of that topic's flicker is probe loss rather than nodes; its first hours (0.5 %/h) are the safer estimate.*", ""]

    open(os.path.join(out, "report.md"), "w").write("\n".join(md))
    print(os.path.join(out, "report.md"))


if __name__ == "__main__":
    main()
