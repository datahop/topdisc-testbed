#!/usr/bin/env python3
"""Figures for the overhead time series, registrar wait times, and per-registrant
discovery time across the ID space.

Usage:
    figures_overhead.py <overhead-series.json> [--metrics metrics.json]
                        [--out-dir DIR] [--label LABEL]

The series file comes from `-overhead-series-out`; the optional metrics file
(`-metrics-out`) adds the ID-space discovery-time figure, which needs the
index-aligned uniqueFoundIds/uniqueFoundAtMs pair.
"""

import argparse
import json
import os

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402
import numpy as np  # noqa: E402

DPI = 130


def load(path):
    with open(path) as fd:
        return json.load(fd)


def save(fig, out, stem):
    for ext in ("png", "pdf"):
        fig.savefig(os.path.join(out, f"{stem}.{ext}"), dpi=DPI, bbox_inches="tight")
    plt.close(fig)


def rates(samples, key, nbuckets):
    """Convert cumulative per-bucket totals into per-second rates.

    Returns (times, rate_matrix) where rate_matrix[i] is the rate during the
    interval ending at times[i], per ID-space bucket.
    """
    ts = [s["tSec"] for s in samples]
    cum = np.array([s[key] for s in samples], dtype=float)
    if len(ts) < 2:
        return [], np.zeros((0, nbuckets))
    dt = np.diff(ts).reshape(-1, 1)
    dt[dt <= 0] = np.nan
    return ts[1:], np.diff(cum, axis=0) / dt


def peak_rate(samples, key, msgtype=None, window=3):
    """Peak per-node byte rate per ID-space bucket, moving-average smoothed.

    Totals over a run scale with how long the run was, which is a property of
    the harness rather than the protocol. The peak sustained rate is not: it
    says how hard a node in that part of the ID space is worked when the system
    is busy. Rates are divided by the live node count in each bucket, so the
    result is per node regardless of how unevenly the keyspace is populated.
    """
    ts = np.array([s["tSec"] for s in samples], dtype=float)
    if ts.size < 2:
        return None
    if msgtype is None:
        cum = np.array([s[key] for s in samples], dtype=float)
    else:
        nb = len(samples[0][key])
        cum = np.array([(s.get("byType") or {}).get(msgtype, {}).get(key, [0] * nb)
                        for s in samples], dtype=float)
    counts = np.array([s.get("nodes") or [] for s in samples], dtype=float)
    if counts.shape != cum.shape:
        return None
    dt = np.diff(ts).reshape(-1, 1)
    dt[dt <= 0] = np.nan
    rate = np.diff(cum, axis=0) / dt                      # bytes/s per bucket
    live = np.maximum(counts[1:], 1.0)
    per_node = rate / live                                # bytes/s per node
    if window > 1 and per_node.shape[0] >= window:
        k = np.ones(window) / window
        per_node = np.apply_along_axis(
            lambda c: np.convolve(c, k, mode="valid"), 0, per_node)
    return np.nanmax(per_node, axis=0)


def bucket_centres(n):
    return (np.arange(n) + 0.5) / n


def plot_idspace_peak_rate(samples, out, label, tpos=None, window=3):
    """Peak per-node throughput across the ID space, sent and received."""
    tx = peak_rate(samples, "txBytes", window=window)
    rx = peak_rate(samples, "rxBytes", window=window)
    if tx is None or rx is None:
        return
    xs = bucket_centres(tx.size)
    fig, ax = plt.subplots(figsize=(10, 4.6), constrained_layout=True)
    ax.plot(xs, tx / 1e3, color="tab:blue", marker="o", ms=3, label="sent")
    ax.plot(xs, rx / 1e3, color="tab:orange", marker="o", ms=3, label="received")
    for t, pos in sorted((tpos or {}).items()):
        ax.axvline(pos, color="#7B1FA2", ls=":", linewidth=1.4, zorder=0)
        ax.annotate(f"t{t}", (pos, ax.get_ylim()[1]), fontsize=7, color="#7B1FA2",
                    ha="center", va="bottom")
    ax.set_xlim(0, 1)
    ax.set_ylim(bottom=0)
    ax.set_xlabel("node ID position (top 64 bits, normalised 0..1)")
    ax.set_ylabel("peak per-node rate (kB/s)")
    ax.set_title(f"{label}: peak sustained per-node throughput across the ID space "
                 f"({window}-sample moving average)")
    ax.grid(alpha=0.3)
    ax.legend(fontsize=8)
    save(fig, out, "oh_02_idspace_peak_rate")


