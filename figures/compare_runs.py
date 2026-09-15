#!/usr/bin/env python3
"""Compare runs of the same scenario on different backends.

Usage:
    compare_runs.py --out DIR [--title TITLE] LABEL=RUN_DIR [LABEL=RUN_DIR ...]

Reads metrics.json, series.json and oh.json from each run directory and writes
<DIR>/report.md with a summary table and overlay figures in <DIR>/figures:
one line style per run, so a backend difference shows as a gap between curves.
"""
import argparse
import collections
import json
import os

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402
import numpy as np  # noqa: E402

STYLES = ["-", "--", ":", "-."]
REG = ("REGTOPIC/v5", "REGTOPIC(renewal)/v5", "REGCONFIRMATION/v5")
LOOKUP = ("TOPICQUERY/v5", "TOPICNODES/v5")


def load(path):
    if not os.path.exists(path):
        return {}
    with open(path) as f:
        return json.load(f)


def pct(v, p):
    return float(np.percentile(v, p)) if len(v) else float("nan")


def cdf(ax, values, **kw):
    v = np.sort(np.asarray(values, dtype=float))
    if v.size:
        ax.plot(v, np.arange(1, v.size + 1) / v.size, **kw)


class Run:
    def __init__(self, label, rd):
        self.label, self.dir = label, rd
        self.m = load(os.path.join(rd, "metrics.json"))
        self.s = load(os.path.join(rd, "series.json"))
        self.oh = load(os.path.join(rd, "oh.json")) or []
        self.results = [r for r in self.m.get("results", []) if r.get("nodeId")]

    def lookups(self):
        """Latency of lookups that reached F_lookup, and per-lookup contacts."""
        lat, nodes, queries, total, hit = [], [], [], 0, 0
        for r in self.results:
            f = min(r.get("fLookup") or 30, r.get("target") or 30)
            for l, n in zip(r.get("lookupLatencyMs") or [], r.get("lookupResults") or []):
                total += 1
                if n >= f:
                    hit += 1
                    lat.append(l / 1000.0)
            nodes += r.get("lookupContacted") or []
            queries += r.get("lookupQueries") or []
        return lat, nodes, queries, total, hit

    def fanout(self):
        cov = (self.m.get("registrationCoverage") or {}).get("byTopic") or {}
        return [v for t in cov.values() for v in (t.get("byRegistrant") or {}).values()]

    def waits(self, key):
        return [x / 1000.0 for w in self.s.get("waitTime") or [] for x in (w.get(key) or [])]

    def traffic(self, key):
        return [n.get(key, 0) / 1e6 for n in self.oh]

    def type_share(self):
        tot = collections.Counter()
        for n in self.oh:
            for t, c in (n.get("byType") or {}).items():
                tot[t] += c.get("txBytes", 0)
        s = sum(tot.values()) or 1
        return {t: v / s for t, v in tot.items()}

    def recall(self):
        pt = self.m.get("perTopic") or []
        n = sum(r["numSearchers"] for r in pt)
        return sum(r["meanRecall"] * r["numSearchers"] for r in pt) / n if n else float("nan")


def fig_path(out, stem):
    return os.path.join(out, "figures", stem + ".png")


