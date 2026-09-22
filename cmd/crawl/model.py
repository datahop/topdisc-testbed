#!/usr/bin/env python3
"""Fit a per-topic churn and popularity model from a crawl's sessions.

Usage: model.py <sessions.csv> --span HOURS [--cadence MIN] [--min-alive N]
                [--names FILE] [--out scenarios/models/crawl-<date>.json]

The model is what scenario.session_churn.model and population.topic_model
consume: for every topic (chain), its share of the live nodes, the fraction
present at the start, the survival of a session (Kaplan-Meier, sessions that
started inside the window; those open at the end are censored), the lengths
of absences that ended in a return, the fraction of departures that never
returned inside the window, and the arrival rate of new nodes. Time inside
the model is in crawl units of --cadence minutes (the probe period), like
the older Nebula model, so the same schedulers apply. A global entry over
all nodes is included as topic "*". Topics with fewer than --min-alive nodes
are folded into "other".
"""
import argparse
import collections
import csv
import json
import os


def km(durations, censored, unit):
    """Kaplan-Meier survival at each unit boundary: [[t, S(t)], ...]."""
    events = collections.Counter(int(d // unit) + 1 for d, c in zip(durations, censored) if not c)
    cens = collections.Counter(int(d // unit) + 1 for d, c in zip(durations, censored) if c)
    at_risk = len(durations)
    s, out = 1.0, [[0, 1.0]]
    last = 1
    for t in range(1, max(list(events) + list(cens) + [1]) + 1):
        d = events.get(t, 0)
        if at_risk > 0 and d:
            s *= 1 - d / at_risk
        at_risk -= d + cens.get(t, 0)
        if d or at_risk <= 0:  # keep only the points where the curve moves
            out.append([t, round(s, 6)])
        last = t
        if at_risk <= 0:
            break
    if out[-1][0] != last:
        out.append([last, out[-1][1]])
    return out


def fit(sessions, span_s, unit, warm_s):
    by_node = collections.defaultdict(list)
    for s in sessions:
        by_node[s["id"]].append(s)
    n = len(by_node)
    # Present from the start: first answered inside the warm-up, while the
    # crawl was still discovering the network. Later first answers are arrivals.
    initial = [i for i, ss in by_node.items() if ss[0]["start"] <= warm_s]
    durations, censored, gaps, forever, arrivals = [], [], [], 0, 0
    rdur, rcens = [], []  # sessions that follow a return: shorter, fitted apart
    for i, ss in by_node.items():
        for k, s in enumerate(ss):
            if k == 0 and s["start"] <= warm_s:
                continue  # started before the window: length unknown
            if k == 0:
                arrivals += 1
            end = s["end"] if s["end"] is not None else span_s
            (rdur if k > 0 else durations).append(end - s["start"])
            (rcens if k > 0 else censored).append(s["end"] is None)
            if s["end"] is not None:
                if k + 1 < len(ss):
                    gaps.append(ss[k + 1]["start"] - s["end"])
                else:
                    forever += 1
    ended = sum(1 for c in censored + rcens if not c)
    gap_hist = collections.Counter(str(int(g // unit) + 1) for g in gaps)
    # Always on: present from the start and never once away.
    still = sum(1 for i in initial if len(by_node[i]) == 1 and by_node[i][0]["end"] is None)
    return {
        "alive": n,
        "initial_up_frac": round(len(initial) / n, 4) if n else 0,
        "flicker_rate_per_h": round(sum(1 for ss in by_node.values() for k in range(1, len(ss)) if ss[k]["start"] > warm_s) / ((span_s - warm_s) / 3600) / n, 6) if n else 0,
        "gone_rate_per_h": round(sum(1 for ss in by_node.values() if ss[-1]["end"] is not None and ss[-1]["end"] > warm_s) / ((span_s - warm_s) / 3600) / n, 6) if n else 0,
        "always_on_frac": round(still / len(initial), 4) if initial else 0,
        "arrival_rate_per_h": round(arrivals / ((span_s - warm_s) / 3600) / n, 6) if n else 0,
        "km_survival": km(durations, censored, unit),
        "km_survival_after_return": km(rdur, rcens, unit),
        "gap_hist_crawls": dict(sorted(gap_hist.items(), key=lambda kv: int(kv[0]))),
        "leave_forever_frac": round(forever / ended, 4) if ended else 0,
        "sessions_fitted": len(durations) + len(rdur),
    }


def prefix_hist(nodes_csv, alive_ids):
    """Per chain, how many alive IPv4 nodes sit in each /24."""
    hist = collections.defaultdict(collections.Counter)
    for r in csv.DictReader(open(nodes_csv)):
        ip = r.get("ip", "")
        if r["id"] not in alive_ids or not ip or ":" in ip:
            continue
        a, b, c, _ = (int(x) for x in ip.split("."))
        hist[r["chain"]][(a << 16) | (b << 8) | c] += 1
    return hist


def hist_list(counter):
    """"a.b.c" for one node in the /24, "a.b.c*n" for n; sorted, so the file diffs."""
    out = []
    for p, n in sorted(counter.items()):
        s = "%d.%d.%d" % (p >> 16, (p >> 8) & 255, p & 255)
        out.append(s if n == 1 else "%s*%d" % (s, n))
    return out


def dump(model, path):
    """indent=1 JSON, except the prefix lists, which stay on one line each."""
    marks = {}
    for i, t in enumerate(model["topics"] + [model["global"]]):
        if "prefixes" in t:
            marks["@P%d@" % i] = json.dumps(t["prefixes"], separators=(",", ":"))
            t["prefixes"] = "@P%d@" % i
    text = json.dumps(model, indent=1)
    for k, v in marks.items():
        text = text.replace('"%s"' % k, v)
    open(path, "w").write(text)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("sessions")
    ap.add_argument("--span", type=float, required=True, help="hours the crawl covered")
    ap.add_argument("--cadence", type=float, default=2.0, help="minutes per crawl unit (the probe period)")
    ap.add_argument("--min-alive", type=int, default=10, help="topics with fewer alive nodes are folded into 'other'")
    ap.add_argument("--warm", type=float, default=60.0, help="minutes at the start not counted for arrivals")
    ap.add_argument("--names", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "chains.json"), help="JSON {chain id: name}")
    ap.add_argument("--source", default="")
    ap.add_argument("--nodes", default="", help="nodes.csv from sessions.py: adds each topic's /24 prefix histogram (IPv4 nodes that answered at least once)")
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    unit, span_s, warm_s = a.cadence * 60, a.span * 3600, a.warm * 60
    names = json.load(open(a.names)) if a.names and os.path.exists(a.names) else {}

    sessions = []
    for r in csv.DictReader(open(a.sessions)):
        sessions.append({"id": r["id"], "chain": r["chain"], "start": float(r["start"]),
                         "end": float(r["end"]) if r["end"] else None, "initial": r["initial"] == "1"})
    sessions.sort(key=lambda s: (s["id"], s["start"]))
    by_chain = collections.defaultdict(list)
    for s in sessions:
        by_chain[s["chain"]].append(s)
    alive = {c: len({s["id"] for s in ss}) for c, ss in by_chain.items()}
    total = sum(alive.values())
    prefixes = prefix_hist(a.nodes, {s["id"] for s in sessions}) if a.nodes else {}

    topics, other = [], []
    for c, n in sorted(alive.items(), key=lambda kv: -kv[1]):
        if n >= a.min_alive:
            t = {"id": c, "name": names.get(c, ""), "share": round(n / total, 5)}
            t.update(fit(by_chain[c], span_s, unit, warm_s))
            if c in prefixes:
                t["prefixes"] = hist_list(prefixes[c])
            topics.append(t)
        else:
            other.extend(by_chain[c])
    if other:
        t = {"id": "other", "name": "topics below min-alive, folded", "share": round(len({s['id'] for s in other}) / total, 5)}
        t.update(fit(other, span_s, unit, warm_s))
        if prefixes:
            t["prefixes"] = hist_list(sum((prefixes[c] for c in {s["chain"] for s in other} if c in prefixes), collections.Counter()))
        topics.append(t)
    g = {"id": "*", "name": "all nodes", "share": 1.0}
    g.update(fit(sessions, span_s, unit, warm_s))
    if prefixes:
        g["prefixes"] = hist_list(sum(prefixes.values(), collections.Counter()))

    model = {
        "source": a.source or os.path.basename(a.sessions),
        "window_hours": a.span,
        "cadence_hours": round(a.cadence / 60, 6),
        "peers": total,
        # global fields keep the older loader working
        "always_on_frac": g["always_on_frac"],
        "km_survival": g["km_survival"],
        "gap_hist_crawls": g["gap_hist_crawls"],
        "global": g,
        "topics": topics,
    }
    os.makedirs(os.path.dirname(a.out) or ".", exist_ok=True)
    dump(model, a.out)
    print(f"{a.out}: {total} nodes, {len(topics)} topics (min alive {a.min_alive}), cadence {a.cadence} min, span {a.span} h")
    print(f"{'topic':22s} {'share':>6s} {'alive':>6s} {'init':>5s} {'stay':>5s} {'arr/h':>7s} {'flick/h':>8s} {'gone/h':>7s} {'S(1h)':>6s} {'S(12h)':>6s} {'gone%':>6s} {'fitted':>6s}")
    for t in topics + [g]:
        s = dict(t["km_survival"])
        s1 = s.get(int(60 / a.cadence), s[max(s)]); s12 = s.get(int(720 / a.cadence), s[max(s)])
        print(f"{t['id']:22s} {t['share']:6.3f} {t['alive']:6d} {t['initial_up_frac']:5.2f} {t['always_on_frac']:5.2f} "
              f"{100*t['arrival_rate_per_h']:6.2f}% {100*t['flicker_rate_per_h']:7.2f}% {100*t['gone_rate_per_h']:6.2f}% {s1:6.2f} {s12:6.2f} {100*t['leave_forever_frac']:5.0f}% {t['sessions_fitted']:6d}")


if __name__ == "__main__":
    main()