def plot_idspace_peak_msgtype(samples, out, label, tpos=None, window=3):
    """Peak per-node throughput by message type, across the ID space."""
    types = sorted({t for s in samples for t in (s.get("byType") or {})})
    if not types:
        return
    # Peak rates need the per-bucket node counts the sampler records; a series
    # written before that existed yields nothing, and an all-blank grid of axes
    # is worse than no figure at all.
    if peak_rate(samples, "rxBytes", types[0], window) is None:
        return
    fig, axes = plt.subplots(len(types), 1, figsize=(10, 1.9 * len(types)),
                             sharex=True, squeeze=False, constrained_layout=True)
    plotted = False
    for ax, t in zip(axes[:, 0], types):
        tx = peak_rate(samples, "txBytes", t, window)
        rx = peak_rate(samples, "rxBytes", t, window)
        if tx is None or rx is None:
            continue
        plotted = True
        xs = bucket_centres(tx.size)
        ax.plot(xs, tx / 1e3, color="tab:blue", marker="o", ms=2.5, label="sent")
        ax.plot(xs, rx / 1e3, color="tab:orange", marker="o", ms=2.5, label="received")
        for pos in (tpos or {}).values():
            ax.axvline(pos, color="#7B1FA2", ls=":", linewidth=1.3, zorder=0)
        ax.set_ylabel(f"{t}\n(kB/s)", fontsize=7.5)
        ax.set_ylim(bottom=0)
        ax.grid(alpha=0.3)
        ax.legend(fontsize=7, loc="upper right")
    axes[-1, 0].set_xlim(0, 1)
    axes[-1, 0].set_xlabel("node ID position (top 64 bits, normalised 0..1)")
    if not plotted:
        plt.close(fig)
        return
    axes[0, 0].set_title(f"{label}: peak per-node throughput by message type "
                         f"({window}-sample moving average; dotted lines: topics)")
    save(fig, out, "oh_04_idspace_peak_msgtype")


def plot_load_vs_topic_distance(nodes, metrics, out, label):
    """Per-node load against log-distance to the topic ID, received and sent.

    Distance is discv5's log2 XOR distance, so bucket 256 is the far half of
    the keyspace and small numbers are the neighbourhood that stores the
    topic's ads. Received and sent are separate panels because registrars near
    a topic mostly answer, so their load shows on the sent side.
    """
    tmap = topic_index_map(metrics)
    if not tmap or not nodes:
        return
    topic_ids = {idx: int(h, 16) for h, idx in tmap.items()}
    fig, axes = plt.subplots(2, 1, figsize=(10, 8), sharex=True, constrained_layout=True)
    plotted = False
    for ax, key, what in ((axes[0], "rxBytes", "received"), (axes[1], "txBytes", "sent")):
        for t, tid in sorted(topic_ids.items()):
            xs, ys = [], []
            for n in nodes:
                nid = n.get("id")
                if not nid:
                    continue
                xs.append((int(nid, 16) ^ tid).bit_length())  # 0 = identical, 256 = farthest
                ys.append(n.get(key, 0) / 1e6)
            if not xs:
                continue
            xs, ys = np.asarray(xs), np.asarray(ys)
            buckets = sorted(set(xs.tolist()))
            ax.plot(buckets, [float(np.median(ys[xs == b])) for b in buckets], marker="o", ms=3,
                    linewidth=1.5, color=plt.cm.tab10(t % 10), label=f"topic {t} (median)")
            plotted = True
        ax.set_ylabel(f"{what} per node (MB)")
        ax.set_title(what.capitalize())
        ax.grid(alpha=0.3)
        ax.legend(fontsize=8)
    if not plotted:
        plt.close(fig)
        return
    axes[1].set_xlabel("log2 XOR distance from node to topic ID (smaller = closer)")
    fig.suptitle(f"{label}: does load depend on distance to the topic?")
    save(fig, out, "oh_07_load_vs_topic_distance")


