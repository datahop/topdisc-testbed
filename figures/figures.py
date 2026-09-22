#!/usr/bin/env python3
"""Generate figures and a Markdown report from a simnet testbed multi-topic
metrics JSON file.

Usage:
    figures.py <metrics.json> [--out-dir DIR] [--label LABEL] [--params KEY=VAL ...]

Each --params entry is stamped into the report's parameters table.

Produces in <DIR> (default ./figures-<label>):

    01_topic_distribution.{png,pdf}        nodes per topic
    02_time_to_first_cdf.{png,pdf}         CDF of time-to-first result, all searchers
    03_unique_found_over_time.{png,pdf}    per-topic mean ± std of unique registrants
                                           found vs time (drives off uniqueFoundAtMs)
    04_id_space_registrants.{png,pdf}      strip plot of admitted registrants in
                                           ID-space, one row per topic
    05_id_space_found_vs_missed.{png,pdf}  per-topic discovery coverage across ID space
    06_fanout_both_views.{png,pdf}         side-by-side: (a) per-registrant fan-out
                                           and (b) per-host load, per topic
    07_registration_latency_bar.{png,pdf}  mean ± std time-to-first-remote-admission
                                           per topic, clipped at 0
    report.md                              Markdown report with embedded figures + tables
"""
import argparse
import collections
import json
import os
import sys

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.lines import Line2D
import numpy as np

matplotlib.rcParams.update({
    "font.size": 12,
    "axes.titlesize": 13,
    "axes.labelsize": 12,
    "legend.fontsize": 10,
    "xtick.labelsize": 11,
    "ytick.labelsize": 11,
    "figure.dpi": 110,
    "savefig.bbox": "tight",
    "pdf.fonttype": 42,
    "ps.fonttype": 42,
})


def emit(fig, out, stem, ok=True):
    """Save a figure, or discard it when the plotter had nothing to draw.

    A figure written with empty axes reads as a real result that happens to be
    flat. Several measurements here are optional or only present in newer runs,
    so a missing one must show up as a missing figure.
    """
    if ok:
        fig.savefig(os.path.join(out, stem + ".png"))
        fig.savefig(os.path.join(out, stem + ".pdf"))
    plt.close(fig)


def load(path):
    with open(path) as f:
        return json.load(f)


# Helper: searchers' foundIds use TerminalString (16-hex prefix); coverage byRegistrant
# uses full String() (64-hex). Truncate both to the first 16 chars to match.
def short_id(id_str):
    return id_str[:16]


# The multi-topic search path records only the deduplicated registrant set
# (foundRegistrantIds); foundIds is populated by the single-topic path. Every
# consumer here intersects with the registrant set anyway, so prefer the former
# and fall back to the latter.
def found_reg_ids(r):
    return set(r.get("foundRegistrantIds") or r.get("foundIds") or [])


def per_topic_fanout(topic_idx, cov_by_topic):
    cov_t = cov_by_topic.get(str(topic_idx), {})
    fan = list(cov_t.get("byRegistrant", {}).values())
    fan.sort()
    return fan


def per_topic_hostload(topic_idx, cov_by_topic):
    """How many distinct registrants of this topic each host holds in its
    topic table. Drives the (b) side of the fan-out figure."""
    cov_t = cov_by_topic.get(str(topic_idx), {})
    load = list(cov_t.get("byHost", {}).values())
    load.sort()
    return load


def per_registrant_discovery(results_for_topic, cov_by_topic, topic_idx):
    """Returns (sorted list of 'found-by-N-searchers' counts, list of registrant short IDs)."""
    fanout = {short_id(k): v for k, v in cov_by_topic.get(str(topic_idx), {}).get("byRegistrant", {}).items()}
    found_count = collections.Counter()
    for r in results_for_topic:
        for fid in found_reg_ids(r):
            found_count[fid] += 1
    counts = [found_count.get(rid, 0) for rid in fanout]
    counts.sort()
    return counts, list(fanout.keys())


def unique_recall_per_searcher(results_for_topic, cov_by_topic, topic_idx):
    fanout = {short_id(k): v for k, v in cov_by_topic.get(str(topic_idx), {}).get("byRegistrant", {}).items()}
    fanout_set = set(fanout.keys())
    target = len(fanout_set) - 1
    out = []
    for r in results_for_topic:
        uniq = len(found_reg_ids(r) & fanout_set)
        out.append(uniq)
    out.sort()
    return out, target


# Per-topic figures draw one row or line per topic up to this many topics.
# Beyond it, a run with a hundred crawl-derived topics is unreadable line by
# line, so the topics are pooled: all together, then by registrant count.
TOPIC_LINES = 12
SIZE_CLASSES = ((1000, "1000+ registrants"), (100, "100-999 registrants"),
                (10, "10-99 registrants"), (0, "under 10 registrants"))


def topic_groups(per_topic):
    """Rows of the per-topic figures as (name, topics, colour, linewidth)."""
    topics = sorted(r["topic"] for r in per_topic)
    if len(topics) <= TOPIC_LINES:
        return [(f"topic {t}", [t], plt.cm.tab10(t % 10), 1.8) for t in topics]
    size = {r["topic"]: r.get("target", 0) for r in per_topic}
    rows = [("all topics", topics, "black", 2.4)]
    hi = None
    for i, (lo, name) in enumerate(SIZE_CLASSES):
        ts = [t for t in topics if size[t] >= lo and (hi is None or size[t] < hi)]
        if ts:
            rows.append((f"{name}, {len(ts)} topics", ts, plt.cm.tab10(i), 1.6))
        hi = lo
    return rows


def pooled(groups):
    return any(len(ts) > 1 for _, ts, _, _ in groups)


def group_label(name, ts):
    """Axis label for an ID-space row: the topic, or the pool it stands for."""
    return name if len(ts) == 1 else name.replace(", ", "\n")


def load_churn(metrics_path):
    """The churn schedule the run applied (churn.json next to metrics.json):
    (window seconds, {node idx: [(down, up), ...]}) or None."""
    path = os.path.join(os.path.dirname(os.path.abspath(metrics_path)), "churn.json")
    if not os.path.exists(path):
        return None
    with open(path) as f:
        d = json.load(f)
    nodes = {int(k): [(e["DownAt"], e["UpAt"]) for e in v] for k, v in (d.get("nodes") or {}).items()}
    return d.get("window", 0.0), nodes


def absence_before(intervals, t):
    """Seconds the node was away during [0, t]."""
    return sum(max(0.0, min(up, t) - max(down, 0.0)) for down, up in intervals)


def online_time(intervals, window):
    return window - absence_before(intervals, window)


# ────────────────────────────────────────────────────────────────────────────
# Figure plotters
# ────────────────────────────────────────────────────────────────────────────


def topic_index_map(metrics):
    """Map a topic's hex ID to its topic index.

    Topic IDs are spread across the keyspace and carry no index, so the run
    emits the mapping. Metrics files written before that existed are recovered
    by matching each timing block's registrants against the per-topic
    registrant sets in registrationCoverage -- the legacy trick of reading an
    index out of the hex silently produced garbage for every topic.
    """
    ids = metrics.get("topicIds")
    if ids:
        return {k: int(v) for k, v in ids.items()}
    cov = (metrics.get("registrationCoverage") or {}).get("byTopic") or {}
    members = {int(t): set(c.get("byRegistrant", {})) for t, c in cov.items()}
    out = {}
    for topic_hex, regs in (metrics.get("registrationTimingNs") or {}).items():
        sample = set(list(regs)[:25])
        if not sample:
            continue
        best, score = None, 0
        for t, ms in members.items():
            hit = len(sample & ms)
            if hit > score:
                best, score = t, hit
        if best is not None:
            out[topic_hex] = best
    return out


