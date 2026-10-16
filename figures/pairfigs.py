"""Figures that put runs of different discovery protocols side by side.

Shared by compare_protocols.py (summary) and report_pair.py (the per-run
report layout, paired). Every figure function takes a Ctx and either saves one
figure (ctx.save) or records why it could not (ctx.skip). Runs are read from
the files a run directory holds (docs/TRACES.md); topics are pooled by size at
any topic count, so a 10-topic smoke run and a 25k crawl run read the same.

Search-phase traffic comes from series.json: the network-wide counters at the
first sample after the search starts and the last full sample before it stops.
Plain nodes report counters only when built on the fork (legacy_build: fork).
"""
import collections
import json
import os
import re

import matplotlib

matplotlib.use("Agg")
import matplotlib.ticker  # noqa: E402
import matplotlib.pyplot as plt  # noqa: E402
import numpy as np  # noqa: E402

from figures import SIZE_CLASSES  # noqa: E402

STYLES = ["-", "--", ":", "-."]
COLORS = ["#2f6f62", "#b5562f", "#4a5fa8", "#8a6d1f"]
CATEGORIES = [
    ("registration", ("REGTOPIC/v5", "REGTOPIC(renewal)/v5", "REGCONFIRMATION/v5")),
    ("topic search", ("TOPICQUERY/v5", "TOPICNODES/v5")),
    ("DHT (FINDNODE/NODES)", ("FINDNODE/v5", "NODES/v5")),
    ("liveness (PING/PONG)", ("PING/v5", "PONG/v5")),
]
CAT_COLORS = ["#7a5c99", "#2f6f62", "#c98b2b", "#9aa3ad", "#d0d4d9"]
FRACTIONS = (0.5, 0.9, 0.99, 1.0)
REQUESTS = ("FINDNODE/v5", "TOPICQUERY/v5", "REGTOPIC/v5", "REGTOPIC(renewal)/v5", "PING/v5")
REPLIES = ("NODES/v5", "TOPICNODES/v5", "REGCONFIRMATION/v5", "PONG/v5")
SEARCH_REQUESTS = ("FINDNODE/v5", "TOPICQUERY/v5")


def category(typ):
    for name, types in CATEGORIES:
        if typ in types:
            return name
    return "session and other"


CAT_NAMES = [c for c, _ in CATEGORIES] + ["session and other"]


def load(path, default=None):
    if not os.path.exists(path):
        return default
    with open(path) as f:
        return json.load(f)


def pct(v, p):
    return float(np.percentile(v, p)) if len(v) else float("nan")


def params(rd):
    log = os.path.join(rd, "run.log")
    if os.path.exists(log):
        m = re.search(r"^PARAMS: (.*)$", open(log).read(), re.M)
        if m:
            return dict(re.findall(r"(\S+?)=(\{[^}]*\}|\S*)", m.group(1)))
    return {}


def fmt_s(v):
    if v != v:
        return "–"
    if v < 1:
        return f"{v * 1000:.0f} ms"
    if v < 120:
        return f"{v:.1f} s"
    if v < 7200:
        return f"{v / 60:.1f} min"
    return f"{v / 3600:.1f} h"


def fmt_mb(v):
    return "–" if v != v else (f"{v:.2f} MB" if v < 10 else f"{v:.0f} MB")


# Time axes are linear by default; LOG_TIME (report_pair.py --log-time) puts
# them on a log scale, which shows sub-second detail at the cost of making
# equal gaps unequal.
LOG_TIME = False


def time_axis(ax, horizon_s, what, note=""):
    """Set up a time x axis; returns the divisor that converts seconds to its unit."""
    if LOG_TIME:
        ax.set_xscale("log")
        ax.set_xlabel(f"{what} (s){note}")
        return 1.0
    div, unit = (1.0, "s") if horizon_s <= 300 else (60.0, "min") if horizon_s <= 3 * 3600 else (3600.0, "h")
    ax.set_xlabel(f"{what} ({unit}){note}")
    ax.set_xlim(0, horizon_s / div * 1.01)
    return div


def idpos(hexid):
    """Position of a node or topic ID in [0, 1) from its leading 16 hex digits."""
    return int(hexid[:16], 16) / 2.0 ** 64