# Message types attributed to each side of the protocol. Registration is the
# REGTOPIC/TICKET/REGCONFIRMATION exchange; lookup is TOPICQUERY and the
# TOPICNODES responses it draws. NODES/FINDNODE/PING/PONG are DHT maintenance
# that both sides rely on, so they are reported separately rather than being
# attributed to either.
REG_TYPES = ("REGTOPIC/v5", "REGTOPIC(renewal)/v5", "REGCONFIRMATION/v5")
LOOKUP_TYPES = ("TOPICQUERY/v5", "TOPICNODES/v5")


def plot_cache_utilisation(samples, out, label):
    """Figure 1: ad-cache utilisation over time.

    The waiting-time function is driven by how full the ad cache is, so this is
    the state underlying every quoted wait. Plotted network-wide as a fill
    fraction, with the per-topic share of occupied slots beneath it.
    """
    pts = [(s["tSec"], s.get("cacheHeld", 0), s.get("cacheCap", 0), s.get("cacheByTopic") or {})
           for s in samples if s.get("cacheCap")]
    if not pts:
        return
    ts = [p[0] for p in pts]
    fill = [100.0 * p[1] / p[2] for p in pts]
    topics = sorted({t for _, _, _, bt in pts for t in bt})

    fig, (ax, ax2) = plt.subplots(2, 1, figsize=(10, 6.5), sharex=True,
                                  constrained_layout=True)
    ax.plot(ts, fill, color="tab:blue", linewidth=1.8)
    ax.set_ylabel("ad cache full (%)")
    # Scale to the data, not to 100%: at these cache sizes occupancy stays far
    # below capacity, and a fixed 0-100 axis renders that as a flat line at zero
    # rather than as the finding it is.
    peak = max(fill)
    ax.set_ylim(0, max(peak * 1.35, 0.01))
    ax.annotate(f"peak {peak:.2f}% of capacity — the occupancy term in the\n"
                f"waiting-time function barely engages at this cache size",
                xy=(0.99, 0.06), xycoords="axes fraction", ha="right", fontsize=8,
                color="#57656E")
    ax.grid(alpha=0.3)
    ax.set_title(f"{label}: ad-cache utilisation over time")
    for i, t in enumerate(topics):
        ax2.plot(ts, [bt.get(t, 0) for _, _, _, bt in pts],
                 color=plt.cm.tab10(i % 10), linewidth=1.5, label=f"topic {t[:10]}")
    ax2.set_xlabel("time since spawn (s)")
    ax2.set_ylabel("ads held (network-wide)")
    ax2.grid(alpha=0.3)
    if topics:
        ax2.legend(fontsize=8, ncol=2)
    save(fig, out, "oh_08_cache_utilisation")


def _per_topic_sizes(metrics):
    return {r["topic"]: r.get("numSearchers", 0) for r in metrics.get("perTopic", [])}


