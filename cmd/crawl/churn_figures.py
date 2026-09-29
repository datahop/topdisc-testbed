#!/usr/bin/env python3
"""Session-length and leaving/returning figures of one crawl, from its sessions.csv.

Usage: churn_figures.py <crawl dir>/derived/gap30/sessions.csv <out dir>
Writes crawl-sessions.png (session lengths and hours online per node) and
crawl-leaving.png (never left / came back / left for good, and time away).
"""
import csv, collections, sys, numpy as np, matplotlib
matplotlib.use("Agg"); import matplotlib.pyplot as plt
path, out = sys.argv[1], sys.argv[2]
S = [dict(id=r["id"], chain=r["chain"], start=float(r["start"]), end=float(r["end"]) if r["end"] else None, initial=r["initial"] == "1", index=int(r["index"])) for r in csv.DictReader(open(path))]
span = max(s["end"] or 0 for s in S); span = max(span, max(s["start"] for s in S)); span_h = span / 3600
by = collections.defaultdict(list)
for s in S: by[s["id"]].append(s)
for ss in by.values(): ss.sort(key=lambda s: s["start"])
# ---- figure A: session lengths, open sessions counted to the end of the crawl
PROBE_H = 2 / 60  # a node seen once has a session of length 0; the probe interval is the resolution
closed = np.maximum(np.array([(s["end"] - s["start"]) / 3600 for s in S if s["end"] is not None]), PROBE_H)
opened = np.maximum(np.array([(span - s["start"]) / 3600 for s in S if s["end"] is None]), PROBE_H)
allen = np.concatenate([closed, opened])
fig, ax = plt.subplots(1, 2, figsize=(13, 4.4))
for arr, lab, st in ((allen, f"all sessions ({len(allen)})", "-"), (closed, f"sessions that ended ({len(closed)})", "--"), (opened, f"still up at the end ({len(opened)}, length = time until the crawl ended)", ":")):
    x = np.sort(arr); ax[0].plot(x, np.arange(1, len(x) + 1) / len(x), st, label=lab)
ax[0].set_xscale("log"); ax[0].set_xlim(PROBE_H, 30); ax[0].set_xticks([2/60, 15/60, 1, 6, 24]); ax[0].set_xticklabels(["2 min", "15 min", "1 h", "6 h", "24 h"])
ax[0].set_xlabel("session length (log)"); ax[0].set_ylabel("CDF over sessions"); ax[0].grid(alpha=.3); ax[0].legend(fontsize=8, loc="upper left")
ax[0].axvline(0.25, color="#C28F24", ls=":", lw=1); ax[0].text(0.26, 0.02, "ad lifetime", fontsize=8, color="#C28F24")
ax[0].set_title("How long a node stays online")
# per node: hours online over the crawl
online = np.array([sum(((s["end"] if s["end"] is not None else span) - s["start"]) for s in ss) / 3600 for ss in by.values()])
x = np.sort(online); ax[1].plot(x, np.arange(1, len(x) + 1) / len(x))
ax[1].set_xlabel(f"hours online over the {span_h:.0f} h crawl, per node"); ax[1].set_ylabel(f"CDF over {len(by)} nodes"); ax[1].grid(alpha=.3)
ax[1].set_title("Time online per node")
fig.tight_layout(); fig.savefig(f"{out}/crawl-sessions.png", dpi=130)
# ---- figure B: who leaves, who comes back, how long they are away
init = {i: ss for i, ss in by.items() if ss[0]["initial"]}
never = sum(1 for ss in init.values() if len(ss) == 1 and ss[0]["end"] is None)
came_back = sum(1 for ss in init.values() if len(ss) > 1)
gone = sum(1 for ss in init.values() if len(ss) == 1 and ss[0]["end"] is not None)
left_again = sum(1 for ss in init.values() if len(ss) > 1 and ss[-1]["end"] is not None)
n = len(init)
absences = np.array([(ss[k]["start"] - ss[k - 1]["end"]) / 60 for ss in by.values() for k in range(1, len(ss))])
fig, ax = plt.subplots(1, 2, figsize=(13, 4.4), gridspec_kw={"width_ratios": [1, 1.3]})
labels = ["never left", "left and came back", "left for good"]; vals = [100 * never / n, 100 * came_back / n, 100 * gone / n]
bars = ax[0].bar(labels, vals, color=["#2C6E71", "#C28F24", "#8A3B3B"])
for b, v in zip(bars, vals): ax[0].text(b.get_x() + b.get_width() / 2, v + 0.8, f"{v:.1f} %", ha="center", fontsize=9)
ax[0].set_ylabel(f"% of the {n} nodes present at the start"); ax[0].set_ylim(0, 100); ax[0].set_title(f"Over the {span_h:.0f} h crawl"); ax[0].grid(axis="y", alpha=.3)
ax[0].text(1, vals[1] + 7, f"of these {100 * left_again / max(came_back, 1):.0f} % left again\nby the end", ha="center", fontsize=8, color="#5B676C")
x = np.sort(absences); ax[1].plot(x, np.arange(1, len(x) + 1) / len(x))
for q, lab in ((50, "p50"), (90, "p90")):
    v = np.percentile(absences, q); ax[1].axvline(v, color="grey", ls="--", lw=1); ax[1].text(v, 0.05 + (0.08 if q == 90 else 0), f" {lab} {v:.0f} min", fontsize=8)
ax[1].axvline(15, color="#C28F24", ls=":", lw=1); ax[1].text(15, 0.9, " 15 min ad lifetime", fontsize=8, color="#C28F24")
ax[1].set_xscale("log"); ax[1].set_xticks([10, 15, 30, 60, 180, 600, 1440]); ax[1].set_xticklabels(["10 min", "15", "30", "1 h", "3 h", "10 h", "24 h"]); ax[1].set_xlabel("time away before coming back (log)"); ax[1].set_ylabel(f"CDF over {len(absences)} absences"); ax[1].grid(alpha=.3)
ax[1].set_title("How long a node is away when it comes back")
fig.tight_layout(); fig.savefig(f"{out}/crawl-leaving.png", dpi=130)
print(f"span {span_h:.1f} h; nodes {len(by)}; at start {n}: never left {never} ({100*never/n:.1f} %), came back {came_back} ({100*came_back/n:.1f} %), gone {gone} ({100*gone/n:.1f} %); returners that left again {left_again}")
print(f"sessions: all {len(allen)} median {np.median(allen):.2f} h; ended {len(closed)} median {np.median(closed)*60:.0f} min p90 {np.percentile(closed,90):.1f} h; open {len(opened)} median {np.median(opened):.1f} h")
print(f"absences {len(absences)}: p50 {np.median(absences):.0f} min p90 {np.percentile(absences,90):.0f} min share under 15 min {100*np.mean(absences<15):.0f} %; online per node median {np.median(online):.1f} h, p10 {np.percentile(online,10):.1f} h")