def save(fig, out, stem):
    fig.savefig(fig_path(out, stem), dpi=130, bbox_inches="tight")
    plt.close(fig)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--title", default="")
    ap.add_argument("--note", action="append", default=[], help="caveat printed under the run list")
    ap.add_argument("runs", nargs="+", help="LABEL=RUN_DIR")
    args = ap.parse_args()
    runs = [Run(*r.split("=", 1)) for r in args.runs]
    os.makedirs(os.path.join(args.out, "figures"), exist_ok=True)
    figs = []

    # Discovery.
    fig, (ax_l, ax_n) = plt.subplots(1, 2, figsize=(13, 4.6), constrained_layout=True)
    for i, r in enumerate(runs):
        lat, nodes, _, total, hit = r.lookups()
        cdf(ax_l, lat, ls=STYLES[i % 4], lw=1.8, label=f"{r.label} (median {pct(lat, 50):.2f}s, {100 * hit / max(total, 1):.0f}% reach)")
        cdf(ax_n, nodes, ls=STYLES[i % 4], lw=1.8, label=f"{r.label} (median {pct(nodes, 50):.0f})")
    ax_l.set_xlabel("lookup latency to F_lookup (s)")
    ax_n.set_xlabel("distinct nodes contacted per lookup")
    for ax in (ax_l, ax_n):
        ax.set_ylabel("CDF over lookups")
        ax.set_ylim(0, 1)
        ax.set_xlim(left=0)
        ax.grid(alpha=0.3)
        ax.legend(fontsize=9, loc="lower right")
    fig.suptitle("Lookups: latency and registrars contacted")
    save(fig, args.out, "cmp_01_lookups")
    figs.append(("cmp_01_lookups", "Lookup latency to F_lookup distinct registrants (lookups that got there) and distinct nodes contacted per lookup, all topics."))

    # Time to first result counts from the search phase start, so in scheduled
    # runs it mostly measures the wait until each node's first lookup.
    scheduled = any(x.get("lookups") for r in runs for x in r.results)
    fig, ax = plt.subplots(figsize=(8, 4.6), constrained_layout=True)
    for i, r in enumerate([] if scheduled else runs):
        ttf = [x["timeToFirstNs"] / 1e9 for x in r.results if x.get("timeToFirstNs", 0) > 0]
        cdf(ax, ttf, ls=STYLES[i % 4], lw=1.8, label=f"{r.label} (median {pct(ttf, 50):.2f}s)")
    ax.set_xlabel("time to first result (s)")
    ax.set_ylabel("CDF over searchers")
    ax.set_ylim(0, 1)
    ax.set_xlim(left=0)
    ax.grid(alpha=0.3)
    ax.legend(fontsize=9, loc="lower right")
    ax.set_title("Time to first result")
    if scheduled:
        plt.close(fig)
    else:
        save(fig, args.out, "cmp_02_time_to_first")
        figs.append(("cmp_02_time_to_first", "Time from the start of a searcher's search to its first result."))

    topics = sorted({x["topic"] for r in runs for x in r.results})
    fig, axes = plt.subplots(1, len(topics), figsize=(3.4 * len(topics), 3.8), sharey=True, squeeze=False, constrained_layout=True)
    for ax, t in zip(axes[0], topics):
        for i, r in enumerate(runs):
            rs = [x for x in r.results if x["topic"] == t]
            if not rs:
                continue
            target = max(rs[0].get("target") or 1, 1)
            end = max((ts[-1] for x in rs for ts in [x.get("uniqueFoundAtMs") or [0]]), default=0)
            grid = np.linspace(0, max(end, 1), 200)
            curves = [np.searchsorted(np.sort(x.get("uniqueFoundAtMs") or []), grid, side="right") / target for x in rs]
            ax.plot(grid / 1000.0, np.median(np.vstack(curves), axis=0), ls=STYLES[i % 4], lw=1.8, label=r.label)
        ax.set_title(f"topic {t}")
        ax.set_xlabel("search time (s)")
        ax.grid(alpha=0.3)
    axes[0][0].set_ylabel("registrants found (median share)")
    axes[0][0].legend(fontsize=8, loc="lower right")
    fig.suptitle("Share of the topic's registrants found over time")
    save(fig, args.out, "cmp_03_found_over_time")
    figs.append(("cmp_03_found_over_time", "Median share of the topic's registrants each searcher had found, by topic."))

    # Registration.
    fig, (ax_f, ax_q, ax_a) = plt.subplots(1, 3, figsize=(17, 4.6), constrained_layout=True)
    for i, r in enumerate(runs):
        fo, q, a = r.fanout(), r.waits("quotedMs"), r.waits("admittedMs")
        cdf(ax_f, fo, ls=STYLES[i % 4], lw=1.8, label=f"{r.label} (median {pct(fo, 50):.0f})")
        cdf(ax_q, q, ls=STYLES[i % 4], lw=1.8, label=f"{r.label} (median {pct(q, 50):.0f}s)")
        cdf(ax_a, a, ls=STYLES[i % 4], lw=1.8, label=f"{r.label} (median {pct(a, 50):.0f}s)")
    for ax, xl, yl in ((ax_f, "registrars holding each registrant's ad", "CDF over registrants"),
                       (ax_q, "quoted wait (s)", "CDF over quotes"),
                       (ax_a, "total wait until admission (s)", "CDF over registrations")):
        ax.set_xlabel(xl)
        ax.set_ylabel(yl)
        ax.set_ylim(0, 1)
        ax.set_xlim(left=0)
        ax.grid(alpha=0.3)
        ax.legend(fontsize=9, loc="lower right")
    fig.suptitle("Registration: fan-out, quoted waits, total registration time")
    save(fig, args.out, "cmp_04_registration")
    figs.append(("cmp_04_registration", "Fan-out after the register-wait phase, waiting times quoted by registrars, and total time of every successful registration."))

    # Overhead.
    fig, (ax_tx, ax_rx) = plt.subplots(1, 2, figsize=(13, 4.6), constrained_layout=True)
    for i, r in enumerate(runs):
        tx, rx = r.traffic("txBytes"), r.traffic("rxBytes")
        cdf(ax_tx, tx, ls=STYLES[i % 4], lw=1.8, label=f"{r.label} (median {pct(tx, 50):.1f} MB)")
        cdf(ax_rx, rx, ls=STYLES[i % 4], lw=1.8, label=f"{r.label} (median {pct(rx, 50):.1f} MB)")
    for ax, xl in ((ax_tx, "sent per node (MB)"), (ax_rx, "received per node (MB)")):
        ax.set_xscale("log")
        ax.set_xlabel(xl)
        ax.set_ylabel("CDF over nodes")
        ax.set_ylim(0, 1)
        ax.grid(alpha=0.3, which="both")
        ax.legend(fontsize=9, loc="lower right")
    fig.suptitle("Traffic per node")
    save(fig, args.out, "cmp_05_traffic_per_node")
    figs.append(("cmp_05_traffic_per_node", "Bytes sent and received per node over the whole run."))

    shares = [r.type_share() for r in runs]
    types = sorted({t for s in shares for t in s}, key=lambda t: -max(s.get(t, 0) for s in shares))
    fig, ax = plt.subplots(figsize=(12, 4.6), constrained_layout=True)
    w = 0.8 / len(runs)
    x = np.arange(len(types))
    for i, (r, s) in enumerate(zip(runs, shares)):
        ax.bar(x + i * w, [100 * s.get(t, 0) for t in types], w, label=r.label)
    ax.set_xticks(x + w * (len(runs) - 1) / 2, types, rotation=30, ha="right")
    ax.set_ylabel("share of bytes sent (%)")
    ax.grid(alpha=0.3, axis="y")
    ax.legend()
    ax.set_title("Traffic by message type")
    save(fig, args.out, "cmp_06_msgtype_share")
    figs.append(("cmp_06_msgtype_share", "Share of all bytes sent, by message type."))

    topics = sorted({x["topic"] for r in runs for x in r.results})

    def per_topic_panels(stem, title, xlabel, values, caption, logx=False):
        fig, axes = plt.subplots(1, len(topics), figsize=(3.4 * len(topics), 3.8), sharey=True, squeeze=False, constrained_layout=True)
        for ax, t in zip(axes[0], topics):
            for i, r in enumerate(runs):
                v = values(r, t)
                cdf(ax, v, ls=STYLES[i % 4], lw=1.8, label=f"{r.label} ({pct(v, 50):.2f})" if v else r.label)
            ax.set_title(f"topic {t}")
            ax.set_xlabel(xlabel)
            ax.set_ylim(0, 1)
            if logx:
                ax.set_xscale("log")
            else:
                ax.set_xlim(left=0)
            ax.grid(alpha=0.3, which="both")
            ax.legend(fontsize=7, loc="lower right")
        axes[0][0].set_ylabel("CDF")
        fig.suptitle(title)
        save(fig, args.out, stem)
        figs.append((stem, caption))

    def topic_lookups(r, t, key):
        out = []
        for x in r.results:
            if x["topic"] != t:
                continue
            f = min(x.get("fLookup") or 30, x.get("target") or 30)
            if key == "latency":
                out += [l / 1000.0 for l, n in zip(x.get("lookupLatencyMs") or [], x.get("lookupResults") or []) if n >= f]
            else:
                out += x.get(key) or []
        return out

    per_topic_panels("cmp_07_lookup_latency_by_topic", "Lookup latency by topic (topic 0 most popular)", "latency (s)",
                     lambda r, t: topic_lookups(r, t, "latency"),
                     "Lookup latency to F_lookup per topic; the legend gives the median in seconds.")
    per_topic_panels("cmp_08_contacts_by_topic", "Distinct nodes contacted per lookup, by topic", "nodes contacted",
                     lambda r, t: topic_lookups(r, t, "lookupContacted"),
                     "Distinct nodes asked per lookup per topic; the legend gives the median.")

    def found_share(r, t):
        fc = next((f for f in r.m.get("findCountByTopic") or [] if f["topic"] == t), None)
        if not fc:
            return []
        searchers = max(fc["registrants"] - 1, 1)
        return [c / searchers for c in fc["counts"]]

    per_topic_panels("cmp_09_found_by_share", "Share of searchers that found each registrant, by topic", "share of searchers",
                     found_share, "For each registrant, the fraction of its topic's other nodes that found it during the run.")

    # Registration: time to first admission, ads held over time.
    fig, (ax_a, ax_c) = plt.subplots(1, 2, figsize=(13, 4.6), constrained_layout=True)
    for i, r in enumerate(runs):
        start = r.m.get("registrationStartNs") or {}
        first = [(ns - start.get(rid, 0)) / 1e9 for regs in (r.m.get("registrationTimingNs") or {}).values() for rid, ns in regs.items()]
        first = [v for v in first if v >= 0]
        cdf(ax_a, first, ls=STYLES[i % 4], lw=1.8, label=f"{r.label} (median {pct(first, 50):.1f}s)")
        sm = [x for x in r.s.get("samples") or []]
        seen = False
        xs, ys = [], []
        for x in sm:
            if x.get("cacheHeld"):
                seen = True
            elif seen:
                continue  # a final sample taken without polling caches
            xs.append(x["tSec"] / 60)
            ys.append(x.get("cacheHeld", 0))
        ax_c.plot(xs, ys, ls=STYLES[i % 4], lw=1.8, label=r.label)
    ax_a.set_xscale("log")
    ax_a.set_xlabel("time from own registration start to first admission (s)")
    ax_a.set_ylabel("CDF over registrants")
    ax_a.set_ylim(0, 1)
    ax_c.set_xlabel("minutes since the run started")
    ax_c.set_ylabel("ads held network-wide")
    for ax in (ax_a, ax_c):
        ax.grid(alpha=0.3, which="both")
        ax.legend(fontsize=9)
    fig.suptitle("Registration: time to first admission and ads held over time")
    save(fig, args.out, "cmp_10_admission_and_cache")
    figs.append(("cmp_10_admission_and_cache", "Time for each registrant to get its first ad admitted, and total ads held across the network over the run."))

    # Load.
    def registrar_load(r, key):
        out = []
        for n in r.oh:
            tl = n.get("topicLoad") or {}
            out.append(sum((l.get("regtopic") or {}).get(key, 0) + (l.get("topicQuery") or {}).get(key, 0) for l in tl.values()))
        return [v for v in out if v > 0]

    fig, (ax_q, ax_b) = plt.subplots(1, 2, figsize=(13, 4.6), constrained_layout=True)
    for i, r in enumerate(runs):
        q, b = registrar_load(r, "rxMsgs"), [v / 1e3 for v in registrar_load(r, "txBytes")]
        cdf(ax_q, q, ls=STYLES[i % 4], lw=1.8, label=f"{r.label} (median {pct(q, 50):.0f}, max {max(q) if q else 0:.0f})")
        cdf(ax_b, b, ls=STYLES[i % 4], lw=1.8, label=f"{r.label} (median {pct(b, 50):.0f} kB)")
    for ax, xl in ((ax_q, "REGTOPIC + TOPICQUERY requests received per registrar"), (ax_b, "reply bytes sent per registrar (kB)")):
        ax.set_xscale("log")
        ax.set_xlabel(xl)
        ax.set_ylabel("CDF over registrars")
        ax.set_ylim(0, 1)
        ax.grid(alpha=0.3, which="both")
        ax.legend(fontsize=9, loc="lower right")
    fig.suptitle("Registrar load: requests served and reply bytes")
    save(fig, args.out, "cmp_11_registrar_load")
    figs.append(("cmp_11_registrar_load", "Topic requests each node received as a registrar, all topics, and the bytes of its replies."))

    def by_types(r, types):
        return [sum((n.get("byType") or {}).get(t, {}).get("txBytes", 0) + (n.get("byType") or {}).get(t, {}).get("rxBytes", 0) for t in types) / 1e6 for n in r.oh]

    fig, (ax_r, ax_l) = plt.subplots(1, 2, figsize=(13, 4.6), constrained_layout=True)
    for i, r in enumerate(runs):
        rg, lk = by_types(r, REG), by_types(r, LOOKUP)
        cdf(ax_r, [v for v in rg if v > 0], ls=STYLES[i % 4], lw=1.8, label=f"{r.label} (median {pct(rg, 50):.2f} MB)")
        cdf(ax_l, [v for v in lk if v > 0], ls=STYLES[i % 4], lw=1.8, label=f"{r.label} (median {pct(lk, 50):.2f} MB)")
    for ax, xl in ((ax_r, "registration traffic per node, sent + received (MB)"), (ax_l, "lookup traffic per node, sent + received (MB)")):
        ax.set_xscale("log")
        ax.set_xlabel(xl)
        ax.set_ylabel("CDF over nodes")
        ax.set_ylim(0, 1)
        ax.grid(alpha=0.3, which="both")
        ax.legend(fontsize=9, loc="lower right")
    fig.suptitle("Registration and lookup traffic per node")
    save(fig, args.out, "cmp_12_reg_lookup_per_node")
    figs.append(("cmp_12_reg_lookup_per_node", "REGTOPIC/REGCONFIRMATION and TOPICQUERY/TOPICNODES bytes per node."))

    fig, (ax_rx, ax_tx) = plt.subplots(1, 2, figsize=(13, 4.6), constrained_layout=True)
    for i, r in enumerate(runs):
        tids = [int(h, 16) for h in (r.m.get("topicIds") or {})]
        if not tids:
            continue
        rows = collections.defaultdict(lambda: ([], []))
        for n in r.oh:
            if not n.get("id"):
                continue
            nid = int(n["id"], 16)
            d = min((nid ^ t).bit_length() for t in tids)
            rows[d][0].append(n.get("rxBytes", 0) / 1e6)
            rows[d][1].append(n.get("txBytes", 0) / 1e6)
        ds = sorted(d for d in rows if len(rows[d][0]) >= 3)
        ax_rx.plot(ds, [np.median(rows[d][0]) for d in ds], ls=STYLES[i % 4], marker="o", ms=3, lw=1.6, label=r.label)
        ax_tx.plot(ds, [np.median(rows[d][1]) for d in ds], ls=STYLES[i % 4], marker="o", ms=3, lw=1.6, label=r.label)
    for ax, yl in ((ax_rx, "received per node (MB, median)"), (ax_tx, "sent per node (MB, median)")):
        ax.set_xlabel("log2 XOR distance to the nearest topic ID (buckets with 3+ nodes)")
        ax.set_ylabel(yl)
        ax.set_yscale("log")
        ax.grid(alpha=0.3, which="both")
        ax.legend(fontsize=9)
    fig.suptitle("Load against distance to the nearest topic")
    save(fig, args.out, "cmp_13_load_vs_distance")
    figs.append(("cmp_13_load_vs_distance", "Median bytes received and sent per node against its distance to the closest topic ID."))

    # Summary table.
    rows = []

    def row(name, fmt, vals):
        rows.append(f"| {name} | " + " | ".join(fmt(v) for v in vals) + " |")

    lk = [r.lookups() for r in runs]
    row("searchers", str, [len(r.results) for r in runs])
    row("mean recall per searcher", lambda v: f"{v:.3f}", [r.recall() for r in runs])
    row("lookups", str, [l[3] for l in lk])
    row("lookups reaching F_lookup", lambda v: f"{v:.1f}%", [100 * l[4] / max(l[3], 1) for l in lk])
    row("lookup latency p50 / p95 (s)", lambda v: f"{v[0]:.2f} / {v[1]:.2f}", [(pct(l[0], 50), pct(l[0], 95)) for l in lk])
    row("nodes contacted per lookup p50 / p95", lambda v: f"{v[0]:.0f} / {v[1]:.0f}", [(pct(l[1], 50), pct(l[1], 95)) for l in lk])
    row("TOPICQUERY per lookup p50", lambda v: f"{v:.0f}", [pct(l[2], 50) for l in lk])
    if not scheduled:
        row("time to first result p50 (s)", lambda v: f"{v:.2f}", [pct([x["timeToFirstNs"] / 1e9 for x in r.results if x.get("timeToFirstNs", 0) > 0], 50) for r in runs])
    def first_adm(r):
        start = r.m.get("registrationStartNs") or {}
        v = [(ns - start.get(rid, 0)) / 1e9 for regs in (r.m.get("registrationTimingNs") or {}).values() for rid, ns in regs.items()]
        return [x for x in v if x >= 0]
    row("time to first admission p50 (s)", lambda v: f"{v:.1f}", [pct(first_adm(r), 50) for r in runs])
    row("requests per registrar p50 / p99 / max", lambda v: f"{v[0]:.0f} / {v[1]:.0f} / {v[2]:.0f}",
        [(pct(q, 50), pct(q, 99), max(q) if q else float("nan")) for q in [registrar_load(r, "rxMsgs") for r in runs]])
    row("fan-out p50 (registrars per registrant)", lambda v: f"{v:.0f}", [pct(r.fanout(), 50) for r in runs])
    row("quoted wait p50 (s)", lambda v: f"{v:.0f}", [pct(r.waits("quotedMs"), 50) for r in runs])
    row("total registration time p50 (s)", lambda v: f"{v:.0f}", [pct(r.waits("admittedMs"), 50) for r in runs])
    row("sent per node p50 / p95 (MB)", lambda v: f"{v[0]:.1f} / {v[1]:.1f}", [(pct(r.traffic("txBytes"), 50), pct(r.traffic("txBytes"), 95)) for r in runs])
    row("registration share of bytes sent", lambda v: f"{100 * v:.1f}%", [sum(s.get(t, 0) for t in REG) for s in shares])
    row("lookup share of bytes sent", lambda v: f"{100 * v:.1f}%", [sum(s.get(t, 0) for t in LOOKUP) for s in shares])

    L = [f"# {args.title or 'Run comparison'}\n", "## Runs\n", "| label | run directory |\n|---|---|"]
    L += [f"| {r.label} | `{r.dir}` |" for r in runs]
    if args.note:
        L += ["", "## Caveats\n"] + [f"- {n}" for n in args.note]
    L += ["", "## Summary\n", "| metric | " + " | ".join(r.label for r in runs) + " |",
          "|---|" + "---:|" * len(runs)] + rows + ["", "## Figures\n"]
    for stem, caption in figs:
        L += [f"### {stem}\n", f"![{stem}](figures/{stem}.png)\n", f"*{caption}*\n"]
    with open(os.path.join(args.out, "report.md"), "w") as f:
        f.write("\n".join(L))
    print(os.path.join(args.out, "report.md"))


if __name__ == "__main__":
    main()