def topic_positions(metrics):
    """Topic index -> the topic ID's own position in the normalised ID space.

    Every ID-space figure plots node IDs on 0..1; without the topic's own
    position on the same axis there is no way to see whether an effect is
    centred on the topic or spread across the keyspace.
    """
    return {idx: int(hex_id[:16], 16) / float(2 ** 64)
            for hex_id, idx in topic_index_map(metrics).items()}


def mark_topic(ax, pos):
    """Draw the topic ID's position as a reference line on an ID-space axis."""
    if pos is None:
        return
    ax.axvline(pos, color="#7B1FA2", ls=":", linewidth=1.8, zorder=5,
               label=f"topic ID position ({pos:.3f})")


def plot_topic_distribution(per_topic, ax, label):
    """01: nodes per topic. Ordered by descending count for legibility."""
    topics = sorted(per_topic, key=lambda r: -r["target"])
    xs = list(range(len(topics)))
    ys = [r["numSearchers"] for r in topics]
    ax.bar(xs, ys, color="#3066BE")
    ax.set_xticks(xs)
    ax.set_xticklabels([f"topic {r['topic']}" for r in topics])
    ax.set_xlabel("topic")
    ax.set_ylabel("nodes")
    ax.set_title(f"{label}: nodes per topic (Zipf draw — each node both registers and searches)")
    for i, y in enumerate(ys):
        ax.text(i, y + max(ys) * 0.01, str(y), ha="center", fontsize=9)


def plot_recall_reached(per_topic, results, fig, label, groups):
    """02: does discovery actually reach every registrant?

    Left: distinct registrants found over time as a fraction of the topic's
    registrant population (median across searchers, inter-quartile band), with
    100% marked. Right: CDF over searchers of the fraction each one ended up
    reaching, so the spread behind the median is visible and the share of
    searchers achieving full recall can be read directly off the axis.
    """
    by_topic = {}
    for r in results:
        ts = r.get("uniqueFoundAtMs") or []
        tgt = r.get("target", 0)
        if tgt <= 0 or not ts:
            continue
        by_topic.setdefault(r["topic"], []).append((np.asarray(ts, dtype=float), tgt))
    if not by_topic:
        return False

    axl, axr = fig.subplots(1, 2)
    horizon = max(float(ts[-1]) for v in by_topic.values() for ts, _ in v) / 1000.0
    grid = np.linspace(0, horizon, 220)

    for name, ts_, colour, lw in groups:
        rows = [x for t in ts_ for x in by_topic.get(t, [])]
        if not rows:
            continue
        curves = np.empty((len(rows), grid.size))
        finals = np.empty(len(rows))
        for i, (ts, tgt) in enumerate(rows):
            curves[i] = np.searchsorted(ts, grid * 1000.0, side="right") / tgt
            finals[i] = len(ts) / tgt
        med = np.median(curves, axis=0)
        axl.plot(grid, med, color=colour, linewidth=lw, label=f"{name} (n={len(finals)})")
        axl.fill_between(grid, np.percentile(curves, 25, axis=0),
                         np.percentile(curves, 75, axis=0), color=colour, alpha=0.15, linewidth=0)
        f = np.sort(finals)
        axr.plot(f, np.arange(1, f.size + 1) / f.size, color=colour, linewidth=lw,
                 label=f"{name}: median {np.median(f):.0%}")

    for ax in (axl, axr):
        ax.grid(alpha=0.3)
        ax.set_ylim(0, 1.02)
    axl.axhline(1.0, ls="--", color="#666", linewidth=1)
    axl.set_xlabel("search time (s)")
    axl.set_ylabel("registrants found / registrants in topic")
    axl.set_title("Distinct peers found over time (median, IQR)", fontsize=10)
    axl.legend(fontsize=8, loc="lower right")
    axr.axvline(1.0, ls="--", color="#666", linewidth=1)
    axr.set_xlim(0, 1.02)
    axr.set_xlabel("fraction of registrants found by end of search")
    axr.set_ylabel("CDF over searchers")
    axr.set_title("Where each searcher finished", fontsize=10)
    axr.legend(fontsize=8, loc="upper left")
    fig.suptitle(f"{label}: discovery completeness (1.0 = every registrant in the topic)")
    return True


def plot_time_to_first_cdf(results, ax, label, groups, churn=None):
    """02b: CDF of time-to-first result, one line per topic or topic pool.

    Split by topic because a searcher's first result depends on how densely
    its topic is registered: a crowded topic answers sooner than a sparse one,
    and a single pooled curve hides that. With the run's churn schedule the
    time excludes the node's own absences: a node that is away when the
    search phase starts is not searching.
    """
    by_topic = {}
    corrected = 0
    for r in results:
        if r.get("timeToFirstNs", 0) > 0:
            t = r["timeToFirstNs"] / 1e9
            if churn and r.get("nodeIdx") in churn[1]:
                away = absence_before(churn[1][r["nodeIdx"]], t)
                if away > 0:
                    t = max(t - away, 0.0)
                    corrected += 1
            by_topic.setdefault(r["topic"], []).append(t)
    if not by_topic:
        return False
    for name, ts, colour, lw in groups:
        xs = np.sort([v for t in ts for v in by_topic.get(t, [])])
        if not xs.size:
            continue
        ys = np.arange(1, xs.size + 1) / xs.size
        ax.plot(xs, ys, linewidth=lw, color=colour,
                label=f"{name} (n={xs.size}, median {np.median(xs):.2f}s)")
    ax.set_xlabel("time to first result (s)" + (", net of the node's absences" if churn else ""))
    ax.set_ylabel("CDF over searchers")
    ax.set_title(f"{label}: time to first result, by topic" + (f" ({corrected} searchers were away first)" if corrected else ""))
    ax.grid(alpha=0.3)
    ax.set_ylim(0, 1.0)
    ax.set_xlim(left=0)
    ax.legend(fontsize=8, loc="lower right")
    return True


def plot_unique_found_over_time(per_topic, results, ax, label):
    """03: per-topic mean ± std of unique-registrants-found over wall time.

    For each searcher, uniqueFoundAtMs is the timestamp (ms since search
    start) at which the i-th distinct registrant was first observed. We
    sample the per-searcher curves on a common time grid and plot the
    cross-searcher mean ± 1σ band per topic.
    """
    by_topic = collections.defaultdict(list)
    for r in results:
        by_topic[r["topic"]].append(r)
    # Cap the plotted window at 50s — discovery is essentially complete by
    # the first few seconds, the rest is plateau that visually hides the
    # early-growth dynamics. Per-searcher data beyond 50s is still counted
    # via cumulative-at-cap, it's just not rendered.
    x_limit_ms = 50_000
    grid = np.linspace(0, x_limit_ms, 200)
    cmap = plt.cm.tab10
    for i, rec in enumerate(sorted(per_topic, key=lambda r: -r["target"])):
        t = rec["topic"]
        rs = by_topic[t]
        per_search_curves = []
        for r in rs:
            ts = r.get("uniqueFoundAtMs") or []
            if not ts:
                per_search_curves.append(np.zeros_like(grid))
                continue
            ts_sorted = np.sort(ts)
            counts = np.searchsorted(ts_sorted, grid, side="right")
            per_search_curves.append(counts.astype(float))
        if not per_search_curves:
            continue
        arr = np.vstack(per_search_curves)
        mean = arr.mean(axis=0)
        std = arr.std(axis=0)
        color = cmap(i % 10)
        ax.plot(grid, mean, linewidth=1.6, color=color,
                label=f"topic {t} ({rec['target']} target)")
        ax.fill_between(grid, np.maximum(mean - std, 0), mean + std,
                        alpha=0.18, color=color)
    ax.set_xlabel("time since search start (ms)")
    ax.set_ylabel("unique registrants found (mean ± 1σ)")
    ax.set_title(f"{label}: unique registrants discovered over time, per topic (first 50 s)")
    ax.legend(loc="lower right", fontsize=9)
    ax.grid(alpha=0.3)
    ax.set_xlim(0, x_limit_ms)
    ax.set_ylim(bottom=0)