class Run:
    def __init__(self, label, rd, i):
        self.label, self.dir, self.i = label, rd, i
        self.style, self.color = STYLES[i % 4], COLORS[i % 4]
        self.m = load(os.path.join(rd, "metrics.json"), {})
        self.oh = load(os.path.join(rd, "oh.json"), []) or []
        self.s = load(os.path.join(rd, "series.json"), {}) or {}
        self.p = params(rd)
        self.results = [r for r in self.m.get("results", []) if r.get("target")]
        asg = os.path.join(rd, "assignments")
        a = load(os.path.join(asg, "node1.json")) or load(os.path.join(asg, "node0.json")) or {}
        self.phases = a.get("phases") or {}
        self.sample_ms = a.get("sample_ms") or 30000
        self.model = (a.get("search") or {}).get("model") or self.p.get("scenario.search.model", "?")
        self.plain = bool(a.get("legacy"))
        self.protocol = "plain discv5" if self.plain else "TopDisc"
        self.notes = []
        # Traffic is usable only when the counters cover the network: a stock
        # plain build counts nothing, and older collectors kept legacy nodes
        # out of oh.json.
        counted = [n for n in self.oh if n.get("byType")]
        nodes = int(self.p.get("scenario.population.nodes") or 0) or len(self.results)
        self.nodes = nodes
        self.traffic = bool(nodes) and len(counted) >= 0.9 * nodes
        if not self.traffic:
            why = ("the plain nodes count no traffic (stock build; use legacy_build: fork)" if self.plain and not counted
                   else f"traffic counters cover {len(counted)} of {nodes} nodes")
            self.notes.append(f"load left out: {why}")
        self.topic_of = {r["nodeIdx"]: r["topic"] for r in self.results}
        self.size_of = {t["topic"]: t.get("target", 0) + 1 for t in self.m.get("perTopic") or []}

    # ---------------------------------------------------------------- search

    def found_at(self, r):
        return np.sort(np.asarray(r.get("uniqueFoundAtMs") or [], dtype=float)) / 1000.0

    def first_found(self, rs=None):
        """Seconds to each searcher's first provider; a provider found at 0 ms
        (already in the routing table) counts."""
        return [float(self.found_at(x)[0]) for x in (self.results if rs is None else rs) if x.get("uniqueFoundAtMs")]

    def table_hits(self, rs=None):
        """Share of searchers whose first provider came at 0 ms: a plain node
        had one in its routing table when the search started."""
        rs = self.results if rs is None else rs
        return sum(1 for x in rs if x.get("uniqueFoundAtMs") and min(x["uniqueFoundAtMs"]) == 0) / max(len(rs), 1)

    def time_to(self, r, f):
        """Seconds into the search when the searcher had found share f of its topic, or None."""
        t = self.found_at(r)
        k = max(int(np.ceil(f * r["target"] - 1e-9)), 1)
        return float(t[k - 1]) if len(t) >= k else None

    def search_s(self):
        if self.phases.get("search_at_ms") and self.phases.get("stop_at_ms"):
            return (self.phases["stop_at_ms"] - self.phases["search_at_ms"]) / 1000.0
        return max((r.get("timeToCompletionNs", 0) / 1e9 for r in self.results), default=0)

    def search_requests(self, r):
        """Requests a searcher's own search sent (TopDisc: TOPICQUERY; fork-built
        plain node: FINDNODE), or None when the run did not count them."""
        q = r.get("lookupQueries") or []
        if q and min(q) >= 0:
            return sum(q)
        st = r.get("searchStats") or {}
        return st.get("queries") if st else None

    # --------------------------------------------------------------- traffic

    def start_ms(self):
        start = self.s.get("startMs")
        if start is None:
            # Older series.json: tSec 0 is the earliest node's first sample, one
            # period after the first node started.
            start = self.phases.get("start_at_ms", 0) + self.sample_ms
            note = "series.json has no startMs; phases are placed from the phase plan (±1 sample)"
            if note not in self.notes:
                self.notes.append(note)
        return start

    def full_samples(self):
        samples = self.s.get("samples") or []
        if not samples:
            return []
        full = max(sum(x["nodes"]) for x in samples)
        return [x for x in samples if sum(x["nodes"]) == full]

    def window(self):
        """Network counters by message type at the start and end of the search
        phase: (t0, t1, nodes, {type: (tx bytes, rx bytes, tx msgs)} deltas), or None."""
        ok = self.full_samples()
        if not ok or not self.phases.get("search_at_ms"):
            return None
        start = self.start_ms()
        s0 = (self.phases["search_at_ms"] - start) / 1000.0
        s1 = (self.phases.get("stop_at_ms", 0) - start) / 1000.0
        a = next((x for x in ok if x["tSec"] >= s0), None)
        b = next((x for x in reversed(ok) if x["tSec"] <= s1), None)
        if a is None or b is None or b["tSec"] <= a["tSec"]:
            return None

        def tot(x):
            return {t: (sum(c["txBytes"]), sum(c["rxBytes"]), sum(c["txMsgs"])) for t, c in x["byType"].items()}

        ta, tb = tot(a), tot(b)
        d = {t: tuple(v - ta.get(t, (0, 0, 0))[k] for k, v in enumerate(vs)) for t, vs in tb.items()}
        return a["tSec"], b["tSec"], sum(b["nodes"]), d

    def series_rates(self):
        """Per-node sent bytes per second by category over time: (minutes, {cat: rate})."""
        samples = self.s.get("samples") or []
        if len(samples) < 2:
            return None
        xs, rates = [], collections.defaultdict(list)
        for p, q in zip(samples, samples[1:]):
            n = sum(q["nodes"])
            # a partial last slot holds only the nodes that sampled in it
            if n < sum(p["nodes"]) or q["tSec"] <= p["tSec"]:
                continue
            dt = q["tSec"] - p["tSec"]
            per = collections.Counter()
            for t, c in q["byType"].items():
                prev = p["byType"].get(t)
                per[category(t)] += sum(c["txBytes"]) - (sum(prev["txBytes"]) if prev else 0)
            xs.append(q["tSec"] / 60.0)
            for cat in CAT_NAMES:
                rates[cat].append(per[cat] / dt / n)
        return xs, rates

    def bucket_peak(self, key="rxMsgs", window=3):
        """Peak sustained per-node rate per ID-space bucket of series.json:
        (bucket centres in [0, 1), rate per second)."""
        samples = self.s.get("samples") or []
        if len(samples) < window + 1:
            return None
        nb = len(samples[0][key])
        peak = np.zeros(nb)
        for i in range(window, len(samples)):
            p, q = samples[i - window], samples[i]
            dt = q["tSec"] - p["tSec"]
            if dt <= 0:
                continue
            nodes = np.asarray(q["nodes"], dtype=float)
            if nodes.sum() < np.sum(p["nodes"]):
                continue
            rate = (np.asarray(q[key], dtype=float) - np.asarray(p[key], dtype=float)) / dt
            with np.errstate(divide="ignore", invalid="ignore"):
                per = np.where(nodes > 0, rate / nodes, 0)
            peak = np.maximum(peak, per)
        return (np.arange(nb) + 0.5) / nb, peak

    def served(self, key, types):
        return [sum((n.get("byType") or {}).get(t, {}).get(key, 0) for t in types) for n in self.oh]

    def node_totals(self, key):
        return [n.get(key, 0) for n in self.oh]

    def run_by_category(self, key="txBytes"):
        tot = collections.Counter()
        for n in self.oh:
            for t, c in (n.get("byType") or {}).items():
                tot[category(t)] += c.get(key, 0)
        return tot

    def hosts(self):
        hd = os.path.join(self.dir, "hostmetrics")
        if os.path.isdir(hd):
            files = [os.path.join(hd, f) for f in os.listdir(hd)]
        elif os.path.exists(hd + ".json"):  # local backend: one host
            files = [hd + ".json"]
        else:
            return None
        cpu, rss, drops = [], [], 0
        for f in files:
            h = load(f, []) or []
            for s in h:
                for ns in s.get("nodes", []):
                    cpu.append(ns.get("cpu_pct", 0))
                    rss.append(ns.get("rss_mb", 0))
            if h:  # host counters are cumulative since boot
                drops += h[-1].get("udp_rcvbuf_errors", 0) - h[0].get("udp_rcvbuf_errors", 0)
        return cpu, rss, drops


def size_groups(runs):
    """[(name, topics)]: all topics, then the size classes present."""
    per = runs[0].m.get("perTopic") or []
    size = {r["topic"]: r.get("target", 0) for r in per}
    topics = sorted(size)
    out = [("all topics", topics)]
    hi = None
    for lo, name in SIZE_CLASSES:
        ts = [t for t in topics if size[t] >= lo and (hi is None or size[t] < hi)]
        if ts:
            out.append((f"{name.replace(' registrants', ' nodes')} ({len(ts)} topics)", ts))
        hi = lo
    if len(out) == 2:  # one size class holds every topic: "all topics" would repeat it
        out = out[1:]
    return out