def plot_cost_per_lookup(nodes, metrics, out, label):
    """Figure 5: lookup traffic per searcher, against topic popularity.

    Total lookup bytes and messages divided by the number of searchers, plotted
    against how many nodes hold the topic. Answers whether a lookup gets more
    expensive as a topic becomes more popular.
    """
    sizes = _per_topic_sizes(metrics)
    if not sizes or not nodes:
        return
    tot_b = tot_m = 0
    for n in nodes:
        for t in LOOKUP_TYPES:
            c = (n.get("byType") or {}).get(t)
            if c:
                tot_b += c.get("txBytes", 0) + c.get("rxBytes", 0)
                tot_m += c.get("txMsgs", 0) + c.get("rxMsgs", 0)
    searchers = sum(sizes.values())
    if not searchers:
        return
    xs = [sizes[t] for t in sorted(sizes)]
    # Traffic is only resolved network-wide, so attribute it to topics in
    # proportion to their searcher count and report the per-searcher cost.
    per_b = [tot_b / searchers / 1e3] * len(xs)
    fig, ax = plt.subplots(figsize=(9, 4.6), constrained_layout=True)
    ax.bar([str(t) for t in sorted(sizes)], [sizes[t] for t in sorted(sizes)],
           color="tab:blue", alpha=0.35, label="searchers on topic")
    ax2 = ax.twinx()
    ax2.plot([str(t) for t in sorted(sizes)], per_b, color="tab:red", marker="o",
             linewidth=1.8, label=f"lookup traffic per searcher ({per_b[0]:.1f} kB)")
    ax.set_xlabel("topic")
    ax.set_ylabel("searchers on topic")
    ax2.set_ylabel("lookup kB per searcher")
    ax.set_title(f"{label}: lookup cost per searcher vs topic popularity "
                 f"({tot_m / max(searchers, 1):.0f} msgs/searcher overall)")
    ax.grid(alpha=0.3)
    ax.legend(fontsize=8, loc="upper right")
    ax2.legend(fontsize=8, loc="upper center")
    save(fig, out, "oh_09_cost_per_lookup")


def plot_reg_vs_lookup_overhead(nodes, out, label, tpos=None):
    """Figure 6: registration vs lookup traffic, per node.

    Each node's bytes split into the registration exchange and the lookup
    exchange, across the ID space. Shows which half of the protocol dominates
    and whether that changes with position relative to the topics.
    """
    xs, reg, look, dht = [], [], [], []
    for n in nodes:
        bt = n.get("byType") or {}
        if not bt or not n.get("id"):
            continue
        def tot(types):
            return sum((bt.get(t) or {}).get("txBytes", 0) + (bt.get(t) or {}).get("rxBytes", 0)
                       for t in types)
        other = sum(v.get("txBytes", 0) + v.get("rxBytes", 0) for k, v in bt.items()
                    if k not in REG_TYPES and k not in LOOKUP_TYPES)
        xs.append(id_pos(n["id"]))
        reg.append(tot(REG_TYPES) / 1e6)
        look.append(tot(LOOKUP_TYPES) / 1e6)
        dht.append(other / 1e6)
    if not xs:
        return
    fig, ax = plt.subplots(figsize=(10, 5), constrained_layout=True)
    ax.scatter(xs, look, s=6, alpha=0.65, color="tab:orange", linewidths=0,
               rasterized=True, label=f"lookup (median {np.median(look):.2f} MB)")
    ax.scatter(xs, reg, s=6, alpha=0.65, color="tab:blue", linewidths=0,
               rasterized=True, label=f"registration (median {np.median(reg):.2f} MB)")
    ax.scatter(xs, dht, s=5, alpha=0.35, color="tab:gray", linewidths=0,
               rasterized=True, label=f"DHT upkeep (median {np.median(dht):.2f} MB)")
    for pos in (tpos or {}).values():
        ax.axvline(pos, color="#7B1FA2", ls=":", linewidth=1.4, zorder=5)
    ax.set_xlim(0, 1)
    ax.set_yscale("symlog", linthresh=0.01)
    ax.set_ylim(bottom=0)  # traffic is never negative; symlog would draw the decades anyway
    ax.set_xlabel("node ID position (top 64 bits, normalised 0..1)")
    ax.set_ylabel("traffic per node (MB, symlog)")
    ax.set_title(f"{label}: registration vs lookup traffic per node")
    ax.grid(alpha=0.3)
    ax.legend(fontsize=8, markerscale=2)
    save(fig, out, "oh_10_reg_vs_lookup")


def id_pos(hex_id):
    return int(hex_id[:16], 16) / float(2 ** 64)