def plot_time_to_fraction(per_topic, results, fig, label, groups, fractions=(0.5, 0.9, 0.99)):
    """03: time for each searcher to find a share of its topic's registrants.

    One panel per share, a CDF over searchers per topic or topic pool.
    Searchers that never reach the share leave their curve below 1, so the
    height where a curve ends is the fraction of searchers that got there.
    """
    targets = {r["topic"]: r["target"] for r in per_topic}
    by_topic = collections.defaultdict(list)
    for r in results:
        by_topic[r["topic"]].append(r.get("uniqueFoundAtMs") or [])
    if not any(ts for runs in by_topic.values() for ts in runs):
        return False
    axes = fig.subplots(1, len(fractions), sharey=True, squeeze=False)[0]
    for ax, frac in zip(axes, fractions):
        for name, ts_, colour, lw in groups:
            reached, total = [], 0
            for t in ts_:
                need = int(np.ceil(frac * targets.get(t, 0)))
                runs = by_topic.get(t, [])
                if need <= 0 or not runs:
                    continue
                total += len(runs)
                reached += [max(ts[need - 1], 1) / 1000.0 for ts in runs if len(ts) >= need]
            reached = np.sort(reached)
            if reached.size:
                ax.plot(reached, np.arange(1, reached.size + 1) / total, linewidth=lw, color=colour,
                        label=f"{name} ({100 * reached.size / total:.0f}% reach, median {np.median(reached):.1f}s)")
        ax.set_title(f"{frac:.0%} of the topic's registrants")
        ax.set_xlabel("search time (s)")
        ax.set_xscale("log")
        ax.grid(alpha=0.3, which="both")
        ax.set_ylim(0, 1.0)
        ax.legend(fontsize=7, loc="lower right")
    axes[0].set_ylabel("CDF over searchers")
    fig.suptitle(f"{label}: time to find a share of the topic's registrants")
    return True


def plot_lookup_latency(results, ax, label, groups):
    """08: lookup latency, the time to reach F_lookup distinct registrants.

    Scheduled runs record each lookup, and the CDF is over lookups. Other
    models run one search per node, so the latency is when that search found
    its F_lookup-th distinct registrant. Lookups or searches that never got
    there keep their curve below 1.
    """
    scheduled = any(r.get("lookups") for r in results)
    by_topic = collections.defaultdict(list)
    missed = collections.Counter()
    for r in results:
        t, f = r["topic"], r.get("fLookup") or 30
        f = min(f, r.get("target") or f)  # a topic smaller than F_lookup: all its registrants
        if scheduled:
            for lat, n in zip(r.get("lookupLatencyMs") or [], r.get("lookupResults") or []):
                if n >= f:
                    by_topic[t].append(lat / 1000.0)
                else:
                    missed[t] += 1
        else:
            ts = r.get("uniqueFoundAtMs") or []
            if len(ts) >= f:
                by_topic[t].append(ts[f - 1] / 1000.0)
            else:
                missed[t] += 1
    if not by_topic:
        return False
    for name, ts, colour, lw in groups:
        xs = np.sort([v for t in ts for v in by_topic.get(t, [])])
        total = xs.size + sum(missed[t] for t in ts)
        if not xs.size:
            continue
        ax.plot(xs, np.arange(1, xs.size + 1) / total, linewidth=lw, color=colour,
                label=f"{name} (median {np.median(xs):.2f}s, {100 * xs.size / total:.0f}% reach)")
    ax.set_xlabel("time to F_lookup distinct registrants (s)")
    ax.set_ylabel("CDF over " + ("lookups" if scheduled else "searchers"))
    ax.set_title(f"{label}: lookup latency, by topic" + ("" if scheduled else " (first F_lookup results of each search)"))
    ax.grid(alpha=0.3)
    ax.set_ylim(0, 1.0)
    ax.set_xlim(left=0)
    ax.legend(fontsize=8, loc="lower right")
    return True


def plot_lookup_contacts(results, fig, label, groups):
    """09: registrars contacted per lookup, by topic or topic pool.

    Distinct nodes asked and TOPICQUERY requests sent. Scheduled runs count
    each lookup; other models count what a search needed to reach F_lookup
    distinct registrants.
    """
    scheduled = any(r.get("lookupContacted") for r in results)
    nodes_by, queries_by = collections.defaultdict(list), collections.defaultdict(list)
    for r in results:
        t = r["topic"]
        if scheduled:
            nodes_by[t] += r.get("lookupContacted") or []
            queries_by[t] += r.get("lookupQueries") or []
        elif r.get("targetContacted", -1) >= 0:
            nodes_by[t].append(r["targetContacted"])
            queries_by[t].append(r["targetQueries"])
    if not any(nodes_by.values()):
        return False
    axes = fig.subplots(1, 2, squeeze=False)[0]
    suffix = " per lookup" if scheduled else " to reach F_lookup"
    for ax, data, what in ((axes[0], nodes_by, "distinct nodes contacted"),
                           (axes[1], queries_by, "TOPICQUERY requests sent")):
        for name, ts, colour, lw in groups:
            xs = np.sort([v for t in ts for v in data.get(t, [])])
            if xs.size:
                ax.plot(xs, np.arange(1, xs.size + 1) / xs.size, linewidth=lw, color=colour,
                        label=f"{name} (median {np.median(xs):.0f})")
        ax.set_xlabel(what + suffix)
        ax.grid(alpha=0.3)
        ax.set_ylim(0, 1.0)
        ax.set_xlim(left=0)
        ax.legend(fontsize=8, loc="lower right")
    axes[0].set_ylabel("CDF over " + ("lookups" if scheduled else "searchers"))
    fig.suptitle(f"{label}: registrars contacted per lookup, by topic")
    return True


def plot_discovery_rate(per_topic, results, fig, label, groups):
    """11: discovery rate of long-lived searches (continuous and conn models).

    (a) New distinct registrants per searcher per minute, mean over the
    searchers of the topic or pool, over search time. (b) The same as a share
    of each searcher's own topic, so topics of different size compare.
    Scheduled runs are skipped: their searches restart every lookup.
    """
    if any(r.get("lookups") for r in results):
        return False
    targets = {r["topic"]: max(r["target"], 1) for r in per_topic}
    by_topic = collections.defaultdict(list)
    end_ms = 0
    for r in results:
        ts = r.get("uniqueFoundAtMs") or []
        by_topic[r["topic"]].append(ts)
        end_ms = max(end_ms, r.get("timeToCompletionNs", 0) / 1e6, ts[-1] if ts else 0)
    if not end_ms or not any(ts for runs in by_topic.values() for ts in runs):
        return False
    width_ms = max(60_000, end_ms / 80)
    edges = np.arange(0, end_ms + width_ms, width_ms)
    ax_a, ax_b = fig.subplots(1, 2, squeeze=False)[0]
    per_min = 60_000 / width_ms
    mids = (edges[:-1] + width_ms / 2) / 60_000
    for name, ts_, colour, lw in groups:
        counts, share, n = np.zeros(len(edges) - 1), np.zeros(len(edges) - 1), 0
        for t in ts_:
            for ts in by_topic.get(t, []):
                h = np.histogram(ts, bins=edges)[0]
                counts += h
                share += h / targets.get(t, 1)
                n += 1
        if not n:
            continue
        regs = sum(targets.get(t, 0) for t in ts_)
        ax_a.plot(mids, counts / n * per_min, lw=lw, color=colour, label=f"{name} ({regs} registrants)")
        ax_b.plot(mids, 100 * share / n * per_min, lw=lw, color=colour, label=name)
    ax_a.set_ylabel("new registrants per searcher per minute (mean)")
    ax_b.set_ylabel("% of the topic's registrants found per minute (mean)")
    for ax in (ax_a, ax_b):
        ax.set_xlabel("search time (min)")
        ax.set_yscale("symlog", linthresh=0.1)
        ax.set_xlim(left=0)
        ax.set_ylim(bottom=0)
        ax.grid(alpha=0.3, which="both")
        ax.legend(fontsize=8)
    fig.suptitle(f"{label}: discovery rate of long-lived searches, by topic ({width_ms / 60_000:.0f} min bins)")
    return True