class Ctx:
    def __init__(self, runs, out):
        self.runs, self.out = runs, out
        self.groups = size_groups(runs)
        self.figs, self.missing = [], []
        self.stats = {r.label: {} for r in runs}
        os.makedirs(os.path.join(out, "figures"), exist_ok=True)

    def in_group(self, r, ts):
        return [x for x in r.results if x["topic"] in ts]

    def save(self, fig, stem, caption):
        fig.savefig(os.path.join(self.out, "figures", stem + ".png"), dpi=130, bbox_inches="tight")
        plt.close(fig)
        self.figs.append((stem, caption))

    def skip(self, stem, why):
        self.missing.append((stem, why))

    def has(self, stem):
        return any(s == stem for s, _ in self.figs)

    def caption(self, stem):
        return next((c for s, c in self.figs if s == stem), "")

    def group_axes(self, height=3.8, sharey=True):
        n = len(self.groups)
        return plt.subplots(1, n, figsize=(3.7 * n, height), sharey=sharey, squeeze=False, constrained_layout=True)


def cdf(ax, r, values, label, denom=None):
    v = np.sort(np.asarray(values, dtype=float))
    if len(v):
        ax.step(v, np.arange(1, len(v) + 1) / (denom or len(v)), where="post", ls=r.style, c=r.color, lw=1.8, label=label)


# ------------------------------------------------------------------ discovery

def found_over_time(ctx):
    fig, axes = ctx.group_axes()
    horizon = max(r.search_s() for r in ctx.runs) or 1
    full = horizon
    if not LOG_TIME:
        # Zoom to where the curves still move: the time by which 95 % of
        # searchers had found their last provider, with a margin.
        last = [float(r.found_at(x)[-1]) for r in ctx.runs for x in r.results if x.get("uniqueFoundAtMs")]
        if last:
            horizon = min(horizon, max(pct(last, 95) * 1.3, 1.0))
    grid = (np.concatenate([[0], np.logspace(-2, np.log10(horizon), 240)]) if LOG_TIME
            else np.linspace(0, horizon, 400))
    for ax, (name, ts) in zip(axes[0], ctx.groups):
        div = time_axis(ax, horizon, "search time", "" if horizon >= full else f", first {fmt_s(horizon)} of {fmt_s(full)}")
        for r in ctx.runs:
            rs = ctx.in_group(r, ts)
            if not rs:
                continue
            curves = np.vstack([np.searchsorted(r.found_at(x), grid, side="right") / x["target"] for x in rs])
            xs = (np.maximum(grid, 0.01) if LOG_TIME else grid) / div
            ax.plot(xs, np.median(curves, axis=0), ls=r.style, c=r.color, lw=1.8, label=r.label)
            ax.fill_between(xs, np.percentile(curves, 25, axis=0), np.percentile(curves, 75, axis=0), color=r.color, alpha=0.12)
        ax.set_title(name, fontsize=9)
        ax.set_ylim(0, 1.02)
        ax.grid(alpha=0.3, which="both")
    axes[0][0].set_ylabel("share of the topic found\n(median, IQR shaded)")
    axes[0][0].legend(fontsize=8, loc="upper left")
    ctx.save(fig, "cp_01_found_over_time",
             "Share of its topic's other nodes each searcher had found, against time since its search started: median line, interquartile range shaded.")


def time_to_first(ctx):
    fig, ax = plt.subplots(figsize=(8, 4.2), constrained_layout=True)
    ts = {r.label: [max(v, 1e-3) if LOG_TIME else v for v in r.first_found()] for r in ctx.runs}
    div = time_axis(ax, max((max(v) for v in ts.values() if v), default=1), "time from search start to the first provider")
    for r in ctx.runs:
        t = ts[r.label]
        n = len(r.results) or 1
        cdf(ax, r, [v / div for v in t], f"{r.label}: median {fmt_s(pct(t, 50))}, {100 * len(t) / n:.0f} % found one", denom=n)
    ax.set_ylabel("share of all searchers")
    ax.set_ylim(0, 1.02)
    ax.grid(alpha=0.3, which="both")
    ax.legend(fontsize=8, loc="lower right")
    ctx.save(fig, "cp_02_time_to_first", "Time to the first provider. Searchers that found none stay out of the curve but count in the denominator.")


def final_share(ctx):
    fig, axes = ctx.group_axes(3.6)
    for ax, (name, ts) in zip(axes[0], ctx.groups):
        for r in ctx.runs:
            v = [len(x.get("uniqueFoundAtMs") or []) / x["target"] for x in ctx.in_group(r, ts)]
            cdf(ax, r, v, f"{r.label} ({100 * pct(v, 50):.0f} %)")
        ax.set_title(name, fontsize=9)
        ax.set_xlabel("share found by the end")
        ax.set_xlim(0, 1.02)
        ax.grid(alpha=0.3)
        ax.legend(fontsize=7, loc="upper left")
    axes[0][0].set_ylabel("CDF over searchers")
    ctx.save(fig, "cp_03_final_share", "Share of its topic each searcher had found when the search phase ended; the legend gives the median.")


def time_to_fraction(ctx):
    fracs = FRACTIONS[:3]
    n = len(ctx.groups)
    fig, axes = plt.subplots(n, len(fracs), figsize=(4.2 * len(fracs), 2.6 * n), sharex=LOG_TIME, sharey=True, squeeze=False, constrained_layout=True)
    for row, (name, ts) in enumerate(ctx.groups):
        for col, f in enumerate(fracs):
            ax = axes[row][col]
            times = {r.label: [v for v in (r.time_to(x, f) for x in ctx.in_group(r, ts)) if v is not None] for r in ctx.runs}
            # linear: each panel scales to its own slowest searcher, or the curves sit in the first pixel
            div = time_axis(ax, max((max(v) for v in times.values() if v), default=1), "search time")
            for r in ctx.runs:
                rs = ctx.in_group(r, ts)
                t = times[r.label]
                cdf(ax, r, [max(v, 1e-2) / div for v in t], f"{r.label}: {fmt_s(pct(t, 50))}, {100 * len(t) / max(len(rs), 1):.0f} % reach", denom=len(rs) or 1)
            ax.set_ylim(0, 1.02)
            ax.grid(alpha=0.3, which="both")
            ax.legend(fontsize=6.5, loc="upper left")
            if row == 0:
                ax.set_title(f"time to {int(f * 100)} % of the topic", fontsize=10)
            if col == 0:
                ax.set_ylabel(name.replace(" (", "\n("), fontsize=8)
            if row != n - 1 and LOG_TIME:
                ax.set_xlabel("")
    ctx.save(fig, "cp_08_time_to_fraction",
             "Time for each searcher to find 50 %, 90 % and 99 % of its topic, by topic size. Searchers that never got there count in "
             "the denominator, so a curve ends at the share that did; the legend gives the median time of those that did.")