def plot_idspace_traffic(nodes, out, label, tpos=None):
    """Total bytes sent and received per node, across the ID space.

    One dot per node rather than a time series: the run's traffic profile over
    time is an artefact of the harness's phase schedule, whereas where the load
    falls in the ID space is a property of the protocol.
    """
    pts = [(id_pos(n["id"]), n.get("txBytes", 0) / 1e6, n.get("rxBytes", 0) / 1e6)
           for n in nodes if n.get("id")]
    if not pts:
        return
    xs = [p[0] for p in pts]
    fig, axes = plt.subplots(2, 1, figsize=(10, 7), sharex=True, constrained_layout=True)
    for ax, idx, name, colour in ((axes[0], 1, "sent", "tab:blue"),
                                  (axes[1], 2, "received", "tab:orange")):
        ys = [p[idx] for p in pts]
        # 10k overlapping points at low alpha vanish; keep the markers small but
        # solid enough to read, and let density show through mild transparency.
        ax.scatter(xs, ys, s=7, alpha=0.75, color=colour, linewidths=0, rasterized=True)
        med = float(np.median(ys))
        ax.axhline(med, color="#444", ls="--", linewidth=1, label=f"median {med:.1f} MB")
        for t, pos in sorted((tpos or {}).items()):
            ax.axvline(pos, color="#7B1FA2", ls=":", linewidth=1.4, zorder=0,
                       label=f"topic {t}" if ax is axes[0] else None)
        ax.set_ylabel(f"{name} (MB)")
        ax.grid(alpha=0.3)
        ax.legend(fontsize=8, loc="upper right")
    axes[-1].set_xlim(0, 1)
    axes[-1].set_xlabel("node ID position (top 64 bits, normalised 0..1)")
    axes[0].set_title(f"{label}: per-node traffic across the ID space")
    save(fig, out, "oh_01_idspace_traffic")


def plot_idspace_msgtype(nodes, out, label, tpos=None):
    """Per-node bytes by message type, across the ID space.

    One panel per discv5 message type, sent and received overlaid, so a
    message whose cost concentrates near a topic is visible as a spike at that
    topic's position rather than being averaged away.
    """
    types = sorted({t for n in nodes for t in (n.get("byType") or {})})
    if not types:
        return
    fig, axes = plt.subplots(len(types), 1, figsize=(10, 1.9 * len(types)),
                             sharex=True, squeeze=False, constrained_layout=True)
    for ax, t in zip(axes[:, 0], types):
        xs, tx, rx = [], [], []
        for n in nodes:
            c = (n.get("byType") or {}).get(t)
            if not c or not n.get("id"):
                continue
            xs.append(id_pos(n["id"]))
            tx.append(c.get("txBytes", 0) / 1e6)
            rx.append(c.get("rxBytes", 0) / 1e6)
        if not xs:
            continue
        ax.scatter(xs, tx, s=6, alpha=0.7, color="tab:blue", linewidths=0,
                   rasterized=True, label="sent")
        ax.scatter(xs, rx, s=6, alpha=0.7, color="tab:orange", linewidths=0,
                   rasterized=True, label="received")
        for pos in (tpos or {}).values():
            ax.axvline(pos, color="#7B1FA2", ls=":", linewidth=1.3, zorder=0)
        ax.set_ylabel(f"{t}\n(MB)", fontsize=8)
        ax.grid(alpha=0.3)
        ax.legend(fontsize=7, loc="upper right", markerscale=2)
    axes[-1, 0].set_xlim(0, 1)
    axes[-1, 0].set_xlabel("node ID position (top 64 bits, normalised 0..1)")
    axes[0, 0].set_title(f"{label}: per-node traffic by message type across the ID space "
                         "(dotted lines: topic positions)")
    save(fig, out, "oh_03_idspace_msgtype")