def plot_dead_results(dead, fig, label, groups):
    """10: search results pointing at registrants that were offline (churn runs).

    (a) Share of returned results that were dead, over time, per topic or pool.
    (b) How long each dead result had been offline, against the ad lifetime.
    """
    recs = {t["topic"]: t for t in ((dead or {}).get("perTopic") or []) if t.get("returned")}
    if not recs:
        return False
    ax_a, ax_b = fig.subplots(1, 2, squeeze=False)[0]
    life_s = (dead.get("adLifetimeMs") or 0) / 1000.0
    # The run counts in short buckets (10 s); a quiet topic returns a handful
    # of results per bucket and two dead ones read as 67%. The share is drawn
    # over 5 min windows so every point rests on hundreds of results.
    bucket_ms = dead.get("bucketMs") or 10_000
    window_ms = max(bucket_ms, 300_000)
    for name, ts, colour, lw in groups:
        rows = [recs[t] for t in ts if t in recs]
        if not rows:
            continue
        over = collections.defaultdict(lambda: [0, 0])
        hist = collections.Counter()
        returned = sum(r["returned"] for r in rows)
        died = sum(r["dead"] for r in rows)
        for rec in rows:
            for p in rec.get("overTime") or []:
                w = (p[0] // window_ms) * window_ms
                over[w][0] += p[1]
                over[w][1] += p[2]
            for k, v in (rec.get("ageHistS") or {}).items():
                hist[int(k)] += v
        ot = [(t + window_ms / 2, v[0], v[1]) for t, v in sorted(over.items()) if v[0] > 0]
        if ot:
            ax_a.plot([p[0] / 1000.0 for p in ot], [100.0 * p[2] / p[1] for p in ot], linewidth=lw, color=colour,
                      label=f"{name} ({100.0 * died / returned:.2f}% overall)")
        if hist:
            ages = np.array(sorted(hist))
            counts = np.array([hist[a] for a in ages])
            ax_b.plot(ages, np.cumsum(counts) / counts.sum(), linewidth=lw, color=colour,
                      label=f"{name} ({counts.sum()} dead)")
    ax_a.set_xlabel("time since search start (s)")
    ax_a.set_ylabel("dead results (% of returned)")
    ax_a.set_title(f"Dead share of returned results ({window_ms // 60_000} min windows)")
    ax_a.grid(alpha=0.3)
    ax_a.set_ylim(bottom=0)
    ax_a.legend(fontsize=8)
    max_age = max((int(k) for rec in recs.values() for k in (rec.get("ageHistS") or {})), default=0)
    if life_s and life_s <= 3 * max_age:
        ax_b.axvline(life_s, color="#7B1FA2", ls=":", linewidth=1.6, label=f"ad lifetime ({life_s / 60:.0f} min)")
    elif life_s:
        ax_b.set_title(f"Age of dead results (all under {max_age + 1} s; ad lifetime {life_s / 60:.0f} min)")
    ax_b.set_xlim(0, max(max_age * 1.05, 1))
    ax_b.set_xlabel("time offline when returned (s)")
    ax_b.set_ylabel("CDF over dead results")
    if not ax_b.get_title():
        ax_b.set_title("Age of dead results")
    ax_b.grid(alpha=0.3)
    ax_b.set_ylim(0, 1.0)
    ax_b.legend(fontsize=8, loc="lower right")
    fig.suptitle(f"{label}: results pointing at offline registrants")
    return True


def plot_id_space_registrants(per_topic, cov_by_topic, fig, label, groups, tpos=None):
    """04: admitted registrants across the ID space, by how widely they placed.

    One panel per topic or topic pool. Each dot is a registrant at its ID
    position; height, size and colour all encode how many distinct registrars
    accepted its ad, so a node that only got one registrar is visibly
    different from one that reached thirty. The dotted line is the topic ID's
    own position, drawn for single-topic rows.
    """
    if not groups:
        return False
    axes = fig.subplots(len(groups), 1, sharex=True, squeeze=False)[:, 0]
    tpos = tpos or {}
    any_row = False
    for ax, (name, ts, _, _) in zip(axes, groups):
        fan = {k: v for t in ts for k, v in cov_by_topic.get(str(t), {}).get("byRegistrant", {}).items()}
        if not fan:
            ax.set_ylabel(f"{group_label(name, ts)}\n(no data)", fontsize=8)
            continue
        any_row = True
        xs = [int(k[:16], 16) / float(2 ** 64) for k in fan]
        ys = list(fan.values())
        sizes = [6 + 2.2 * v for v in ys]
        sc = ax.scatter(xs, ys, s=sizes, c=ys, cmap="viridis", alpha=0.75, linewidths=0)
        fig.colorbar(sc, ax=ax, pad=0.01, label="registrars")
        if len(ts) == 1:
            mark_topic(ax, tpos.get(ts[0]))
        ax.set_ylabel(f"{group_label(name, ts)}\nregistrars holding", fontsize=8)
        ax.set_ylim(bottom=0)
        ax.grid(True, alpha=0.3)
        if len(ts) == 1:
            ax.legend(fontsize=7, loc="upper right")
    axes[-1].set_xlim(0, 1.0)
    axes[-1].set_xlabel("registrant ID position (top 64 bits, normalised 0..1)")
    axes[0].set_title(f"{label}: how widely each registrant placed its ad, across the ID space")
    return any_row


def plot_id_space_registrars(per_topic, cov_by_topic, fig, label, groups, tpos=None):
    """04b: registrars across the ID space, by how many ads they hold.

    The mirror of figure 04: each dot is a host that accepted at least one ad
    for the topic (or any topic of the pool), placed at its own ID position,
    with height and colour showing how many of those ads it stores. Marker
    size is fixed -- ad load spans two orders of magnitude, and scaling the
    markers by it produces blobs that hide both the baseline population and
    the topic line.
    """
    if not groups:
        return False
    axes = fig.subplots(len(groups), 1, sharex=True, squeeze=False)[:, 0]
    tpos = tpos or {}
    any_row = False
    for ax, (name, ts, _, _) in zip(axes, groups):
        load = collections.Counter()
        for t in ts:
            for k, v in cov_by_topic.get(str(t), {}).get("byHost", {}).items():
                load[k] += v
        if not load:
            ax.set_ylabel(f"{group_label(name, ts)}\n(no data)", fontsize=8)
            continue
        any_row = True
        xs = [int(k[:16], 16) / float(2 ** 64) for k in load]
        ys = list(load.values())
        sc = ax.scatter(xs, ys, s=9, c=ys, cmap="plasma", alpha=0.7, linewidths=0)
        fig.colorbar(sc, ax=ax, pad=0.01, label="ads held")
        if len(ts) == 1:
            mark_topic(ax, tpos.get(ts[0]))
        ax.set_ylabel(f"{group_label(name, ts)}\nads held", fontsize=8)
        ax.set_ylim(bottom=0)
        ax.grid(True, alpha=0.3)
        if len(ts) == 1:
            ax.legend(fontsize=7, loc="upper right")
    axes[-1].set_xlim(0, 1.0)
    axes[-1].set_xlabel("registrar ID position (top 64 bits, normalised 0..1)")
    axes[0].set_title(f"{label}: how many ads each registrar holds, across the ID space")
    return any_row


# Discovery-coverage bands for figure 05: an ordered green-to-yellow ramp for
# registrants most searchers reached, and a deliberately off-ramp magenta for
# the under-80% tail so a coverage problem is impossible to mistake for the
# merely-imperfect end of the scale. (label, colour, lower bound as a fraction
# of the searcher count); evaluated top-down.
COVERAGE_BANDS = [
    ("found by every searcher", "#14532D", 1.0),
    ("95-99%", "#3F8A4F", 0.95),
    ("90-94%", "#7FB069", 0.90),
    ("85-89%", "#C3D17B", 0.85),
    ("80-84%", "#EFD469", 0.80),
    ("under 80%", "#C2185B", 0.0),
]


def coverage_color(found, n_searchers):
    """Colour for a registrant found by `found` of `n_searchers` searchers."""
    if n_searchers <= 0:
        return COVERAGE_BANDS[-1][1]
    frac = found / n_searchers
    for _, colour, lo in COVERAGE_BANDS:
        if frac >= lo and (lo < 1.0 or found >= n_searchers):
            return colour
    return COVERAGE_BANDS[-1][1]


def plot_id_space_found_vs_missed(per_topic, results, cov_by_topic, fig, label, groups, tpos=None,
                                  churn=None, ad_lifetime_s=900.0):
    """05: discovery coverage across the ID space, per topic or topic pool.

    One subplot per row: x = registrant ID position, y = share of the
    searchers of its topic that returned that registrant via the iterator
    (a count for single-topic rows). With the run's churn schedule, a
    registrant online for less than an ad lifetime is drawn hollow: its ad
    expired before most searchers could see it, which is churn, not search.
    """
    short_lived = set()
    if churn:
        window, sched = churn
        for r in results:
            if r.get("nodeId") and r.get("nodeIdx") in sched and online_time(sched[r["nodeIdx"]], window) < ad_lifetime_s:
                short_lived.add(short_id(r["nodeId"]))
    if not groups:
        return
    axs = fig.subplots(nrows=len(groups), ncols=1, sharex=True)
    if len(groups) == 1:
        axs = [axs]
    by_topic = collections.defaultdict(list)
    for r in results:
        by_topic[r["topic"]].append(r)
    searchers = {r["topic"]: r["numSearchers"] for r in per_topic}
    # A topic with a single member has nobody to find that member; its
    # registrant is not a coverage failure and is left out.
    lonely = {r["topic"] for r in per_topic if r.get("target", 0) == 0}
    for ax, (name, ts, _, _) in zip(axs, groups):
        single = len(ts) == 1
        xs, ys, colors, brief = [], [], [], []
        for t in ts:
            if t in lonely:
                continue
            fanout = {short_id(k): v for k, v in cov_by_topic.get(str(t), {}).get("byRegistrant", {}).items()}
            found_count = collections.Counter()
            searcher_ids = set()
            for r in by_topic.get(t, []):
                if r.get("nodeId"):
                    searcher_ids.add(short_id(r["nodeId"]))
                for fid in found_reg_ids(r):
                    found_count[fid] += 1
            for rid in fanout:
                c = found_count.get(rid, 0)
                # A node never finds itself: the possible finders exclude it.
                n_s = searchers.get(t, 0) - (1 if rid in searcher_ids else 0)
                xs.append(int(rid[:16], 16) / float(2 ** 64))
                ys.append(c if single else (c / n_s if n_s > 0 else 0.0))
                colors.append(coverage_color(c, n_s))
                brief.append(rid in short_lived)
        top = (searchers.get(ts[0], 0) - 1) if single else 1.0
        # Never-found registrants are the real failures, not just the low end
        # of the ramp, so they get their own marker rather than a colour.
        short = [i for i, b in enumerate(brief) if b]
        miss = [i for i, y in enumerate(ys) if y == 0 and not brief[i]]
        keep = [i for i, y in enumerate(ys) if y > 0 and not brief[i]]
        ax.scatter([xs[i] for i in keep], [ys[i] for i in keep],
                   c=[colors[i] for i in keep], s=18, alpha=0.85)
        if short:
            ax.scatter([xs[i] for i in short], [ys[i] for i in short], facecolors="none",
                       edgecolors="#6b7280", s=26, linewidths=1.0)
        if miss:
            ax.scatter([xs[i] for i in miss], [0] * len(miss), marker="x", s=34,
                       c=COVERAGE_BANDS[-1][1], linewidths=1.4)
        ax.axhline(top, color="black", linestyle="--", linewidth=1, alpha=0.4)
        if ax is axs[0]:
            # The dot colour bands how completely a registrant was discovered;
            # without a key the three colours are unreadable.
            handles = [Line2D([], [], marker="o", ls="", color=c, label=lab)
                       for lab, c, _ in COVERAGE_BANDS]
            handles.append(Line2D([], [], marker="x", ls="", color=COVERAGE_BANDS[-1][1],
                                  label="never found"))
            if short_lived:
                handles.append(Line2D([], [], marker="o", ls="", markerfacecolor="none", color="#6b7280",
                                      label=f"online under an ad lifetime ({len(short_lived)})"))
            handles.append(Line2D([], [], ls="--", color="black", alpha=0.4,
                                  label="every other searcher (max possible)"))
            ax.legend(handles=handles, fontsize=7.5, loc="lower right", ncol=3, framealpha=0.92)
        if single:
            mark_topic(ax, (tpos or {}).get(ts[0]))
            ax.set_ylim(-1, top + 5)
            ax.set_ylabel(f"{name}\ntimes found", fontsize=9)
        else:
            ax.set_ylim(-0.02, 1.08)
            ax.set_ylabel(f"{group_label(name, ts)}\nshare of searchers", fontsize=8)
        ax.set_xlim(0, 1.0)
        ax.grid(True, axis="x", alpha=0.3)
    axs[-1].set_xlabel("registrant ID position (top 64 bits, normalised 0..1)")
    fig.suptitle(f"{label}: per-registrant discovery coverage across ID space", fontsize=12)


def plot_fanout_both_views(per_topic, cov_by_topic, fig, label, num_hosts, groups):
    """06: side-by-side box plots per topic or topic pool -- (a) per-registrant
    fan-out (how many registrars hold each registrant's ad), and (b) per-host
    load (how many registrants of the topic each host holds)."""
    target = {r["topic"]: r["target"] for r in per_topic}
    ax_a, ax_b = fig.subplots(nrows=1, ncols=2, sharey=False)

    data_a, data_b, labels_list = [], [], []
    for name, ts, _, _ in groups:
        fan = [v for t in ts for v in per_topic_fanout(t, cov_by_topic)]
        if not fan:
            continue
        data_a.append(fan)
        data_b.append([v for t in ts for v in per_topic_hostload(t, cov_by_topic)])
        labels_list.append(f"{name}\n({target[ts[0]]} target)" if len(ts) == 1 else group_label(name, ts))
    for ax, data, colour in ((ax_a, data_a, "#3066BE"), (ax_b, data_b, "#E76F51")):
        if data:
            bp = ax.boxplot(data, showfliers=True, patch_artist=True)
            for box in bp["boxes"]:
                box.set(facecolor=colour, alpha=0.6)
            ax.set_xticks(range(1, len(labels_list) + 1))
            ax.set_xticklabels(labels_list, fontsize=8 if pooled(groups) else None)
        ax.grid(True, axis="y", alpha=0.3)
    ax_a.set_ylabel(f"registrars holding each registrant (cap = {num_hosts - 1})")
    ax_a.set_title("(a) per-registrant fan-out")
    ax_b.set_ylabel("registrants of the topic per host")
    ax_b.set_title("(b) per-host load")
    fig.suptitle(f"{label}: fan-out two views — per registrant vs per host", fontsize=12)


def plot_placement_time_idspace(per_topic, reg_timing, ax_data, fig, label, groups, tpos=None,
                                start_ns=None, placements=None, mode="min"):
    """07 / 07b: how long placing an ad took, per node, across the ID space.

    mode="min"  -- time until the ad was admitted *anywhere*: how long before a
                   registrant is findable at all.
    mode="mean" -- mean time across every registrar that accepted the ad: how
                   long the full placement took, which is the cost a registrant
                   actually pays to spread itself.

    Both are measured from the node's own registration start where that was
    recorded, so the harness's staggered start does not leak into the numbers.
    One row per topic or topic pool.
    """
    if not groups:
        return False
    by_hex = ax_data or {}
    hex_of = {i: h for h, i in by_hex.items() if h in reg_timing}
    sn = start_ns or {}
    axes = fig.subplots(len(groups), 1, sharex=True, squeeze=False)[:, 0]
    any_data = False
    for ax, (name, ts, _, _) in zip(axes, groups):
        xs, ys = [], []
        for t in ts:
            hex_id = hex_of.get(t)
            if not hex_id:
                continue
            place = (placements or {}).get(hex_id, {})
            for rid, first in reg_timing[hex_id].items():
                began = sn.get(rid, 0)
                if mode == "mean":
                    a = place.get(rid)
                    if not a or not a.get("count"):
                        continue
                    val = (a["sumNs"] / a["count"] - began) / 1e9
                else:
                    val = (first - began) / 1e9
                xs.append(int(rid[:16], 16) / float(2 ** 64))
                ys.append(max(val, 0.0))
        if xs:
            any_data = True
            ax.scatter(xs, ys, s=9, c=ys, cmap="viridis", alpha=0.7, linewidths=0)
            ax.axhline(float(np.median(ys)), color="#444", ls="--", linewidth=1,
                       label=f"median {np.median(ys):.1f}s")
        if len(ts) == 1:
            mark_topic(ax, (tpos or {}).get(ts[0]))
        ax.set_ylabel(f"{group_label(name, ts)}\ntime (s)", fontsize=8)
        ax.set_ylim(bottom=0)
        ax.grid(alpha=0.3)
        ax.legend(fontsize=7, loc="upper right")
    axes[-1].set_xlim(0, 1)
    axes[-1].set_xlabel("registrant ID position (top 64 bits, normalised 0..1)")
    axes[0].set_title(
        f"{label}: time to place an ad anywhere, per registrant" if mode == "min"
        else f"{label}: mean time to place an ad across all its registrars")
    return any_data


def plot_registration_latency_bar(per_topic, reg_timing, ax, label, groups, topic_ids=None,
                                  start_ns=None):
    """07: mean ± std time-to-first-remote-admission per topic or topic pool.
    Bars clipped at zero so the lower error bar never crosses zero."""
    if not reg_timing:
        return False
    target = {r["topic"]: r["target"] for r in per_topic}
    # Topic IDs carry no index, so the caller supplies the resolved mapping.
    hex_of = {i: h for h, i in (topic_ids or {}).items() if h in reg_timing}
    # Measure from each registrant's own registration start: nodes are
    # staggered across minutes at 10k, so timing them from the start of the
    # sweep reports the schedule, not the protocol.
    sn = start_ns or {}
    means_ms, stds_ms, counts, xs_labels = [], [], [], []
    for name, ts, _, _ in groups:
        vals_ms = []
        for t in ts:
            hex_id = hex_of.get(t)
            if hex_id is None:
                continue
            for rid, admitted in reg_timing[hex_id].items():
                vals_ms.append(max(admitted - sn.get(rid, 0), 0) / 1e6)
        if not vals_ms:
            continue
        means_ms.append(float(np.mean(vals_ms)))
        stds_ms.append(float(np.std(vals_ms)))
        counts.append(len(vals_ms))
        xs_labels.append(f"{name}\n({target[ts[0]]} target)" if len(ts) == 1 else group_label(name, ts))
    if not means_ms:
        ax.set_title(f"{label}: no registration timing data")
        return
    means_ms = np.array(means_ms)
    stds_ms = np.array(stds_ms)
    # Asymmetric error bars: lower error capped so the bar never dips below 0.
    lower_err = np.minimum(stds_ms, means_ms)
    upper_err = stds_ms
    xs = np.arange(len(means_ms))
    ax.bar(xs, means_ms, yerr=[lower_err, upper_err], capsize=6, color="#3066BE",
           edgecolor="black", alpha=0.85, error_kw={"linewidth": 1.2})
    ax.set_xticks(xs)
    ax.set_xticklabels(xs_labels, fontsize=8 if pooled(groups) else None)
    ax.set_ylabel("time from own registration start\nto first remote admission (ms) — mean ± 1σ")
    ax.set_title(f"{label}: registration latency per topic")
    ax.grid(True, axis="y", alpha=0.3)
    ax.set_ylim(bottom=0)
    for i, (m, s, c) in enumerate(zip(means_ms, stds_ms, counts)):
        ax.text(i, m + s + max(means_ms) * 0.04, f"{m:.0f}±{s:.0f}\nn={c}",
                ha="center", fontsize=9)
    return True


def plot_search_bucket(per_topic, results, fig, label, groups):
    """12: where searches query, by topic or topic pool.

    (a) Share of TOPICQUERY requests per search-table bucket, 0 the farthest
    from the topic. (b) For adaptive searches, the median active bucket over
    search time, with the bucket each topic settles in and when it got there.
    (c) The settled bucket of every topic against its size: a popular topic
    should stay far out, a rare one move in.
    """
    by_topic = collections.defaultdict(list)
    depth = 0
    for r in results:
        st = r.get("searchStats") or {}
        if st.get("queriesByBucket"):
            by_topic[r["topic"]].append(st)
            depth = max(depth, len(st["queriesByBucket"]))
    if not by_topic:
        return False
    members = {r["topic"]: r.get("target", 0) for r in per_topic}
    adaptive = any(p["bucket"] > 0 for sts in by_topic.values() for st in sts for p in (st.get("activeTrace") or []))
    axes = fig.subplots(1, 3 if adaptive else 1, squeeze=False)[0]
    ax_q = axes[0]
    xs = np.arange(depth)
    end_ms = max((p["atMs"] for sts in by_topic.values() for st in sts for p in (st.get("activeTrace") or [])), default=0)
    grid = np.linspace(0, max(end_ms, 1), 200)
    settle_from = int(0.8 * grid.size)  # the last fifth of the search decides where a topic settled

    def median_curve(sts):
        curves = []
        for st in sts:
            tr = sorted((p["atMs"], p["bucket"]) for p in (st.get("activeTrace") or []))
            if not tr:
                continue
            at = np.array([p[0] for p in tr], dtype=float)
            bk = np.array([p[1] for p in tr], dtype=float)
            idx = np.searchsorted(at, grid, side="right") - 1
            curves.append(np.where(idx >= 0, bk[np.maximum(idx, 0)], np.nan))
        if not curves:
            return None
        med = np.nanmedian(np.vstack(curves), axis=0)
        # Light smoothing: the per-searcher traces are step functions and the
        # raw median flickers between neighbouring buckets.
        k = 5
        return np.convolve(np.nan_to_num(med), np.ones(k) / k, mode="same")

    def settled(curve):
        b = float(np.median(curve[settle_from:]))
        reached = np.nonzero(curve >= b - 0.5)[0]
        return b, (grid[reached[0]] / 60_000 if reached.size else float("nan"))

    for name, ts, colour, lw in groups:
        sts = [st for t in ts for st in by_topic.get(t, [])]
        if not sts:
            continue
        n_members = sum(members.get(t, 0) for t in ts)
        counts = np.zeros(depth)
        for st in sts:
            q = st["queriesByBucket"]
            counts[:len(q)] += q
        if counts.sum():
            mean_b = float((counts * xs).sum() / counts.sum())
            ax_q.plot(xs, 100 * counts / counts.sum(), marker="o", ms=3, lw=lw, color=colour,
                      label=f"{name} ({n_members} members, {int(counts.sum())} queries, mean bucket {mean_b:.1f})")
        if adaptive:
            curve = median_curve(sts)
            if curve is not None:
                b, t_min = settled(curve)
                axes[1].plot(grid / 60_000, curve, lw=lw, color=colour,
                             label=f"{name} ({n_members} members): settles at {b:.0f} after {t_min:.1f} min")
    ax_q.set_xlabel("search-table bucket (0 = farthest from the topic)")
    ax_q.set_ylabel("share of TOPICQUERY requests (%)")
    ax_q.set_title("Where queries go")
    ax_q.set_xticks(xs)
    ax_q.grid(alpha=0.3)
    ax_q.legend(fontsize=7)
    if adaptive:
        axes[1].set_xlabel("search time (min)")
        axes[1].set_ylabel("active bucket (median over searchers, smoothed)")
        axes[1].set_title("Where the adaptive search settles")
        axes[1].set_ylim(-0.5, depth - 0.5)
        axes[1].grid(alpha=0.3)
        axes[1].legend(fontsize=7, loc="lower right")
        # (c) every topic on its own, whatever the pooling: settled bucket vs size
        colour_of = {t: colour for _, ts, colour, _ in groups for t in ts}
        pts = []
        for t, sts in by_topic.items():
            curve = median_curve(sts)
            if curve is not None and members.get(t, 0) > 0:
                pts.append((members[t], settled(curve)[0], t))
        if pts:
            mx = np.array([p[0] for p in pts], dtype=float)
            my = np.array([p[1] for p in pts])
            axes[2].scatter(mx, my, s=28, c=[colour_of.get(p[2], "gray") for p in pts], alpha=0.85, linewidths=0)
            if len(pts) <= TOPIC_LINES:
                for x, y, t in pts:
                    axes[2].annotate(str(t), (x, y), fontsize=7, xytext=(3, 3), textcoords="offset points")
            if len(pts) >= 3:
                lx = np.log2(mx)
                a, c = np.polyfit(lx, my, 1)
                xx = np.linspace(lx.min(), lx.max(), 50)
                r = np.corrcoef(lx, my)[0, 1]
                axes[2].plot(2 ** xx, a * xx + c, ls="--", color="#444", lw=1,
                             label=f"fit: {a:+.2f} buckets per doubling of members (r = {r:.2f})")
                axes[2].legend(fontsize=7, loc="upper right")
            axes[2].set_xscale("log", base=2)
            axes[2].set_xlabel("topic members")
            axes[2].set_ylabel("settled bucket (closest = %d)" % (depth - 1))
            axes[2].set_ylim(-0.5, depth - 0.5)
            axes[2].set_title(f"Settled bucket vs topic size ({len(pts)} topics)")
            axes[2].grid(alpha=0.3, which="both")
    fig.suptitle(f"{label}: search distance to the topic, by topic")
    return True


# ────────────────────────────────────────────────────────────────────────────
# Markdown report
# ────────────────────────────────────────────────────────────────────────────


def write_report(out_dir, label, params, per_topic, results, cov_by_topic, reg_timing, num_hosts):
    path = os.path.join(out_dir, "report.md")
    lines = []
    lines.append(f"# Simnet experiment report — `{label}`\n")

    lines.append("## Simulation parameters\n")
    lines.append("| parameter | value |")
    lines.append("|---|---|")
    for k, v in params.items():
        lines.append(f"| {k} | {v} |")
    lines.append("")

    # Figure 1 lives in the setup section — it's a visual of the topic
    # distribution drawn by the Zipf process, i.e. another input parameter.
    if os.path.exists(os.path.join(out_dir, "01_topic_distribution.png")):
        lines.append("![01_topic_distribution](01_topic_distribution.png)\n")
        lines.append("*Nodes per topic (Zipf draw).*\n")

    # Aggregate
    n_searchers = sum(r["numSearchers"] for r in per_topic)
    full = sum(r["fullRecall"] for r in per_topic)
    lines.append("## Aggregate results\n")
    lines.append("| metric | value |")
    lines.append("|---|---|")
    lines.append(f"| total nodes (every node both registers and searches its topic) | {n_searchers} |")
    lines.append(f"| topics | {len(per_topic)} |")
    lines.append(f"| full-recall searches | {full} / {n_searchers} |")
    lines.append("")

    by_topic_results = collections.defaultdict(list)
    for r in results:
        by_topic_results[r["topic"]].append(r)

    # Coverage
    lines.append("## Post-register-wait coverage\n")
    lines.append("| topic | registrants visible | fan-out min | med | max |")
    lines.append("|---:|---:|---:|---:|---:|")
    for r in sorted(per_topic, key=lambda x: -x["target"]):
        t = r["topic"]
        fan = per_topic_fanout(t, cov_by_topic)
        if not fan:
            lines.append(f"| {t} | 0 | — | — | — |")
            continue
        lines.append(f"| {t} | {len(fan)} | {fan[0]} | {fan[len(fan)//2]} | {fan[-1]} |")
    lines.append("")
    lines.append("> *Fan-out is the number of distinct hosts that hold each registrant's ad in their topic table at the moment the registration phase ends.*\n")

    # Per-searcher unique recall
    lines.append("## Per-searcher unique recall\n")
    lines.append("| topic | min | med | max | target | ≥ target |")
    lines.append("|---:|---:|---:|---:|---:|---:|")
    for r in sorted(per_topic, key=lambda x: -x["target"]):
        t = r["topic"]
        uniq, target = unique_recall_per_searcher(by_topic_results[t], cov_by_topic, t)
        if not uniq:
            continue
        atTarget = sum(1 for u in uniq if u >= target) if target > 0 else 0
        lines.append(
            f"| {t} | {uniq[0]} | {uniq[len(uniq)//2]} | {uniq[-1]} | {target} | {atTarget}/{len(uniq)} |"
        )
    lines.append("")

    lines.append("## Figures\n")
    # Figure 01 (topic distribution) is embedded above in the Simulation
    # parameters section and intentionally omitted from this list.
    figs = [
        ("02_time_to_first_cdf", "CDF of time-to-first result across all searchers."),
        ("03_unique_found_over_time", "Per-topic mean ± 1σ of unique registrants discovered over time."),
        ("04_id_space_registrants", "ID-space distribution of registrants admitted to ≥1 registrar (one row per topic)."),
        ("05_id_space_found_vs_missed", "Per-topic discovery coverage across ID space — y is the number of searchers that returned each registrant. Green = found by all, orange = some misses, red = many misses."),
        ("06_fanout_both_views", "Per-topic fan-out two views: (a) per-registrant — how many registrars hold each registrant's ad; (b) per-host — how many registrants each host holds for this topic."),
        ("07_registration_latency_bar", "Mean ± 1σ time to first remote admission per topic (clipped at 0)."),
    ]
    for stem, caption in figs:
        if os.path.exists(os.path.join(out_dir, stem + ".png")):
            lines.append(f"### {stem}\n")
            lines.append(f"![{stem}]({stem}.png)\n")
            lines.append(f"*{caption}*\n")
    lines.append("")

    with open(path, "w") as f:
        f.write("\n".join(lines))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("metrics_json")
    ap.add_argument("--out-dir", default=None)
    ap.add_argument("--label", default=None)
    ap.add_argument("--params", nargs="*", default=[],
                    help="key=value pairs stamped into the report's parameters table")
    ap.add_argument("--max-node-idx", type=int, default=0,
                    help="if >0, drop searcher results with nodeIdx >= this (excludes mid-run churn joiners; keeps stable nodes)")
    args = ap.parse_args()

    label = args.label or os.path.splitext(os.path.basename(args.metrics_json))[0]
    out = args.out_dir or f"./figures-{label}"
    os.makedirs(out, exist_ok=True)

    data = load(args.metrics_json)
    per_topic = data["perTopic"]
    results = data["results"]
    if args.max_node_idx > 0:
        results = [r for r in results if r.get("nodeIdx", 0) < args.max_node_idx]
    cov_by_topic = data.get("registrationCoverage", {}).get("byTopic", {})
    reg_timing = data.get("registrationTimingNs", {})
    groups = topic_groups(per_topic)
    churn = load_churn(args.metrics_json)
    ad_lifetime_s = ((data.get("deadResults") or {}).get("adLifetimeMs") or 900_000) / 1000.0

    num_hosts = max(
        (len(cov_by_topic.get(str(r["topic"]), {}).get("byHost", {}))
         for r in per_topic),
        default=0,
    ) or sum(r["numSearchers"] for r in per_topic)

    params = collections.OrderedDict()
    for p in args.params:
        if "=" in p:
            k, v = p.split("=", 1)
            params[k.strip()] = v.strip()

    # 01 topic distribution
    fig, ax = plt.subplots(figsize=(8, 4))
    plot_topic_distribution(per_topic, ax, label)
    fig.savefig(os.path.join(out, "01_topic_distribution.png"))
    fig.savefig(os.path.join(out, "01_topic_distribution.pdf"))
    plt.close(fig)

    # 02 discovery completeness: distinct peers over time + where searchers finish
    fig = plt.figure(figsize=(13, 4.6), constrained_layout=True)
    ok = plot_recall_reached(per_topic, results, fig, label, groups)
    emit(fig, out, "02_recall_reached", ok)

    # 02b time-to-first CDF
    fig, ax = plt.subplots(figsize=(7.5, 4.4), constrained_layout=True)
    ok = plot_time_to_first_cdf(results, ax, label, groups, churn)
    emit(fig, out, "02b_time_to_first_cdf", ok)

    # 03 time to find 50/90/99% of the topic's registrants
    fig = plt.figure(figsize=(16, 4.8), constrained_layout=True)
    ok = plot_time_to_fraction(per_topic, results, fig, label, groups)
    emit(fig, out, "03_time_to_fraction", ok)

    # 11 discovery rate of long-lived searches
    fig = plt.figure(figsize=(14, 4.8), constrained_layout=True)
    ok = plot_discovery_rate(per_topic, results, fig, label, groups)
    emit(fig, out, "11_discovery_rate", ok)

    # 12 where searches query, and where adaptive searches settle
    fig = plt.figure(figsize=(19, 5), constrained_layout=True)
    ok = plot_search_bucket(per_topic, results, fig, label, groups)
    emit(fig, out, "12_search_bucket", ok)

    # 08 lookup latency: time to F_lookup distinct registrants
    fig, ax = plt.subplots(figsize=(8, 4.6), constrained_layout=True)
    ok = plot_lookup_latency(results, ax, label, groups)
    emit(fig, out, "08_lookup_latency_cdf", ok)

    # 09 registrars contacted per lookup
    fig = plt.figure(figsize=(13, 4.6), constrained_layout=True)
    ok = plot_lookup_contacts(results, fig, label, groups)
    emit(fig, out, "09_lookup_contacts_cdf", ok)

    # 10 dead results (churn runs)
    fig = plt.figure(figsize=(13, 4.6), constrained_layout=True)
    ok = plot_dead_results(data.get("deadResults"), fig, label, groups)
    emit(fig, out, "10_dead_results", ok)

    tpos = topic_positions(data)

    # 04 ID-space registrants, by how widely each placed its ad
    fig = plt.figure(figsize=(10, 2.0 * len(groups) + 1.5), constrained_layout=True)
    ok = plot_id_space_registrants(per_topic, cov_by_topic, fig, label, groups, tpos)
    emit(fig, out, "04_id_space_registrants", ok)

    # 04b ID-space registrars, by how many ads each holds
    fig = plt.figure(figsize=(10, 2.0 * len(groups) + 1.5), constrained_layout=True)
    ok = plot_id_space_registrars(per_topic, cov_by_topic, fig, label, groups, tpos)
    emit(fig, out, "04b_id_space_registrars", ok)

    # 07 / 07b placement time across the ID space
    tmap = topic_index_map(data)
    for mode, stem in (("min", "07_placement_time_idspace"),):
        fig = plt.figure(figsize=(10, 2.0 * len(groups) + 1.5), constrained_layout=True)
        ok = plot_placement_time_idspace(per_topic, reg_timing, tmap, fig, label, groups, tpos,
                                         data.get("registrationStartNs"),
                                         data.get("registrationPlacements"), mode)
        if ok:
            fig.savefig(os.path.join(out, stem + ".png"))
            fig.savefig(os.path.join(out, stem + ".pdf"))
        plt.close(fig)

    # 05 ID-space found-vs-missed grid
    fig = plt.figure(figsize=(10, 1.7 * len(groups) + 1.5))
    plot_id_space_found_vs_missed(per_topic, results, cov_by_topic, fig, label, groups, tpos, churn, ad_lifetime_s)
    fig.tight_layout(rect=[0, 0, 1, 0.96])
    fig.savefig(os.path.join(out, "05_id_space_found_vs_missed.png"))
    fig.savefig(os.path.join(out, "05_id_space_found_vs_missed.pdf"))
    plt.close(fig)

    # 06 fan-out, both views
    fig = plt.figure(figsize=(13, 4.5))
    plot_fanout_both_views(per_topic, cov_by_topic, fig, label, num_hosts, groups)
    fig.tight_layout(rect=[0, 0, 1, 0.95])
    fig.savefig(os.path.join(out, "06_fanout_both_views.png"))
    fig.savefig(os.path.join(out, "06_fanout_both_views.pdf"))
    plt.close(fig)

    # 07 registration latency bar (only if probe data is present)
    has_reg_timing = bool(reg_timing) and any(v for v in reg_timing.values())
    if has_reg_timing:
        fig, ax = plt.subplots(figsize=(8, 4.5))
        ok = plot_registration_latency_bar(per_topic, reg_timing, ax, label, groups,
                                           topic_index_map(data),
                                           data.get("registrationStartNs"))
        emit(fig, out, "07_registration_latency_bar", ok)
    else:
        print(f"[{label}] skipping figure 07 (no registrationTimingNs in metrics — predates instrumentation)")

    # Markdown report
    write_report(out, label, params, per_topic, results, cov_by_topic, reg_timing, num_hosts)

    # Summary
    n_searchers = sum(r["numSearchers"] for r in per_topic)
    full = sum(r["fullRecall"] for r in per_topic)
    print(f"[{label}] {n_searchers} searchers, {full}/{n_searchers} full recall across {len(per_topic)} topics")
    print(f"figures + report in: {out}")


if __name__ == "__main__":
    main()