def discovery_rate(ctx):
    horizon = max(r.search_s() for r in ctx.runs) or 1
    nb = 60
    edges = np.linspace(0, horizon, nb + 1)
    width_min = (edges[1] - edges[0]) / 60.0
    fig, axes = ctx.group_axes()
    for ax, (name, ts) in zip(axes[0], ctx.groups):
        for r in ctx.runs:
            rs = ctx.in_group(r, ts)
            if not rs:
                continue
            h = np.zeros(nb)
            for x in rs:
                h += np.histogram(r.found_at(x), bins=edges)[0]
            ax.plot(edges[1:] / 60.0, h / len(rs) / width_min, ls=r.style, c=r.color, lw=1.6, label=r.label)
        ax.set_yscale("symlog", linthresh=0.1)
        ax.set_title(name, fontsize=9)
        ax.set_xlabel("search time (min)")
        ax.grid(alpha=0.3, which="both")
    axes[0][0].set_ylabel("new providers per searcher per minute")
    axes[0][0].legend(fontsize=8)
    ctx.save(fig, "cp_09_discovery_rate", "New providers found per searcher per minute over the search, by topic size: how fast each protocol is still learning.")


def search_requests(ctx):
    got = [(r, [q for q in (r.search_requests(x) for x in r.results) if q is not None]) for r in ctx.runs]
    got = [(r, q) for r, q in got if q]
    if len(got) < 1:
        ctx.skip("cp_10_search_requests", "no run counted the requests of each search")
        return
    fig, ax = plt.subplots(figsize=(8, 4.2), constrained_layout=True)
    for r, q in got:
        what = "FINDNODE" if r.plain else "TOPICQUERY"
        cdf(ax, r, [max(v, 0.5) for v in q], f"{r.label} ({what}): median {pct(q, 50):,.0f}")
    ax.set_xscale("log")
    ax.set_xlabel("requests sent by one searcher's search")
    ax.set_ylabel("CDF over searchers")
    ax.grid(alpha=0.3, which="both")
    ax.legend(fontsize=8, loc="lower right")
    absent = [r.label for r in ctx.runs if r not in [g[0] for g in got]]
    note = f" Not counted for {', '.join(absent)}." if absent else ""
    ctx.save(fig, "cp_10_search_requests",
             "Requests each searcher's own search sent: TOPICQUERY for TopDisc, FINDNODE for the plain walk (fork build)." + note)


def lookup_latency(ctx):
    multi = [r for r in ctx.runs if any(len(x.get("lookupLatencyMs") or []) > 1 for x in r.results)]
    if not multi:
        ctx.skip("cp_11_lookup_latency", "every searcher ran one long search (continuous or connection model); lookup latency applies to the scheduled model")
        return
    fig, ax = plt.subplots(figsize=(8, 4.2), constrained_layout=True)
    lats = {r.label: [l / 1000.0 for x in r.results for l, n in zip(x.get("lookupLatencyMs") or [], x.get("lookupResults") or [])
                      if n >= min(x.get("fLookup") or 30, x.get("target") or 30)] for r in multi}
    div = time_axis(ax, max((max(v) for v in lats.values() if v), default=1), "lookup latency to F_lookup providers")
    for r in multi:
        lat = lats[r.label]
        cdf(ax, r, [v / div for v in lat], f"{r.label}: median {fmt_s(pct(lat, 50))}")
    ax.set_ylabel("CDF over lookups that got there")
    ax.grid(alpha=0.3, which="both")
    ax.legend(fontsize=8, loc="lower right")
    ctx.save(fig, "cp_11_lookup_latency", "Scheduled lookups: time to reach F_lookup distinct providers.")


def idspace_found_share(ctx):
    fig, axes = plt.subplots(1, len(ctx.runs), figsize=(6.2 * len(ctx.runs), 3.8), sharey=True, squeeze=False, constrained_layout=True)
    drawn = False
    for ax, r in zip(axes[0], ctx.runs):
        cnt = collections.Counter()
        for x in r.results:
            for fid in x.get("uniqueFoundIds") or []:
                cnt[fid[:16]] += 1
        ids = [(x["nodeId"][:16], x["topic"]) for x in r.results]
        if not cnt or not ids:
            ax.set_title(f"{r.label}: no per-provider ids", fontsize=9)
            continue
        xs = [idpos(i) for i, _ in ids]
        ys = [cnt.get(i, 0) / max(r.size_of.get(t, 2) - 1, 1) for i, t in ids]
        ax.scatter(xs, ys, s=4, c=r.color, alpha=0.5, lw=0)
        ax.set_title(f"{r.label}: median {100 * pct(ys, 50):.0f} %, {sum(1 for y in ys if y == 0)} never found", fontsize=9)
        ax.set_xlabel("node ID position")
        ax.set_xlim(0, 1)
        ax.set_ylim(0, 1.02)
        ax.grid(alpha=0.3)
        drawn = True
    axes[0][0].set_ylabel("share of its topic's searchers\nthat found the node")
    if drawn:
        ctx.save(fig, "cp_12_idspace_found_share", "For each provider, the share of the other searchers of its topic that found it, across the ID space.")
    else:
        plt.close(fig)
        ctx.skip("cp_12_idspace_found_share", "no run kept uniqueFoundIds")


# ------------------------------------------------------------- overhead/load

def traffic_runs(ctx, stem):
    rs = [r for r in ctx.runs if r.traffic]
    if not rs:
        ctx.skip(stem, "no run with traffic counters on 90 % of its nodes (plain nodes need legacy_build: fork)")
    return rs


def topic_marks(ax, r):
    tids = list((r.m.get("topicIds") or {}).keys())
    if len(tids) <= 12:
        for h in tids:
            ax.axvline(idpos(h), c="k", lw=0.5, ls=":", alpha=0.6)