def plot_wait_times(waits, out, label, tmap=None):
    """(a) CDF of registrar-quoted waiting times; (b) CDF of the total wait of
    every successful registration, from its first REGTOPIC to admission."""
    waits = [w for w in waits if w.get("quotedMs") or w.get("admittedMs")]
    if not waits:
        return
    tmap = tmap or {}

    def name(w):
        return f"topic {tmap[w['topic']]}" if w["topic"] in tmap else w["topic"][:10]

    fig, (ax_q, ax_a) = plt.subplots(1, 2, figsize=(14, 4.6), constrained_layout=True)
    for w in sorted(waits, key=lambda w: (tmap.get(w["topic"], 1 << 30), w["topic"])):
        color = plt.cm.tab10(tmap[w["topic"]] % 10) if w["topic"] in tmap else None
        for ax, key in ((ax_q, "quotedMs"), (ax_a, "admittedMs")):
            v = np.sort(np.array(w.get(key) or [], dtype=float)) / 1000.0
            if v.size:
                ax.plot(v, np.arange(1, v.size + 1) / v.size, color=color,
                        label=f"{name(w)} (n={v.size}, median {np.median(v):.0f}s)")
    ax_q.set_xlabel("quoted wait time (s)")
    ax_q.set_ylabel("CDF over quotes")
    ax_q.set_title("Waiting times quoted by registrars")
    ax_a.set_xlabel("total wait until admission (s)")
    ax_a.set_ylabel("CDF over successful registrations")
    ax_a.set_title("Total time of each registration")
    for ax in (ax_q, ax_a):
        ax.grid(True, alpha=0.3)
        ax.set_xlim(left=0)
        ax.set_ylim(0, 1)
        ax.legend(fontsize=8, loc="lower right")
    fig.suptitle(f"{label}: registration waiting times")
    save(fig, out, "oh_05_wait_time_cdf")

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


def plot_idspace_found_time(metrics, out, label):
    """Per-registrant discovery latency across the ID space.

    Measured from when the registrant's ad was first admitted on a remote
    registrar (registrationTimingNs), not from a searcher's own start: each
    searcher begins at a different moment, so per-searcher elapsed times are
    not comparable across searchers and their median means little. Putting
    both events on the run's common clock -- searchStartMs + uniqueFoundAtMs
    for the discovery, registrationTimingNs for the ad -- gives the quantity
    that actually matters: how long an advertisement takes to become findable.

    Falls back to searcher-relative time when the registration probe produced
    no timing data, and says so in the axis label.
    """
    results = metrics.get("results", [])
    per_topic = metrics.get("perTopic", [])
    cov = metrics.get("registrationCoverage", {}).get("byTopic", {})
    reg_timing = metrics.get("registrationTimingNs", {})

    # topic index -> {short registrant id: ad-placed ms on the common clock}
    tmap = topic_index_map(metrics)
    tpos = {idx: int(h[:16], 16) / float(2 ** 64) for h, idx in tmap.items()}
    placed_by_topic = {}
    for topic_hex, regs in reg_timing.items():
        t = tmap.get(topic_hex)
        if t is None:
            continue
        placed_by_topic[t] = {rid[:16]: ns / 1e6 for rid, ns in regs.items()}
    # Ad-placement mode needs both halves on the common clock. Without
    # searchStartMs the discovery side is still searcher-relative (small) while
    # the ad side is measured from regStart (large), so every difference is
    # negative and clamps to zero -- a field of zeros that looks like data.
    have_offsets = any("searchStartMs" in r for r in results)
    have_placement = bool(any(placed_by_topic.values()) and have_offsets)

    by_topic = {}
    for r in results:
        ids = r.get("uniqueFoundIds") or []
        ts = r.get("uniqueFoundAtMs") or []
        if not ids:
            continue
        offset = r.get("searchStartMs", 0) if have_placement else 0
        placed = placed_by_topic.get(r["topic"], {})
        d = by_topic.setdefault(r["topic"], {})
        for rid, t_ms in zip(ids, ts):
            found_ms = offset + t_ms
            if have_placement:
                ad_ms = placed.get(rid[:16])
                if ad_ms is None:
                    continue  # never admitted remotely; nothing to measure from
                # From when the ad could first be found: placed and the search running.
                latency = (found_ms - max(ad_ms, offset)) / 1000.0
                if latency < 0:
                    latency = 0.0
            else:
                latency = t_ms / 1000.0
            d.setdefault(rid, []).append(latency)
    if not by_topic:
        return

    topics = sorted(t["topic"] for t in per_topic) or sorted(by_topic)
    fig, axes = plt.subplots(
        len(topics), 1, figsize=(9, 2.4 * len(topics)), sharex=True, squeeze=False
    )
    for ax, t in zip(axes[:, 0], topics):
        found = by_topic.get(t, {})
        admitted = list(cov.get(str(t), {}).get("byRegistrant", {}).keys())
        short = {a[:16] for a in admitted}
        xs, ys = [], []
        for rid, lat in found.items():
            xs.append(int(rid[:16], 16) / float(2**64))
            ys.append(float(np.median(lat)))
        if xs:
            ax.scatter(xs, ys, s=10, alpha=0.6, color="tab:blue",
                       label="median over searchers that found it")
        missed = [m for m in short if m not in found]
        if missed:
            top = max(ys) * 1.08 if ys else 1.0
            ax.scatter([int(m[:16], 16) / float(2**64) for m in missed],
                       [top] * len(missed), s=26, marker="x", color="#C2185B",
                       linewidths=1.3, label=f"never found ({len(missed)})")
        if t in tpos:
            ax.axvline(tpos[t], color="#7B1FA2", ls=":", linewidth=1.6, zorder=0,
                       label=f"topic ID position ({tpos[t]:.3f})")
        ax.set_ylabel(f"topic {t}\ntime (s)", fontsize=8)
        ax.set_ylim(bottom=0)  # a latency is never negative; keep autoscale from implying it
        ax.grid(True, alpha=0.3)
        ax.legend(fontsize=7, loc="lower right")
    axes[-1, 0].set_xlabel("registrant ID position (top 64 bits, normalised 0..1)")
    axes[0, 0].set_title(
        f"{label}: time from ad placement or search start (whichever later) to first discovery"
        if have_placement else
        f"{label}: time to discovery (searcher-relative; no ad-placement data)"
    )
    save(fig, out, "oh_06_idspace_found_time")


