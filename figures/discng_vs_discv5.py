#!/usr/bin/env python3
"""Figures of docs/discng-vs-discv5.md.

Usage:
    discng_vs_discv5.py [--out docs/figures/discng-vs-discv5]

The 5k numbers are the medians of docs/topdisc-vs-discv5.md, the mainnet
numbers come from the crawl log of docs/crawl-2026-09-18.md.
"""
import argparse
import os

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

DISC = "#2a7ad5"
WALK = "#c4c4b8"
TEXT2 = "#666666"

# service size, walk time to 50 % (s), DISC-NG time to 50 % (s), walk / DISC-NG found after 1 h
HALF = [
    ("1 000+ nodes", 1620, 10, 0.74, 1.0),
    ("100–999 nodes", 1560, 2.4, 0.75, 1.0),
    ("10–99 nodes", 1560, 1.1, 0.75, 1.0),
    ("under 10 nodes", 1260, 0.9, 0.78, 1.0),
]
# scenario, walk MB, DISC-NG MB (median bytes sent per node)
BYTES = [
    ("1 h, no churn", 18.7, 5.8),
    ("1 h 30 min, crawl churn", 28.4, 8.6),
]
# mainnet crawl 2026-09-18
RECORDS_LEARNED = 200653
RECORDS = [
    ("Answered at least once (24 h)", 29184),
    ("Live, found within 20 min", 23569),
    ("Live, found within 10 min", 21365),
]


def dur(s):
    return f"{s:g} s" if s < 60 else f"{round(s / 60)} min"


def half_service(out):
    fig, ax = plt.subplots(figsize=(9, 3.6))
    n = len(HALF)
    for i, (label, walk, disc, wf, df) in enumerate(HALF):
        y = n - 1 - i
        ax.plot([disc, walk], [y, y], color=WALK, lw=2, zorder=1)
        ax.scatter([disc], [y], s=80, color=DISC, zorder=2)
        ax.scatter([walk], [y], s=80, color=WALK, zorder=2)
        ax.annotate(dur(disc), (disc, y), xytext=(0, -16), textcoords="offset points", ha="center", color=DISC)
        ax.annotate(dur(walk), (walk, y), xytext=(0, -16), textcoords="offset points", ha="center", color=TEXT2)
        mid = (disc * walk) ** 0.5
        ax.annotate(f"{float(f'{walk / disc:.2g}'):g}×", (mid, y), xytext=(0, 6), textcoords="offset points",
                    ha="center", color=TEXT2)
        ax.text(1.04, y, f"{wf:.0%}", transform=ax.get_yaxis_transform(), va="center", color=TEXT2)
        ax.text(1.12, y, f"{df:.0%}", transform=ax.get_yaxis_transform(), va="center", color=DISC, weight="bold")
    ax.text(1.04, n - 0.3, "walk", transform=ax.get_yaxis_transform(), color=TEXT2)
    ax.text(1.12, n - 0.3, "DISC-NG", transform=ax.get_yaxis_transform(), color=TEXT2)
    ax.text(1.04, n + 0.1, "Found after 1 h", transform=ax.get_yaxis_transform())
    ax.set_xscale("log")
    ax.set_xlim(0.5, 3600)
    ax.set_xticks([1, 10, 60, 600, 3600], ["1 s", "10 s", "1 min", "10 min", "1 h"])
    ax.set_yticks(range(n), [h[0] for h in reversed(HALF)])
    ax.set_ylim(-0.7, n - 0.4)
    ax.grid(axis="x", color="#e6e6e6")
    for s in ("top", "right", "left"):
        ax.spines[s].set_visible(False)
    ax.tick_params(axis="y", length=0)
    ax.scatter([], [], color=DISC, label="DISC-NG")
    ax.scatter([], [], color=WALK, label="discv5 random walk")
    ax.legend(loc="upper left", bbox_to_anchor=(0, 1.18), ncol=2, frameon=False)
    ax.set_title("DISC-NG reaches half of a service far faster than a random walk\n"
                 "Median time to find 50 % of the service, 5 000 nodes, log scale", loc="left", pad=34)
    save(fig, out, "half-service-time")


def bytes_per_node(out):
    fig, ax = plt.subplots(figsize=(9, 3))
    ys, h = [], 0.36
    for i, (label, walk, disc) in enumerate(BYTES):
        y = len(BYTES) - 1 - i
        ys.append(y)
        ax.barh(y + h / 2, walk, h, color=WALK, label="discv5 random walk" if i == 0 else None)
        ax.barh(y - h / 2, disc, h, color=DISC, label="DISC-NG" if i == 0 else None)
        ax.text(walk + 0.4, y + h / 2, f"{walk} MB", va="center", color=TEXT2)
        ax.text(disc + 0.4, y - h / 2, f"{disc} MB   {walk / disc:.1f}× fewer", va="center", color=DISC)
    ax.set_yticks(ys, [b[0] for b in BYTES])
    ax.set_xticks([])
    for s in ("top", "right", "bottom"):
        ax.spines[s].set_visible(False)
    ax.tick_params(axis="y", length=0)
    ax.legend(loc="upper left", bbox_to_anchor=(0, 1.2), ncol=2, frameon=False)
    ax.set_title("DISC-NG nodes send about a third of the bytes\n"
                 "Median bytes sent per node over the whole run, 5 000 nodes", loc="left", pad=30)
    save(fig, out, "bytes-per-node")


def crawl_records(out):
    fig, ax = plt.subplots(figsize=(9, 3))
    answered = RECORDS[0][1]
    dead = RECORDS_LEARNED - answered
    labels = ["Records learned"] + [r[0] for r in RECORDS]
    n = len(labels)
    ax.barh(n - 1, answered, color=WALK)
    ax.barh(n - 1, dead, left=answered, color=DISC)
    ax.text(answered + 1500, n - 1 + 0.48, f"never answered: {dead:,} ({dead / RECORDS_LEARNED:.1%})", color=DISC)
    ax.text(RECORDS_LEARNED, n - 1 + 0.48, f"{RECORDS_LEARNED:,} total", ha="right", color=TEXT2)
    for i, (label, v) in enumerate(RECORDS):
        y = n - 2 - i
        ax.barh(y, v, color=WALK)
        ax.text(v + 2000, y, f"{v:,}", va="center", color=TEXT2)
    ax.set_yticks(range(n), list(reversed(labels)))
    ax.set_xticks([])
    for s in ("top", "right", "bottom"):
        ax.spines[s].set_visible(False)
    ax.tick_params(axis="y", length=0)
    ax.set_title(f"{dead / RECORDS_LEARNED:.0%} of the records a crawler learns never answer\n"
                 "Mainnet discv5 crawl, 2026-09-18, 24 h, 2 min liveness probe", loc="left", pad=20)
    save(fig, out, "crawl-records")


def save(fig, out, stem):
    os.makedirs(out, exist_ok=True)
    fig.savefig(os.path.join(out, stem + ".png"), dpi=130, bbox_inches="tight")
    plt.close(fig)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--out", default="docs/figures/discng-vs-discv5")
    a = p.parse_args()
    half_service(a.out)
    bytes_per_node(a.out)
    crawl_records(a.out)


if __name__ == "__main__":
    main()