def idspace_traffic(ctx):
    rs = traffic_runs(ctx, "cp_13_idspace_traffic")
    if not rs:
        return
    fig, (ax_s, ax_r) = plt.subplots(1, 2, figsize=(13, 4.2), sharey=True, constrained_layout=True)
    for r in rs:
        xs = [idpos(n["id"]) for n in r.oh if n.get("id")]
        for ax, key in ((ax_s, "txBytes"), (ax_r, "rxBytes")):
            ys = [max(n.get(key, 0) / 1e6, 1e-3) for n in r.oh if n.get("id")]
            ax.scatter(xs, ys, s=5, c=r.color, alpha=0.55, lw=0, label=f"{r.label}: median {fmt_mb(pct(ys, 50))}")
    topic_marks(ax_s, rs[0])
    topic_marks(ax_r, rs[0])
    for ax, what in ((ax_s, "sent"), (ax_r, "received")):
        ax.set_yscale("log")
        ax.set_xlim(0, 1)
        ax.set_xlabel("node ID position (dotted: topic IDs)")
        ax.set_title(f"MB {what} per node, whole run", fontsize=10)
        ax.grid(alpha=0.3, which="both")
        ax.legend(fontsize=8, markerscale=3)
    ctx.save(fig, "cp_13_idspace_traffic", "Bytes each node sent and received over the whole run, against its position in the ID space.")


def msgtype_split(ctx):
    rs = traffic_runs(ctx, "cp_14_msgtype")
    if not rs:
        return
    fig, axes = plt.subplots(1, 2, figsize=(12, 4.2), constrained_layout=True)
    for ax, key, what in ((axes[0], "txBytes", "sent"), (axes[1], "rxBytes", "received")):
        x = np.arange(len(rs))
        bottom = np.zeros(len(rs))
        for cat, col in zip(CAT_NAMES, CAT_COLORS):
            v = np.array([r.run_by_category(key)[cat] / max(len(r.oh), 1) / 1e6 for r in rs])
            ax.bar(x, v, 0.6, bottom=bottom, color=col, label=cat)
            bottom += v
        for i, b in enumerate(bottom):
            ax.text(i, b, fmt_mb(b), ha="center", va="bottom", fontsize=8)
        if bottom.min() > 0 and bottom.max() / bottom.min() > 30:
            ax.set_yscale("log")
        ax.set_xticks(x, [r.label for r in rs], fontsize=8)
        ax.set_ylabel(f"MB {what} per node, mean")
        ax.set_title(f"Bytes {what} by message category, whole run", fontsize=10)
    axes[0].legend(fontsize=7)
    ctx.save(fig, "cp_14_msgtype", "Mean bytes per node by message category over the whole run: registration, topic search, DHT walk, liveness and session setup.")


def traffic_over_time(ctx):
    rated = [(r, r.series_rates()) for r in ctx.runs if r.traffic]
    rated = [(r, x) for r, x in rated if x and x[0]]
    if not rated:
        ctx.skip("cp_04_traffic_over_time", "no run with counter samples on 90 % of its nodes (plain nodes need legacy_build: fork)")
        return
    # separate y scales: the two protocols can differ by orders of magnitude
    fig, axes = plt.subplots(1, len(rated), figsize=(6.2 * len(rated), 3.8), squeeze=False, constrained_layout=True)
    for ax, (r, (xs, rates)) in zip(axes[0], rated):
        ax.stackplot(xs, [np.asarray(rates[c]) / 1e3 for c in CAT_NAMES], labels=CAT_NAMES, colors=CAT_COLORS, alpha=0.9)
        start = r.start_ms()
        for k, lab in (("register_at_ms", "register"), ("search_at_ms", "search"), ("stop_at_ms", "stop")):
            if r.phases.get(k):
                x = (r.phases[k] - start) / 60000.0
                ax.axvline(x, c="k", lw=0.8, ls=":")
                ax.text(x, 0.6, f" {lab}", transform=ax.get_xaxis_transform(), fontsize=7, rotation=90, va="bottom")
        ax.set_title(r.label, fontsize=9)
        ax.set_xlabel("minutes since the first node sampled")
        ax.set_ylabel("kB/s sent per node")
        ax.grid(alpha=0.3)
    axes[0][-1].legend(fontsize=7, loc="upper right")
    ctx.save(fig, "cp_04_traffic_over_time", "Bytes sent per node per second, by message category, over the whole run; dotted lines mark the phases. Each panel has its own y scale.")


def peak_rate_idspace(ctx):
    rs = [(r, r.bucket_peak()) for r in ctx.runs if r.traffic]
    rs = [(r, p) for r, p in rs if p]
    if not rs:
        ctx.skip("cp_15_peak_rate_idspace", "no run with counter samples on 90 % of its nodes")
        return
    fig, ax = plt.subplots(figsize=(9, 4.2), constrained_layout=True)
    for r, (xs, peak) in rs:
        ax.plot(xs, np.maximum(peak, 1e-2), ls=r.style, c=r.color, marker="o", ms=3, lw=1.6, label=f"{r.label}: max {peak.max():,.1f}/s")
    topic_marks(ax, rs[0][0])
    ax.set_yscale("log")
    ax.set_xlim(0, 1)
    ax.set_xlabel("ID-space bucket (dotted: topic IDs)")
    ax.set_ylabel("peak messages received\nper node per second")
    ax.grid(alpha=0.3, which="both")
    ax.legend(fontsize=8)
    ctx.save(fig, "cp_15_peak_rate_idspace", "Highest sustained receive rate per node (3-sample moving window) in each ID-space bucket, over the whole run.")


def load_vs_topic_distance(ctx):
    rs = traffic_runs(ctx, "cp_16_load_vs_topic_distance")
    if not rs:
        return
    fig, (ax_rx, ax_tx) = plt.subplots(1, 2, figsize=(13, 4.2), constrained_layout=True)
    for r in rs:
        tids = [int(h, 16) for h in (r.m.get("topicIds") or {})]
        if not tids:
            continue
        rows = collections.defaultdict(lambda: ([], []))
        for n in r.oh:
            if not n.get("id"):
                continue
            nid = int(n["id"], 16)
            d = min((nid ^ t).bit_length() for t in tids)
            rows[d][0].append(sum((n.get("byType") or {}).get(t, {}).get("rxMsgs", 0) for t in REQUESTS))
            rows[d][1].append(n.get("txBytes", 0) / 1e6)
        ds = sorted(d for d in rows if len(rows[d][0]) >= 3)
        ax_rx.plot(ds, [max(np.median(rows[d][0]), 1) for d in ds], ls=r.style, c=r.color, marker="o", ms=3, lw=1.6, label=r.label)
        ax_tx.plot(ds, [max(np.median(rows[d][1]), 1e-3) for d in ds], ls=r.style, c=r.color, marker="o", ms=3, lw=1.6, label=r.label)
    for ax, yl in ((ax_rx, "requests received per node (median)"), (ax_tx, "MB sent per node (median)")):
        ax.set_xlabel("log2 XOR distance to the nearest topic ID (buckets with 3+ nodes)")
        ax.xaxis.set_major_locator(matplotlib.ticker.MaxNLocator(integer=True))
        ax.set_ylabel(yl)
        ax.set_yscale("log")
        ax.grid(alpha=0.3, which="both")
        ax.legend(fontsize=8)
    ctx.save(fig, "cp_16_load_vs_topic_distance",
             "Load against a node's distance to the closest topic ID. TopDisc concentrates registrations and queries near topic IDs; a plain walk does not depend on topics.")