def _dist_stats(vals):
    v = np.asarray(vals, dtype=float)
    if v.size == 0:
        return None
    mean = v.mean()
    return {"n": int(v.size), "median": float(np.median(v)), "p95": float(np.percentile(v, 95)),
            "p99": float(np.percentile(v, 99)), "max": float(v.max()),
            "cv": float(v.std() / mean) if mean else 0.0}


def plot_topic_load(nodes, metrics, out, label):
    """Load distribution per topic, and its summary statistics.

    Registrar side, from each node's per-topic request counters: REGTOPIC and
    TOPICQUERY requests received for the topic, and the bytes of the replies
    sent. Requester side, from each node's per-operation counters: the bytes
    its registration and search for its own topic sent and received. Writes
    load_summary.md with median, p95, p99, max and coefficient of variation,
    over all nodes for the registrar side and over the topic's members for
    the requester side.
    """
    tmap = topic_index_map(metrics)
    if not nodes or not tmap or not any(n.get("topicLoad") or n.get("ops") for n in nodes):
        return
    node_topic = {r["nodeIdx"]: r["topic"] for r in metrics.get("results", []) if "nodeIdx" in r}
    reqs = {t: [] for t in tmap.values()}
    reply_kb = {t: [] for t in tmap.values()}
    member_kb = {t: [] for t in tmap.values()}
    for n in nodes:
        load = n.get("topicLoad") or {}
        for hex_id, t in tmap.items():
            tl = load.get(hex_id) or {}
            rg, tq = tl.get("regtopic") or {}, tl.get("topicQuery") or {}
            reqs[t].append(rg.get("rxMsgs", 0) + tq.get("rxMsgs", 0))
            reply_kb[t].append((rg.get("txBytes", 0) + tq.get("txBytes", 0)) / 1e3)
        t = node_topic.get(n.get("idx"))
        if t is not None and n.get("ops"):
            member_kb[t].append(sum(o.get("txBytes", 0) + o.get("rxBytes", 0) for o in n["ops"]) / 1e3)
    panels = (("requests received per registrar", reqs, "msgs"),
              ("reply bytes sent per registrar", reply_kb, "kB"),
              ("registration + lookup traffic per member", member_kb, "kB"))
    fig, axes = plt.subplots(1, 3, figsize=(17, 4.8), constrained_layout=True)
    for ax, (what, data, unit) in zip(axes, panels):
        for t in sorted(data):
            v = np.sort([x for x in data[t] if x > 0])
            if v.size:
                ax.plot(v, np.arange(1, v.size + 1) / v.size, linewidth=1.8, color=plt.cm.tab10(t % 10),
                        label=f"topic {t} ({v.size} nodes, median {np.median(v):.0f})")
        ax.set_xscale("log")
        ax.set_xlabel(f"{what} ({unit})")
        ax.set_ylim(0, 1)
        ax.grid(alpha=0.3, which="both")
        ax.legend(fontsize=8, loc="lower right")
    axes[0].set_ylabel("CDF over nodes with that load")
    fig.suptitle(f"{label}: load distribution per topic")
    save(fig, out, "oh_11_topic_load")

    rows = []
    for what, data, unit in panels:
        for t in sorted(data):
            st = _dist_stats(data[t])
            if st:
                rows.append(f"| {what} | {t} | {unit} | {st['n']} | {st['median']:.1f} | {st['p95']:.1f} | "
                            f"{st['p99']:.1f} | {st['max']:.1f} | {st['cv']:.2f} |")
    tot = _dist_stats([(n.get("txBytes", 0) + n.get("rxBytes", 0)) / 1e6 for n in nodes])
    if tot:
        rows.append(f"| total traffic per node | all | MB | {tot['n']} | {tot['median']:.1f} | {tot['p95']:.1f} | "
                    f"{tot['p99']:.1f} | {tot['max']:.1f} | {tot['cv']:.2f} |")
    with open(os.path.join(out, "load_summary.md"), "w") as f:
        f.write("| load | topic | unit | nodes | median | p95 | p99 | max | CV |\n")
        f.write("|---|---:|---|---:|---:|---:|---:|---:|---:|\n")
        f.write("\n".join(rows) + "\n")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("series_json")
    ap.add_argument("--metrics", default="")
    ap.add_argument("--overhead", default="", help="per-node overhead dump (-overhead-out)")
    ap.add_argument("--window", type=int, default=3,
                    help="moving-average window (samples) for peak-rate figures")
    ap.add_argument("--out-dir", default="")
    ap.add_argument("--label", default="")
    args = ap.parse_args()

    label = args.label or os.path.splitext(os.path.basename(args.series_json))[0]
    out = args.out_dir or f"./figures-{label}"
    os.makedirs(out, exist_ok=True)

    data = load(args.series_json)
    samples = data.get("samples", [])
    metrics = load(args.metrics) if args.metrics else {}

    plot_wait_times(data.get("waitTime") or [], out, label, topic_index_map(metrics) if metrics else {})
    plot_cache_utilisation(samples, out, label)
    tpos = ({idx: int(h[:16], 16) / float(2 ** 64)
             for h, idx in topic_index_map(metrics).items()} if metrics else {})
    plot_idspace_peak_rate(samples, out, label, tpos, args.window)
    plot_idspace_peak_msgtype(samples, out, label, tpos, args.window)
    if args.overhead and os.path.exists(args.overhead):
        nodes = load(args.overhead)
        plot_idspace_traffic(nodes, out, label, tpos)
        plot_idspace_msgtype(nodes, out, label, tpos)
        plot_load_vs_topic_distance(nodes, metrics, out, label)
        plot_reg_vs_lookup_overhead(nodes, out, label, tpos)
        if metrics:
            plot_cost_per_lookup(nodes, metrics, out, label)
            plot_topic_load(nodes, metrics, out, label)
    if metrics:
        plot_idspace_found_time(metrics, out, label)

    print(f"figures written to {out}")


if __name__ == "__main__":
    main()
