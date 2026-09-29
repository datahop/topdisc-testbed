#!/usr/bin/env python3
"""Churn-rate axis across runs (plan §2): dead results, recall and dead-result
age against the churn rate each run applied.

Usage:
    churn_axis.py --out DIR LABEL=RUN_DIR [LABEL=RUN_DIR ...]

The churn rate is measured from the run's churn.json (departures per node per
hour over the search window), not taken from the scenario, so runs with
different models or windows sit on the same axis. Reads metrics.json for the
dead-result snapshot and the per-searcher recall.
"""
import argparse
import json
import os

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np


def churn_rate(rd, population):
    """Departures per node per hour over the search window; churn.json lists
    only the nodes that have a schedule, so the population comes from the run."""
    p = os.path.join(rd, "churn.json")
    if not os.path.exists(p):
        return 0.0, 0
    c = json.load(open(p))
    window = c.get("window") or 0
    nodes = c.get("nodes") or {}
    departures = sum(len(ev) for ev in nodes.values())
    if window <= 0 or not population:
        return 0.0, departures
    return departures / population / (window / 3600.0), departures


def dead_share(m):
    dead = m.get("deadResults") or {}
    per = dead.get("perTopic") or []
    returned = sum(t.get("returned", 0) for t in per)
    died = sum(t.get("dead", 0) for t in per)
    hist = {}
    for t in per:
        for k, v in (t.get("ageHistS") or {}).items():
            hist[int(k)] = hist.get(int(k), 0) + v
    ages = np.array(sorted(hist)); counts = np.array([hist[a] for a in ages]) if hist else np.array([])
    p50 = p90 = None
    if counts.size:
        cum = np.cumsum(counts) / counts.sum()
        p50 = ages[np.searchsorted(cum, 0.5)]
        p90 = ages[np.searchsorted(cum, 0.9)]
    return (100.0 * died / returned if returned else None), p50, p90, (dead.get("adLifetimeMs") or 900000) / 1000


def recall(m):
    """Continuous searches: recall at the end. Connection-driven searches stop
    at 16 registrants, so the outcome there is the share that filled their
    slots and the median time it took."""
    res = m.get("results", [])
    conn = [x for x in res if x.get("slotsFilledAtMs") is not None and x.get("outboundConns")]
    if conn and sum(1 for x in conn if x.get("slotsFilledAtMs", 0) > 0) > 0.5 * len(conn):
        filled = [x["slotsFilledAtMs"] / 1000 for x in conn if x.get("slotsFilledAtMs", 0) > 0]
        return (100 * len(filled) / len(conn), float(np.median(filled)), "slots filled (%) / median time to fill (s)")
    r = [x["uniqueRegistrant"] / x["target"] for x in res if x.get("target")]
    return (100 * float(np.median(r)), 100 * float(np.percentile(r, 10)), "recall median / p10 (%)") if r else (None, None, "")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--title", default="")
    ap.add_argument("runs", nargs="+", help="LABEL=RUN_DIR")
    a = ap.parse_args()
    rows = []
    for spec in a.runs:
        label, rd = spec.split("=", 1)
        m = json.load(open(os.path.join(rd, "metrics.json")))
        rate, deps = churn_rate(rd, len(m.get("results", [])))
        ds, p50, p90, life = dead_share(m)
        rec_med, rec_p10, rec_kind = recall(m)
        rows.append((label, rate, deps, ds, p50, p90, life, rec_med, rec_p10, rec_kind))
    rows.sort(key=lambda r: r[1])
    os.makedirs(a.out, exist_ok=True)
    with open(os.path.join(a.out, "churn_axis.md"), "w") as f:
        f.write(f"| run | departures per node per hour | departures | dead results % | dead age p50 / p90 (s) | {rows[0][9]} |\n|---|---:|---:|---:|---:|---:|\n")
        for r in rows:
            f.write(f"| {r[0]} | {r[1]:.2f} | {r[2]} | {r[3]:.2f} | {r[4]} / {r[5]} | {r[7]:.1f} / {r[8]:.1f} |\n" if r[3] is not None
                    else f"| {r[0]} | {r[1]:.2f} | {r[2]} | - | - | {r[7]:.1f} / {r[8]:.1f} |\n")
    x = [r[1] for r in rows]
    fig, ax = plt.subplots(1, 3, figsize=(15, 4.2))
    ax[0].plot(x, [r[3] or 0 for r in rows], "o-"); ax[0].set_ylabel("dead results (% of returned)")
    kind = rows[0][9]
    if kind.startswith("slots"):
        ax[1].plot(x, [r[7] for r in rows], "o-", label="slots filled (%)"); ax1b = ax[1].twinx(); ax1b.plot(x, [r[8] for r in rows], "s--", color="C1", label="median time to fill (s)"); ax1b.set_ylabel("s"); ax[1].set_ylabel("searchers that filled 16 slots (%)")
        ax[1].legend(loc="upper left"); ax1b.legend(loc="upper right")
    else:
        ax[1].plot(x, [r[7] for r in rows], "o-", label="median"); ax[1].plot(x, [r[8] for r in rows], "s--", label="p10"); ax[1].set_ylabel("recall at end (%)"); ax[1].legend()
    ax[2].plot(x, [r[4] or 0 for r in rows], "o-", label="p50"); ax[2].plot(x, [r[5] or 0 for r in rows], "s--", label="p90")
    ax[2].axhline(rows[0][6], color="grey", linestyle=":", label="ad lifetime"); ax[2].set_ylabel("age of dead results (s)"); ax[2].legend()
    for k in range(3):
        ax[k].set_xlabel("departures per node per hour")
        for r in rows:
            ax[k].annotate(r[0], (r[1], [r[3] or 0, r[7], r[4] or 0][k]), textcoords="offset points", xytext=(4, 4), fontsize=8)
        ax[k].grid(alpha=0.3)
    fig.suptitle(a.title or "Churn-rate axis")
    fig.tight_layout()
    fig.savefig(os.path.join(a.out, "churn_axis.png"), dpi=130)
    print(open(os.path.join(a.out, "churn_axis.md")).read())


if __name__ == "__main__":
    main()