def cost_vs_topic_size(ctx):
    rs = traffic_runs(ctx, "cp_17_cost_vs_topic_size")
    if not rs:
        return
    fig, (ax_q, ax_p) = plt.subplots(1, 2, figsize=(13, 4.2), constrained_layout=True)
    for r in rs:
        per = collections.defaultdict(list)
        for n in r.oh:
            t = r.topic_of.get(n.get("idx"))
            if t is None:
                continue
            per[t].append(sum((n.get("byType") or {}).get(k, {}).get("txMsgs", 0) for k in SEARCH_REQUESTS))
        found = collections.defaultdict(list)
        for x in r.results:
            found[x["topic"]].append(len(x.get("uniqueFoundAtMs") or []))
        ts = sorted(per, key=lambda t: r.size_of.get(t, 0))
        sz = [r.size_of.get(t, 0) for t in ts]
        q = [np.median(per[t]) for t in ts]
        ax_q.plot(sz, q, ls=r.style, c=r.color, marker="o", ms=4, lw=1.4, label=r.label)
        ppr = [np.median(found[t]) / max(qq, 1) for t, qq in zip(ts, q)]
        ax_p.plot(sz, ppr, ls=r.style, c=r.color, marker="o", ms=4, lw=1.4, label=r.label)
    for ax, yl in ((ax_q, "FINDNODE + TOPICQUERY sent per node\n(median per topic, whole run)"),
                   (ax_p, "providers found per request sent\n(median per topic)")):
        ax.set_xscale("log")
        ax.set_yscale("log")
        ax.set_xlabel("topic size (nodes)")
        ax.set_ylabel(yl)
        ax.grid(alpha=0.3, which="both")
        ax.legend(fontsize=8)
    ctx.save(fig, "cp_17_cost_vs_topic_size",
             "Discovery requests each node sent over the whole run, and providers found per request, against the size of its topic. "
             "Whole-run counts include table upkeep and, for TopDisc, registration walks.")


def search_cost(ctx):
    win = {r.label: r.window() if r.traffic else None for r in ctx.runs}
    have = [r for r in ctx.runs if win[r.label]]
    if not have:
        ctx.skip("cp_05_search_cost", "no run with counter samples covering the search phase")
        return
    fig, (ax_b, ax_c) = plt.subplots(1, 2, figsize=(12, 4.2), constrained_layout=True)
    x = np.arange(len(have))
    bottom = np.zeros(len(have))
    for cat, col in zip(CAT_NAMES, CAT_COLORS):
        v = np.array([sum(d[0] + d[1] for t, d in win[r.label][3].items() if category(t) == cat) / win[r.label][2] / 1e6 for r in have])
        ax_b.bar(x, v, 0.6, bottom=bottom, color=col, label=cat)
        bottom += v
    for i, b in enumerate(bottom):
        ax_b.text(i, b, fmt_mb(b), ha="center", va="bottom", fontsize=8)
    if bottom.min() > 0 and bottom.max() / bottom.min() > 30:
        ax_b.set_yscale("log")
    ax_b.set_xticks(x, [r.label for r in have], fontsize=8)
    ax_b.set_ylabel("MB per node, sent + received")
    ax_b.set_title("Traffic during the search phase", fontsize=10)
    ax_b.legend(fontsize=7)
    for i, r in enumerate(have):
        t0, t1, n, d = win[r.label]
        found = sum(len(x_.get("uniqueFoundAtMs") or []) for x_ in r.results)
        sent = sum(v[0] for v in d.values())
        reqs = sum(d.get(t, (0, 0, 0))[2] for t in SEARCH_REQUESTS)
        st = ctx.stats[r.label]
        st["search_window_s"] = t1 - t0
        st["search_bytes_per_node_mb"] = sum(v[0] + v[1] for v in d.values()) / n / 1e6
        st["search_requests_per_node"] = reqs / n
        st["bytes_sent_per_provider_kb"] = sent / found / 1e3 if found else float("nan")
        st["requests_per_provider"] = reqs / found if found else float("nan")
        ax_c.bar(i, st["bytes_sent_per_provider_kb"], 0.6, color=r.color)
        ax_c.text(i, st["bytes_sent_per_provider_kb"], f"{st['bytes_sent_per_provider_kb']:.1f} kB\n{st['requests_per_provider']:.1f} req", ha="center", va="bottom", fontsize=8)
    cost = [ctx.stats[r.label]["bytes_sent_per_provider_kb"] for r in have]
    if min(cost) > 0 and max(cost) / min(cost) > 30:
        ax_c.set_yscale("log")
    ax_c.set_xticks(x, [r.label for r in have], fontsize=8)
    ax_c.set_ylabel("kB sent network-wide per provider found")
    ax_c.set_title("Cost per provider found (search phase)", fontsize=10)
    ax_c.margins(y=0.2)
    ctx.save(fig, "cp_05_search_cost",
             "Left: bytes each node sent and received while the searches ran, by category, averaged over all nodes. "
             "Right: bytes sent by the whole network during the search phase divided by the providers all searchers found, "
             "with the FINDNODE and TOPICQUERY requests per provider.")


def serving_load(ctx):
    rs = traffic_runs(ctx, "cp_06_serving_load")
    if not rs:
        return
    # Bytes received would mostly count replies to the node's own requests,
    # so a busy searcher would look loaded.
    fig, (ax_m, ax_b) = plt.subplots(1, 2, figsize=(12, 4.2), constrained_layout=True)
    for r in rs:
        for ax, v, unit in ((ax_m, r.served("rxMsgs", REQUESTS), ""), (ax_b, np.asarray(r.served("txBytes", REPLIES)) / 1e6, " MB")):
            v = np.sort(np.asarray(v, dtype=float))
            v = v[v > 0]
            if len(v):
                cdf(ax, r, v, f"{r.label}: p50 {pct(v, 50):,.1f}{unit}, max {v[-1]:,.1f}{unit} ({v[-1] / pct(v, 50):.1f}× p50)")
    ax_m.set_xlabel("requests received per node, whole run")
    ax_b.set_xlabel("reply MB sent per node, whole run")
    for ax in (ax_m, ax_b):
        ax.set_xscale("log")
        ax.set_ylabel("CDF over nodes")
        ax.grid(alpha=0.3, which="both")
        ax.legend(fontsize=7, loc="lower right")
    ctx.save(fig, "cp_06_serving_load",
             "Load each node carries for others over the whole run: requests received (FINDNODE, TOPICQUERY, REGTOPIC, PING) "
             "and bytes of the replies it sent (NODES, TOPICNODES, REGCONFIRMATION, PONG).")


# ------------------------------------------------------------ peers, churn

def conn_runs(ctx):
    return [r for r in ctx.runs if any(x.get("outboundConns") for x in r.results)]


def peer_slots(ctx):
    conn = conn_runs(ctx)
    if not conn:
        return
    fig, (ax_o, ax_f) = plt.subplots(1, 2, figsize=(12, 4.2), constrained_layout=True)
    for r in conn:
        ob = [x.get("outboundConns", 0) for x in r.results]
        cdf(ax_o, r, ob, f"{r.label} (median {pct(ob, 50):.0f})")
    fills = {r.label: [x["slotsFilledAtMs"] / 1000 for x in r.results if x.get("slotsFilledAtMs", 0) > 0] for r in conn}
    div = time_axis(ax_f, max((max(v) for v in fills.values() if v), default=1), "time from search start until the outbound slots were full")
    for r in conn:
        ff = fills[r.label]
        cdf(ax_f, r, [v / div for v in ff], f"{r.label}: median {fmt_s(pct(ff, 50))}, {100 * len(ff) / len(r.results):.0f} % filled", denom=len(r.results))
    ax_o.set_xlabel("outbound peers at the end")
    for ax in (ax_o, ax_f):
        ax.set_ylabel("share of searchers")
        ax.grid(alpha=0.3, which="both")
        ax.legend(fontsize=8, loc="lower right")
    ctx.save(fig, "cp_07_peer_slots", "Connection model: outbound provider peers per node at the end, and time to fill the outbound slots.")


def dead_results(ctx):
    have = [(r, r.m.get("deadResults")) for r in ctx.runs]
    have = [(r, d) for r, d in have if d and d.get("perTopic")]
    if not have:
        ctx.skip("cp_18_dead_results", "no dead-result data (simnet churn runs only)")
        return
    fig, ax = plt.subplots(figsize=(9, 4.2), constrained_layout=True)
    for r, d in have:
        win = max(d.get("bucketMs") or 10_000, 300_000)
        over = collections.defaultdict(lambda: [0, 0])
        for t in d["perTopic"]:
            for p in t.get("overTime") or []:
                w = (p[0] // win) * win
                over[w][0] += p[1]
                over[w][1] += p[2]
        ws = sorted(over)
        ax.plot([w / 60000 for w in ws], [100 * over[w][1] / max(over[w][0], 1) for w in ws], ls=r.style, c=r.color, lw=1.8, label=r.label)
    ax.set_xlabel("minutes")
    ax.set_ylabel("dead results (% of returned)")
    ax.grid(alpha=0.3)
    ax.legend(fontsize=8)
    ctx.save(fig, "cp_18_dead_results", "Share of returned results whose node was offline, over 5-min windows, all topics.")


# --------------------------------------------------------------------- tables

def row(cells):
    return "| " + " | ".join(cells) + " |"


def head(ctx, first="metric"):
    return [row([first] + [r.label for r in ctx.runs]), "|---|" + "---:|" * len(ctx.runs)]


def notes(ctx, extra=()):
    out = list(extra)
    if any(r.model == "conn" for r in ctx.runs):
        out.append("Connection model: a searcher stops once its peer slots are full, while every node keeps its "
                   "table and registrations up for the whole phase. Search-phase cost per provider found therefore "
                   "grows with the phase length; compare it between continuous runs only.")
    for r in ctx.runs:
        hits = r.table_hits()
        if r.plain and hits >= 0.1:
            out.append(f"{r.label}: {100 * hits:.0f} % of searchers had a provider in their routing table when the "
                       f"search started and found it at 0 ms. In {r.nodes:,} nodes the table holds a large share of "
                       "the network, so time to first provider favours plain discv5 more than in a large network; "
                       "see the per-topic table.")
    return out + sorted({f"{r.label}: {n}" for r in ctx.runs for n in r.notes})


def search_table(ctx):
    L = head(ctx)
    L.append(row(["searchers"] + [f"{len(r.results):,}" for r in ctx.runs]))
    L.append(row(["found at least one provider"] + [f"{100 * sum(1 for x in r.results if x.get('uniqueFoundAtMs')) / max(len(r.results), 1):.1f} %" for r in ctx.runs]))
    L.append(row(["time to first provider, p50 / p95"] + [
        " / ".join(fmt_s(pct(v, q)) for q in (50, 95)) for v in
        [r.first_found() for r in ctx.runs]]))
    if any(r.plain for r in ctx.runs):
        L.append(row(["first provider already in the routing table"] + [
            f"{100 * r.table_hits():.0f} %" if r.plain else "–" for r in ctx.runs]))
    for name, ts in ctx.groups:
        L.append(row([f"**{name}**: share found at the end, median"] + [
            f"{100 * pct([len(x.get('uniqueFoundAtMs') or []) / x['target'] for x in ctx.in_group(r, ts)], 50):.1f} %" if ctx.in_group(r, ts) else "–" for r in ctx.runs]))
        for f in FRACTIONS:
            cells = []
            for r in ctx.runs:
                rs = ctx.in_group(r, ts)
                t = [r.time_to(x, f) for x in rs]
                got = [v for v in t if v is not None]
                if not rs:
                    cells.append("–")
                elif len(got) * 2 >= len(rs):
                    cells.append(f"{fmt_s(float(np.median([v if v is not None else np.inf for v in t])))} ({100 * len(got) / len(rs):.0f} % reach)")
                else:
                    cells.append(f"not reached by half ({100 * len(got) / len(rs):.0f} % reach)")
                st = ctx.stats[r.label].setdefault("time_to", {}).setdefault(name, {})
                st[str(f)] = {"median_s": float(np.median([v if v is not None else np.inf for v in t])) if t else None, "reached": len(got), "of": len(rs)}
            L.append(row([f"&nbsp;&nbsp;time to {int(f * 100)} %, median"] + cells))
    return L


def per_topic_table(ctx):
    """One row per topic: size, then share found and time to 90 % for each run."""
    sizes = ctx.runs[0].size_of
    L = [row(["topic", "nodes"] + [f"{r.label}: found / to 90 %" for r in ctx.runs]),
         "|---:|---:|" + "---:|" * len(ctx.runs)]
    for t in sorted(sizes, key=lambda t: -sizes[t]):
        cells = []
        for r in ctx.runs:
            rs = [x for x in r.results if x["topic"] == t]
            if not rs:
                cells.append("–")
                continue
            share = pct([len(x.get("uniqueFoundAtMs") or []) / x["target"] for x in rs], 50)
            t90 = [r.time_to(x, 0.9) for x in rs]
            got = [v for v in t90 if v is not None]
            to90 = fmt_s(float(np.median([v if v is not None else np.inf for v in t90]))) if len(got) * 2 >= len(rs) else "–"
            cells.append(f"{100 * share:.0f} % / {to90}")
        L.append(row([str(t), f"{sizes[t]:,}"] + cells))
    return L


def first_by_topic_table(ctx):
    """One row per topic: time to the first provider, p50 / p95, and for plain
    runs the share of searchers that had one in the routing table."""
    sizes = ctx.runs[0].size_of
    L = [row(["topic", "nodes"] + [f"{r.label}: p50 / p95" + (" / in table" if r.plain else "") for r in ctx.runs]),
         "|---:|---:|" + "---:|" * len(ctx.runs)]
    for t in sorted(sizes, key=lambda t: -sizes[t]):
        cells = []
        for r in ctx.runs:
            rs = [x for x in r.results if x["topic"] == t]
            v = r.first_found(rs)
            if not v:
                cells.append(f"none found ({len(rs)} searchers)" if rs else "–")
                continue
            c = " / ".join(fmt_s(pct(v, q)) for q in (50, 95))
            cells.append(c + (f" / {100 * r.table_hits(rs):.0f} %" if r.plain else ""))
        L.append(row([str(t), f"{sizes[t]:,}"] + cells))
    return L


def load_table(ctx):
    def cell(r, f):
        return f(r) if r.traffic else "–"

    L = head(ctx)
    L.append(row(["sent per node, whole run, p50 / p95"] + [cell(r, lambda r: " / ".join(fmt_mb(pct(r.node_totals("txBytes"), q) / 1e6) for q in (50, 95))) for r in ctx.runs]))
    L.append(row(["requests served per node, whole run, p50 / max"] + [cell(r, lambda r: (lambda v: f"{pct(v, 50):,.0f} / {max(v):,.0f}")(r.served("rxMsgs", REQUESTS))) for r in ctx.runs]))
    L.append(row(["reply bytes sent per node, whole run, p50 / max"] + [cell(r, lambda r: (lambda v: f"{fmt_mb(pct(v, 50) / 1e6)} / {fmt_mb(max(v) / 1e6)}")(r.served("txBytes", REPLIES))) for r in ctx.runs]))
    for r in ctx.runs:
        if r.traffic:
            by = r.run_by_category()
            ctx.stats[r.label]["sent_share_by_category"] = {c: by[c] / max(sum(by.values()), 1) for c in CAT_NAMES}
    for c in CAT_NAMES:
        L.append(row([f"&nbsp;&nbsp;share of bytes sent: {c}"] + [cell(r, lambda r: f"{100 * ctx.stats[r.label]['sent_share_by_category'][c]:.1f} %") for r in ctx.runs]))
    if any("search_bytes_per_node_mb" in ctx.stats[r.label] for r in ctx.runs):
        def g(r, k, f):
            v = ctx.stats[r.label].get(k)
            return f(v) if v is not None else "–"
        L.append(row(["search phase: sent + received per node, mean"] + [g(r, "search_bytes_per_node_mb", fmt_mb) for r in ctx.runs]))
        L.append(row(["search phase: FINDNODE + TOPICQUERY sent per node, mean"] + [g(r, "search_requests_per_node", lambda v: f"{v:,.0f}") for r in ctx.runs]))
        L.append(row(["search phase: bytes sent per provider found"] + [g(r, "bytes_sent_per_provider_kb", lambda v: f"{v:.1f} kB") for r in ctx.runs]))
        L.append(row(["search phase: requests per provider found"] + [g(r, "requests_per_provider", lambda v: f"{v:.1f}") for r in ctx.runs]))
    return L


def peer_table(ctx):
    if not conn_runs(ctx):
        return []
    L = head(ctx)
    L.append(row(["outbound peers at the end, p50 / p5"] + [" / ".join(f"{pct([x.get('outboundConns', 0) for x in r.results], q):.0f}" for q in (50, 5)) for r in ctx.runs]))
    L.append(row(["slots filled, share of searchers"] + [f"{100 * sum(1 for x in r.results if x.get('slotsFilledAtMs', 0) > 0) / max(len(r.results), 1):.0f} %" for r in ctx.runs]))
    L.append(row(["time to fill the slots, p50"] + [fmt_s(pct([x['slotsFilledAtMs'] / 1000 for x in r.results if x.get('slotsFilledAtMs', 0) > 0], 50)) for r in ctx.runs]))
    return L


def health_table(ctx):
    hs = [r.hosts() for r in ctx.runs]
    if not any(hs):
        return []
    L = head(ctx)
    L.append(row(["node CPU, p95 / max"] + [f"{pct(h[0], 95):.0f} % / {max(h[0] or [0]):.0f} %" if h else "–" for h in hs]))
    L.append(row(["node RSS, p95 / max"] + [f"{pct(h[1], 95):.0f} / {max(h[1] or [0]):.0f} MB" if h else "–" for h in hs]))
    L.append(row(["UDP receive-buffer drops"] + [f"{h[2]:,}" if h else "–" for h in hs]))
    return L


def runs_table(ctx):
    L = [row(["label", "protocol", "lookup model", "nodes", "topics", "search phase", "run directory"]), "|---|---|---|---:|---:|---:|---|"]
    for r in ctx.runs:
        L.append(row([r.label, r.protocol, r.model, f"{r.nodes:,}", str(len(r.m.get("perTopic") or [])), fmt_s(r.search_s()),
                      f"`{os.path.basename(os.path.normpath(r.dir))}`"]))
    return L


def figure_md(ctx, stem):
    return [f"### {stem}\n", f"![{stem}](figures/{stem}.png)\n", f"*{ctx.caption(stem)}*\n"] if ctx.has(stem) else []


def write_stats(ctx):
    with open(os.path.join(ctx.out, "stats.json"), "w") as f:
        json.dump(ctx.stats, f, indent=1, default=lambda v: None)
