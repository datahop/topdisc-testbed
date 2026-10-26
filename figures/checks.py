#!/usr/bin/env python3
"""Functional correctness checks over a run directory.

Usage:
    checks.py <run-dir> [--out checks.json] [--md checks.md] [tolerances]

Every check ends as pass, fail, not-exercised (the run never reached the
condition), needs-data (the traces do not carry what the check needs) or
blocked (waits on another issue), with the evidence that decided it. Exit
status is 1 when any check fails.

Real backends (local, cloud, Grid'5000) write a trace per node, which most
checks read; simnet writes metrics.json, series.json and oh.json only, so the
registrar-side checks run on the network-wide series there.
"""
import argparse
import bisect
import functools
import gc
import glob
import json
import math
import multiprocessing
import os
import re
import statistics
import sys
from array import array
from collections import Counter, defaultdict

import bootnodes

AD_LIFETIME_MS = 900_000
CACHE_SIZE = 5000
REG_BUCKET_SIZE = 5
TOPIC_NODES_LIMIT = 16
AUX_NODES_LIMIT = 8
SAMPLE_MS = 30_000  # assignment sample_ms default
LOAD_WORKERS = min(32, os.cpu_count() or 1)
LOAD_CHUNK = 32


# ------------------------------------------------------------------ loading
#
# The loader keeps, per node, the trace fields the checks read and reduces the
# per-event lists (samples, admissions, wait times, lookup results, ops) to
# what each check consumes. A reduction mirrors the check's own expression; an
# exception it raises is kept and re-raised where the check reads the value,
# so a malformed trace still fails only the checks that read it.

class Failed:
    def __init__(self, e):
        self.e = e


def derive(fn):
    try:
        return fn()
    except Exception as e:
        return Failed(e)


def got(v):
    if isinstance(v, Failed):
        raise v.e
    return v


def tally(items):
    """(count, first 10) of an offender stream."""
    n, first = 0, []
    for x in items:
        n += 1
        if len(first) < 10:
            first.append(x)
    return n, first


def merge(parts):
    """Per-node tallies, in node order, as (count, first 10) of their concatenation."""
    n, first = 0, []
    for c, f in parts:
        n += c
        first.extend(f[:max(0, 10 - len(first))])
    return n, first


def compact(xs):
    try:
        return array("q", xs)
    except (TypeError, OverflowError):  # keep floats as they are
        return xs


def slim_bucket(b):
    if not isinstance(b, dict):
        return b
    s = {k: b[k] for k in ("dist", "registered", "waiting", "standby", "target") if k in b}
    if "nodes" in b:
        nodes = b["nodes"]
        s["nodes"] = [({"ip": nd["ip"]} if "ip" in nd else {}) if isinstance(nd, dict) else nd for nd in nodes] if isinstance(nodes, list) else nodes
    return s


def slim_lookup(lk):
    if not isinstance(lk, dict):
        return lk  # malformed: the checks that read it fail on it as before
    s = {k: lk[k] for k in ("results", "hit_target", "latency_ms") if k in lk}
    if "search" in lk:
        st = lk["search"]
        s["search"] = {k: st[k] for k in ("maxTopicPerReply", "maxAuxPerReply", "queries", "received") if k in st} if isinstance(st, dict) else st
    return s


def found_results(t):
    """(result ids, found_at_ms) over the node's lookups and its connection phase."""
    ids, ats = [], []
    for lk in t.get("lookups") or []:
        for r in lk.get("found") or []:
            ids.append(r["id"])
            ats.append(lk["start_ms"] + r["at_ms"])
    conn = t.get("conn") or {}
    for r in conn.get("found") or []:
        if isinstance(r, dict):
            ids.append(r["id"])
            ats.append(conn["start_ms"] + r["at_ms"])
    return ids, compact(ats)


def wait_stats(i, t, lim):
    """W1 over one node: (admitted waits, longest, tally of waits past the timeout)."""
    src = [(topic, ms) for topic, st in (t.get("wait") or {}).items() for ms in (st.get("admittedMs") or [])]
    over = tally((i, topic[:8], ms) for topic, ms in src if ms > lim["wait_limit_ms"])
    return len(src), max((ms for _, ms in src), default=None), over


def early_admissions(i, evs, lim):
    """W2 over one node: (admissions, tally of early ones)."""
    n = [0]

    def early():
        for ev in evs:
            if not ev.get("admitted") or ev.get("renewal"):
                continue
            n[0] += 1
            if ev["waitedMs"] < ev["requiredMs"] - lim["floor_ms"]:
                yield i, ev["advertiser"][:8], ev["waitedMs"], ev["requiredMs"]
    bad = tally(early())
    return n[0], bad


def quote_pairs(i, evs, lim):
    """W3 over one node: (repeated-quote pairs, tally of undercuts)."""
    seq = defaultdict(list)
    for ev in evs:
        if ev.get("quoteMs", 0) > 0:
            seq[(ev["topic"], ev["advertiser"])].append((ev["atMs"], ev["quoteMs"]))
    n = [0]

    def undercuts():
        for key, q in seq.items():
            q.sort()
            for (t1, q1), (t2, q2) in zip(q, q[1:]):
                n[0] += 1
                if q2 < q1 - (t2 - t1) - lim["floor_ms"]:
                    yield i, key[1][:8], q1, q2, t2 - t1
    bad = tally(undercuts())
    return n[0], bad


def full_cache(i, evs, lim):
    """W4 over one node: (tally of admissions into a full cache, requests at a full cache)."""
    full = tally((i, ev["held"]) for ev in evs if ev.get("admitted") and not ev.get("renewal") and ev["held"] >= lim["cache_size"])
    return full, sum(1 for ev in evs if ev["held"] >= lim["cache_size"])


def record_address(i, evs, lim):
    """W6 over one node: (admissions, tally of record IPs other than the source)."""
    n = [0]

    def bad():
        for ev in evs:
            if not ev.get("admitted"):
                continue
            n[0] += 1
            if ev.get("recordIp") and ev.get("fromIp") and ev["recordIp"] != ev["fromIp"]:
                yield i, ev["advertiser"][:8], ev["recordIp"], ev["fromIp"]
    b = tally(bad())
    return n[0], b


def idle_buckets(i, t, lim):
    """R3 over time for one node: tally of buckets idle with candidates past the allowance."""
    done = t.get("reg_complete_ms") or 0
    idle_since = {}

    def runs():
        for smp in t.get("samples") or []:
            if smp["at_ms"] < done:
                continue
            for k, b in enumerate(smp.get("reg_buckets") or []):
                if b["registered"] == 0 and b["waiting"] == 0 and b["standby"] > 0:
                    idle_since.setdefault(k, smp["at_ms"])
                    if smp["at_ms"] - idle_since[k] > lim["idle_bucket_ms"]:
                        yield i, k, (smp["at_ms"] - idle_since[k]) // 1000
                        idle_since[k] = smp["at_ms"]
                else:
                    idle_since.pop(k, None)
    return tally(runs())


def up_intervals(t, lim):
    """When the node's processes ran by its trace, merged: each recorded
    session, and each sample give or take a sample window (the sample period
    plus the clock skew). A session shorter than the period leaves no sample
    but is still in sessions[]."""
    w = lim["sample_window_ms"]
    sessions = t["sessions"] if "sessions" in t else [t["session"]] if t.get("session") else []
    iv = sorted([(x["start_ms"], x["end_ms"]) for x in sessions or [] if isinstance(x, dict) and "start_ms" in x and "end_ms" in x]
                + [(x["at_ms"] - w, x["at_ms"] + w) for x in t.get("samples") or []])
    out = []
    for a, b in iv:
        if out and a <= out[-1][1]:
            out[-1] = (out[-1][0], max(out[-1][1], b))
        else:
            out.append((a, b))
    return out


def first_record(t):
    """When the trace first records the node running: its first session start or sample."""
    sessions = t["sessions"] if "sessions" in t else [t["session"]] if t.get("session") else []
    at = [x["start_ms"] for x in sessions or [] if isinstance(x, dict) and "start_ms" in x] + [x["at_ms"] for x in t.get("samples") or []]
    return min(at, default=None)


def slim_node(i, t, lim):
    """The fields of a trace (or simnet node state) the checks read, per-event lists reduced."""
    s = {k: t[k] for k in ("idx", "id", "topic", "legacy", "stop_ms", "ads_snapshot_ms", "stale_ad_max_ms",
                           "reg_complete_ms", "search_at_ms", "ads_final", "ads_final_expiry_ms",
                           "ghosts_assigned", "ghosts_in_table", "table_size") if k in t}
    if "ads_first_seen_ms" in t:
        v = t["ads_first_seen_ms"]
        s["ads_first_seen_ms"] = derive(lambda: {topic: list(seen) for topic, seen in v.items()} if isinstance(v, dict) else v)
    if "reg_buckets_final" in t:
        v = t["reg_buckets_final"]
        s["reg_buckets_final"] = [slim_bucket(b) for b in v] if isinstance(v, list) else v
    smp = t.get("samples")
    s["stop"] = derive(lambda: smp[-1]["at_ms"] if smp else None)
    s["up"] = derive(lambda: up_intervals(t, lim))
    s["first_record"] = derive(lambda: first_record(t))
    # the fork's plain build samples counters only, no cache
    reg = smp if not t.get("legacy") else None
    s["cache"] = derive(lambda: (max(x["cache_held"] for x in reg), reg[0]["cache_cap"]) if reg else None)
    s["cache_peak"] = derive(lambda: max((x["cache_held"] for x in reg or []), default=None))
    s["sampled_buckets"] = derive(lambda: bool((t.get("samples") or [{}])[0].get("reg_buckets")))
    if t.get("reg_buckets_final"):
        s["idle"] = derive(lambda: idle_buckets(i, t, lim))
    s["wait"] = derive(lambda: wait_stats(i, t, lim))
    s["has_admissions"] = "admissions" in t
    evs = t.get("admissions") or []
    s["w2"] = derive(lambda: early_admissions(i, evs, lim))
    s["w3"] = derive(lambda: quote_pairs(i, evs, lim))
    s["w4"] = derive(lambda: full_cache(i, evs, lim))
    s["w6"] = derive(lambda: record_address(i, evs, lim))
    s["ops"] = derive(lambda: [{**{k: op[k] for k in ("msg",) if k in op}, "nodeIds": tuple(set(op["nodeIds"]))}
                               for op in t.get("ops") or [] if "nodeIds" in op])
    s["lookups"] = derive(lambda: [slim_lookup(lk) for lk in t.get("lookups") or []])
    # per lookup, distinct results yielded (None: counts only); L6 alone reads the list
    s["n_found"] = derive(lambda: [len(lk.get("found") or []) if "found" in lk else None for lk in t.get("lookups") or []])
    s["found"] = derive(lambda: found_results(t))
    s["conn_found"] = derive(lambda: len((t.get("conn") or {}).get("found") or []))
    return s


def load_trace(job, lim):
    k, path = job
    with open(path) as f:
        t = json.load(f)
    return k, slim_node(t["idx"], t, lim)


def intern_ids(t):
    """Share the node ids and topic hashes the nodes' slim traces repeat."""
    def s(x):
        return sys.intern(x) if type(x) is str else x
    for k in ("id", "topic"):
        if k in t:
            t[k] = s(t[k])
    v = t.get("ads_final")
    if isinstance(v, dict):
        t["ads_final"] = {s(tp): [s(a) for a in ads] if isinstance(ads, list) else ads for tp, ads in v.items()}
    v = t.get("ads_first_seen_ms")
    if isinstance(v, dict):
        t["ads_first_seen_ms"] = {s(tp): [s(a) for a in ads] if isinstance(ads, list) else ads for tp, ads in v.items()}
    v = t.get("ads_final_expiry_ms")
    if isinstance(v, dict):
        t["ads_final_expiry_ms"] = {s(tp): {s(a): e for a, e in m.items()} if isinstance(m, dict) else m for tp, m in v.items()}
    if isinstance(t["found"], tuple):
        ids, ats = t["found"]
        t["found"] = ([s(x) for x in ids], ats)
    if isinstance(t["ops"], list):
        for op in t["ops"]:
            op["nodeIds"] = tuple(s(x) for x in op["nodeIds"])
    return t


# metrics.json fields the checks read; the rest, mostly per-result arrays, is skipped
METRICS_KEYS = ("topicIds", "registrationCoverage", "deadResults")
RESULT_KEYS = ("nodeId", "nodeIdx", "topic", "foundExtra")


def slim_metrics(m):
    if not isinstance(m, dict):
        return m
    s = {k: m[k] for k in METRICS_KEYS if k in m}
    if "results" in m:
        r = m["results"]
        s["results"] = [{k: e[k] for k in RESULT_KEYS if k in e} if isinstance(e, dict) else e for e in r] if isinstance(r, list) else r
    return s


class Unstreamable(Exception):
    pass


def load_metrics(path):
    """metrics.json reduced to what the checks read.

    Both writers (pkg/coordinator, cmd/simnet) indent by two spaces, which puts
    each top-level key on a line at indent 2 and each result's fields at
    indent 6; the file is streamed on that layout and falls back to json.load
    for any other.
    """
    try:
        return stream_metrics(path)
    except (Unstreamable, ValueError):
        with open(path) as f:
            return slim_metrics(json.load(f))


DECODER = json.JSONDecoder()


def value(v):
    try:
        return json.loads(v)
    except ValueError:
        raise Unstreamable(v[:80])


def key_line(line, indent):
    """(key, value text without the trailing comma) of a `"key": value` line at indent."""
    if line[:indent] != b" " * indent or line[indent:indent + 1] != b'"':
        raise Unstreamable(line[:80])
    try:
        text = line.decode()
        key, end = DECODER.raw_decode(text, indent)
    except ValueError:
        raise Unstreamable(line[:80])
    if text[end:end + 2] != ": ":
        raise Unstreamable(line[:80])
    v = text[end + 2:].rstrip()
    return key, (v[:-1] if v.endswith(",") else v).encode()


OPENERS = (b"[", b"{")
NESTING = {b"[": 1, b"{": 1, b"]": -1, b"}": -1, b"],": -1, b"},": -1}
BRACKET = re.compile(rb"[][{}]")


def nesting(line):
    """How a line inside an indented value changes the nesting depth. Only a
    lone opener or closer, a key and an opener, or complete values pass."""
    s = line.strip()
    d = NESTING.get(s)
    if d is not None:
        return d
    s = s[:-1] if s.endswith(b",") else s
    if s.endswith(OPENERS):
        key, sep, rest = s[:-1].rpartition(b":")
        try:
            if sep and not rest.strip() and isinstance(json.loads(key), str):
                return 1
        except ValueError:
            pass
        raise Unstreamable(line[:80])
    try:
        json.loads(s)
    except ValueError:
        value(b"{" + s + b"}")  # one or more `"key": value` pairs
    return 0


def read_block(f, first):
    """The JSON text of a value opened by `first` at the end of the previous line, through its close."""
    buf, depth, search = [first], 1, BRACKET.search
    for line in f:
        if search(line) is not None:
            depth += nesting(line)
            if depth == 0:
                buf.append(line.strip().rstrip(b","))
                return b"".join(buf)
        buf.append(line)
    raise Unstreamable("unterminated block")


def skip_block(f):
    depth, search = 1, BRACKET.search
    for line in f:
        if search(line) is not None:
            depth += nesting(line)
            if depth == 0:
                return
    raise Unstreamable("unterminated block")


def stream_results(f):
    out, cur = [], None
    for line in f:
        s = line.strip()
        if line.startswith(b'      "') and cur is not None:
            key, v = key_line(line, 6)
            if v in OPENERS:
                if key in RESULT_KEYS:
                    cur[key] = value(read_block(f, v))
                else:
                    skip_block(f)
            else:
                x = value(v)
                if key in RESULT_KEYS:
                    cur[key] = x
        elif line.startswith(b"    ") and not line.startswith(b"     ") and s in (b"}", b"},") and cur is not None:
            out.append(cur)
            cur = None
        elif line.startswith(b"  ") and not line.startswith(b"   ") and s in (b"]", b"],") and cur is None:
            return out
        elif line.startswith(b"    ") and not line.startswith(b"     ") and cur is None:
            if s == b"{":
                cur = {}
            else:  # a whole element on one line
                out.append(slim_metrics({"results": [value(s[:-1] if s.endswith(b",") else s)]})["results"][0])
        else:
            raise Unstreamable(line[:80])
    raise Unstreamable("unterminated results")


def stream_metrics(path):
    out = {}
    with open(path, "rb") as f:
        if f.readline().rstrip() != b"{":
            raise Unstreamable("not an indented object")
        for line in f:
            if line.rstrip() == b"}":
                if f.read().strip():
                    raise Unstreamable("data after the object")
                return out
            key, v = key_line(line, 2)
            if key == "results" and v == b"[":
                out[key] = stream_results(f)
            elif v in OPENERS:
                if key in METRICS_KEYS or key == "results":
                    out.update(slim_metrics({key: value(read_block(f, v))}))
                else:
                    skip_block(f)
            else:
                x = value(v)
                if key in METRICS_KEYS or key == "results":
                    out.update(slim_metrics({key: x}))
    raise Unstreamable("unterminated object")


class Run:
    def __init__(self, rd, tol=None):
        self.rd = rd
        tol = tol or argparser().parse_args([rd])
        self.params = self.read_params()
        self.churn = self.load("churn.json")
        self.assign = {}
        for p in glob.glob(os.path.join(rd, "assignments", "node*.json")):
            a = json.load(open(p))
            a["n_ghosts"] = len(a.pop("ghosts", None) or [])
            self.assign[a["idx"]] = a
        # the coordinator times every node's churn from node 0's search start (pkg/coordinator/drive.go)
        # without node 0's search start, each node's own (the coordinator's base cannot be recovered)
        a0 = self.assign.get(0)
        self.churn_base = (a0.get("phases") or {}).get("search_at_ms") if a0 else None
        self.churn_base_note = ("churn.json" if not self.assign else "node 0" if self.churn_base is not None
                                else "per node (node 0 assignment missing)" if a0 is None else "per node (node 0 has no search start)")
        # scenario-level topic parameters: 0 means the fork default
        t = next(iter(self.assign.values()))["topic"] if self.assign else {}
        self.ad_lifetime = t.get("ad_lifetime_ms") or self.param_ms("scenario.topic.ad_lifetime") or AD_LIFETIME_MS
        self.cache_size = t.get("ad_cache_size") or int(self.params.get("scenario.topic.ad_cache_size") or 0) or CACHE_SIZE
        self.attempt_timeout = t.get("reg_attempt_timeout_ms") or self.param_ms("scenario.topic.reg_attempt_timeout") or int(1.5 * self.ad_lifetime)
        self.bucket_size = t.get("reg_bucket_size") or int(self.params.get("scenario.topic.reg_bucket_size") or 0) or REG_BUCKET_SIZE
        self.nodes_limit = t.get("topic_nodes_limit") or int(self.params.get("scenario.topic.topic_nodes_limit") or 0) or TOPIC_NODES_LIMIT
        self.aux_limit = t.get("aux_nodes_limit") or int(self.params.get("scenario.topic.aux_nodes_limit") or 0) or AUX_NODES_LIMIT
        # The advertiser bounds the sum of the quoted waits by the attempt
        # timeout; the registrar measures wall time since the first ticket,
        # which adds each round's RTT and retry latency on top.
        self.wait_limit = int(round(self.attempt_timeout * (1 + tol.attempt_slack))) + tol.skew_ms
        lim = {"cache_size": self.cache_size, "wait_limit_ms": self.wait_limit, "floor_ms": tol.floor_ms,
               "skew_ms": tol.skew_ms, "idle_bucket_ms": tol.idle_bucket_ms,
               "sample_window_ms": max((a.get("sample_ms") or SAMPLE_MS for a in self.assign.values()), default=SAMPLE_MS) + tol.skew_ms}
        self.sample_window = lim["sample_window_ms"]
        self.live, self.sched = {}, {}
        # A 25k-node run holds 64 GB of traces and a 14 GB metrics.json:
        # workers parse them and hand back only what the checks read.
        mp = os.path.join(rd, "metrics.json")
        paths = glob.glob(os.path.join(rd, "traces", "node*.json"))
        gc.freeze()  # keep the forked workers from copying the parent's heap
        with multiprocessing.Pool(LOAD_WORKERS) as pool:
            metrics = pool.apply_async(load_metrics, (mp,)) if os.path.exists(mp) else None
            got = {}
            for k, t in pool.imap_unordered(functools.partial(load_trace, lim=lim), enumerate(paths), chunksize=LOAD_CHUNK):
                got[k] = intern_ids(t)
            self.metrics = metrics.get() if metrics else None
        gc.unfreeze()
        self.traces = {}
        for k in range(len(paths)):
            t = got.pop(k)
            self.traces[t["idx"]] = t
        # A trace whose first record comes after the node's stop holds only a
        # restart after the run: its real trace was lost. It counts as no
        # trace, for liveness and for the checks' unknown ids.
        self.uncovered = set()
        for i, t in self.traces.items():
            first = t["first_record"]
            stop = (self.assign.get(i, {}).get("phases") or {}).get("stop_at_ms")
            if not isinstance(first, Failed) and first is not None and stop is not None and first >= stop:
                self.uncovered.add(i)
        self.covered = len(self.traces) - len(self.uncovered)
        self.oh = self.load("oh.json") or []
        self.simnet = not self.traces
        # series.json carries what the checks read only when there are no traces
        self.series = self.load("series.json") if self.simnet else None
        self.host = []
        for p in glob.glob(os.path.join(rd, "hostmetrics", "host*.json")):
            self.host.append(json.load(open(p)))
        one = self.load("hostmetrics.json")  # the local backend: one host
        if one:
            self.host.append(one if isinstance(one, list) else one.get("samples") or [])
        # simnet writes the registrar and advertiser state of every node at the
        # end of the run instead of per-node traces
        self.state = {}
        if self.simnet:
            ads = self.load("ads.json") or {}
            buckets = self.load("buckets.json") or {}
            adm = self.load("admissions.json") or {}
            for i, per_topic in ads.get("nodes", {}).items():
                st = self.state.setdefault(int(i), {"idx": int(i)})
                st["ads_final"] = {t: list(m) for t, m in per_topic.items()}
                st["ads_final_expiry_ms"] = per_topic
                st["stop_ms"] = ads.get("at_ms")
                st["ads_snapshot_ms"] = ads.get("at_ms")
                st["stale_ad_max_ms"] = ads.get("stale_max_ms", 0)
            for i, bs in buckets.items():
                self.state.setdefault(int(i), {"idx": int(i)})["reg_buckets_final"] = bs
            for i, ev in adm.items():
                self.state.setdefault(int(i), {"idx": int(i)})["admissions"] = ev
            self.state = {i: slim_node(i, st, lim) for i, st in self.state.items()}
            self.has_admissions = bool(adm) or os.path.exists(os.path.join(rd, "admissions.json"))
        else:
            self.has_admissions = any(t["has_admissions"] for t in self.traces.values())
        self.topic_ids = self.metrics.get("topicIds", {}) if self.metrics else {}
        # node id -> assignment idx, topics (hash), legacy
        self.topic_of = {}  # idx -> topic hash (real backends: from the trace; simnet: from the results)
        for i, t in self.traces.items():
            if t.get("topic"):
                self.topic_of[i] = t["topic"]
        self.id_idx = {}
        for i, t in self.traces.items():
            self.id_idx[t["id"]] = i
        for r in (self.metrics or {}).get("results", []):
            self.id_idx.setdefault(r["nodeId"], r["nodeIdx"])
            # simnet traces and plain-node traces carry no topic hash
            if "topic" in r and (self.simnet or "topic" not in self.traces.get(r["nodeIdx"], {})):
                self.topic_of.setdefault(r["nodeIdx"], self.topic_hash(r["topic"]))
        # a node without a topic in its trace or a search result (a plain node
        # offline for the whole search): its assignment's, as metrics.json maps names
        for i, a in self.assign.items():
            m = re.fullmatch(r"topic-(\d+)", (a.get("topics") or [""])[0])
            if i not in self.topic_of and m and self.topic_hash(int(m.group(1))):
                self.topic_of[i] = self.topic_hash(int(m.group(1)))
        for n in self.oh:
            if n.get("id"):
                self.id_idx.setdefault(n["id"], n["idx"])
        self.legacy = {i: a.get("legacy", False) for i, a in self.assign.items()}
        self.bootnodes = bootnodes.from_assignments(self.assign)
        for i, t in self.traces.items():
            self.legacy[i] = t.get("legacy", False)

    def topic_hash(self, index):
        for h, i in self.topic_ids.items():
            if i == index:
                return h
        return None

    def nodes(self):
        """Per-node registrar and advertiser state: the traces, or simnet's dumps."""
        if self.traces:
            for i, t in self.traces.items():
                st = dict(t)
                st["stop_ms"] = got(t["stop"])
                yield i, st
        else:
            for i, st in self.state.items():
                yield i, st

    def load(self, name):
        p = os.path.join(self.rd, name)
        return json.load(open(p)) if os.path.exists(p) else None

    def read_params(self):
        p = os.path.join(self.rd, "run.log")
        if not os.path.exists(p):
            return {}
        for ln in open(p, errors="replace"):
            if ln.startswith("PARAMS:"):
                return dict(kv.split("=", 1) for kv in ln[7:].split() if "=" in kv)
        return {}

    def param_ms(self, key):
        v = self.params.get(key, "")
        m = re.fullmatch(r"(?:(\d+)h)?(?:(\d+)m)?(?:([\d.]+)s)?", v)
        if not v or not m or v == "0s":
            return 0
        h, mi, s = (float(x) if x else 0 for x in m.groups())
        return int(((h * 60 + mi) * 60 + s) * 1000)

    def has_churn(self):
        return bool(self.churn) or any(a.get("churn") for a in self.assign.values())

    def offline_intervals(self, idx):
        """[(down_ms, up_ms)] absolute, as scheduled: the assignment (real) or churn.json (simnet)."""
        if idx not in self.sched:
            if idx in self.assign and self.assign[idx].get("churn"):
                a = self.assign[idx]
                base = self.churn_base if self.churn_base is not None else a["phases"]["search_at_ms"]
                self.sched[idx] = [(base + int(e["DownAt"] * 1000), base + int(e["UpAt"] * 1000)) for e in a["churn"]]
            elif self.churn and str(idx) in self.churn.get("nodes", {}):
                self.sched[idx] = [(int(e["DownAt"] * 1000), int(e["UpAt"] * 1000)) for e in self.churn["nodes"][str(idx)]]
            else:
                self.sched[idx] = []
        return self.sched[idx]

    def untraced(self, j):
        """Whether an id is unknown to the checks: no index, or a trace that covers nothing of the run."""
        return j is None or j in self.uncovered

    def scheduled_down_since(self, idx, at_ms):
        """ms the node had been offline at at_ms by the churn schedule, or None when it was up."""
        for down, up in self.offline_intervals(idx):
            if down <= at_ms < up:
                return at_ms - down
        return None

    def liveness(self, idx):
        """(when the node ran by its trace, start, stop), or None without a trace that records any.

        Kills and restarts can fail silently, so where the trace records the
        node's processes, it decides. Two departures from samples alone:
        - sessions[] counts too: a session shorter than the sample period
          leaves no sample (see up_intervals);
        - before the first record the schedule decides, not "down since the
          start": an arrival (DownAt 0) is started anyway and killed at the
          search start, and that first process leaves no record."""
        if idx not in self.live:
            t = self.traces.get(idx) if idx not in self.uncovered else None
            up = got(t["up"]) if t else None
            ph = self.assign.get(idx, {}).get("phases") or {}
            self.live[idx] = (up, ph.get("start_at_ms"), ph.get("stop_at_ms")) if up else None
        return self.live[idx]

    def down_since(self, idx, at_ms):
        """ms the node had been offline at at_ms, or None when it was up: the trace where it records the node, else the schedule."""
        lv = self.liveness(idx)
        if lv is None:
            return self.scheduled_down_since(idx, at_ms)
        up, start, stop = lv
        if start is not None and at_ms < start:
            return 0  # not started yet
        if at_ms < up[0][0]:
            return self.scheduled_down_since(idx, at_ms)
        k = bisect.bisect_right(up, (at_ms, math.inf)) - 1  # the last interval starting by at_ms
        if at_ms <= up[k][1]:
            return None
        if k == len(up) - 1 and (stop is None or at_ms > stop + self.sample_window):
            return self.scheduled_down_since(idx, at_ms)
        return at_ms - up[k][1]

    def online_ms(self, idx, start, stop):
        """ms of [start, stop), a window within the node's run, that it was up."""
        def scheduled(a, b):
            return max(0, b - a) - sum(max(0, min(up, b) - max(down, a)) for down, up in self.offline_intervals(idx))
        lv = self.liveness(idx)
        if lv is None:
            return scheduled(start, stop)
        up = lv[0]
        return scheduled(start, min(stop, up[0][0])) + sum(max(0, min(b, stop) - max(a, start)) for a, b in up)

    def cleared(self, idx, at_ms, limit):
        """Whether the schedule has the node down longer than limit at at_ms
        but its trace does not: 'up' where the trace shows it up, 'shorter'
        where down for less, else None."""
        if self.liveness(idx) is None:
            return None
        ds = self.scheduled_down_since(idx, at_ms)
        if ds is None or ds <= limit:
            return None
        d = self.down_since(idx, at_ms)
        return "up" if d is None else "shorter" if d <= limit else None


def verdict(status, summary, **evidence):
    return {"status": status, "summary": summary, "evidence": evidence}


def pf(fails, summary_ok, summary_fail, **ev):
    return verdict("fail" if fails else "pass", summary_fail if fails else summary_ok, **ev)


# ---------------------------------------------------------------- registrar side

def trace_peak(run):
    """Most ads any node held in any sample."""
    return max((p for p in (got(t["cache_peak"]) for t in run.traces.values()) if p is not None), default=0)


def c1_capacity(run, tol):
    if run.traces:
        cache = [(i, got(t["cache"])) for i, t in run.traces.items()]
        over = [(i, held, cap) for i, c in cache if c for held, cap in [c] if held > cap]
        peak = trace_peak(run)
        cap = next((c[1] for _, c in cache if c), run.cache_size)
        return pf(over, f"peak {peak} of {cap} per node", f"{len(over)} nodes over capacity", peak=peak, capacity=cap, offenders=over[:10])
    if run.series:
        peak = max(s["cacheHeld"] for s in run.series["samples"])
        cap = run.series["samples"][0].get("cacheCap", 0)
        return pf(peak > cap, f"network-wide peak {peak} of {cap} (per-node maxima not in simnet series)", "network-wide ads exceed total capacity", peak=peak, capacity=cap)
    return verdict("needs-data", "no cache samples")


def c2_no_duplicates(run, tol):
    if not any("ads_final" in st for _, st in run.nodes()):
        return verdict("needs-data", "no per-registrar ad sets (simnet: ads.json)")
    dup = []
    for i, t in run.nodes():
        for topic, ads in (t.get("ads_final") or {}).items():
            c = Counter(ads)
            for ad, n in c.items():
                if n > 1:
                    dup.append((i, topic[:8], ad[:8], n))
    return pf(dup, "one ad per advertiser and topic in every final ad set", f"{len(dup)} duplicate ads", offenders=dup[:10])


def c3_only_registrants(run, tol):
    if not any("ads_final" in st for _, st in run.nodes()):
        return verdict("needs-data", "no per-registrar ad sets (simnet: ads.json)")
    bad = []
    unknown = 0
    for i, t in run.nodes():
        for topic, seen in (got(t.get("ads_first_seen_ms")) or t.get("ads_final") or {}).items():
            for ad in seen:
                j = run.id_idx.get(ad)
                if run.untraced(j):
                    unknown += 1
                elif run.topic_of.get(j) != topic:
                    bad.append((i, topic[:8], j))
    complete = run.simnet or run.covered >= len(run.assign) > 0
    st = "fail" if bad or (unknown and complete) else "pass"
    return verdict(st, f"{len(bad)} ads from nodes not registering that topic; {unknown} ads from ids without a trace"
                   + ("" if complete or not unknown else " (traces missing, see H2)"), offenders=bad[:10], unknown_ids=unknown)


def c4_only_capable(run, tol):
    if not any(run.legacy.values()):
        return verdict("not-exercised", "no legacy nodes in the run")
    if not any("ads_final" in st for _, st in run.nodes()):
        return verdict("needs-data", "no per-registrar ad sets (simnet: ads.json)")
    bad = [(i, ad[:8]) for i, t in run.nodes() for seen in (t.get("ads_final") or {}).values()
           for ad in seen if run.legacy.get(run.id_idx.get(ad, -1), False)]
    return pf(bad, "no legacy node in any cache", f"{len(bad)} ads from legacy nodes", offenders=bad[:10])


def c5_no_self_ads(run, tol):
    if not any("ads_final" in st for _, st in run.nodes()):
        return verdict("needs-data", "no per-registrar ad sets (simnet: ads.json)")
    idx_id = {i: nid for nid, i in run.id_idx.items()}
    bad = [i for i, t in run.nodes() if any(idx_id.get(i) in seen for seen in (t.get("ads_final") or {}).values())]
    return pf(bad, "no registrar holds its own ad", f"{len(bad)} registrars hold their own ad", offenders=bad[:10])


def k1_departed_leave(run, tol):
    if not run.has_churn():
        return verdict("not-exercised", "no churn in the run")
    if not any("ads_final" in st for _, st in run.nodes()):
        return verdict("needs-data", "no final ad sets (simnet: ads.json)")
    stale, cleared, unknown = [], Counter(), 0
    limit = run.ad_lifetime + tol.snapshot_ms + tol.skew_ms
    for i, t in run.nodes():
        stop = t.get("ads_snapshot_ms") or t.get("stop_ms")  # when ads_final was taken
        if stop is None:
            continue
        for topic, ads in (t.get("ads_final") or {}).items():
            for ad in ads:
                j = run.id_idx.get(ad)
                if run.untraced(j):
                    unknown += 1
                    continue
                d = run.down_since(j, stop)
                if d is not None and d > limit:
                    stale.append((i, j, d // 1000))
                else:
                    cleared[run.cleared(j, stop, limit)] += 1
    note = unknown_note(unknown, "ads") + trace_note(cleared)
    return pf(stale, "no ad for a node down longer than a lifetime" + note, f"{len(stale)} ads of long-departed nodes" + note,
              offenders=stale[:10], unknown_ids=unknown, schedule_down_trace_up=cleared["up"], schedule_down_trace_shorter=cleared["shorter"],
              churn_base=run.churn_base_note)


def unknown_note(n, what):
    return f"; {n} {what} from ids without a trace (see H2)" if n else ""


def trace_note(cleared):
    """Where the trace overruled the schedule: kills that did not happen, or returns it did not know."""
    note = f"; schedule said down, trace shows up: {cleared['up']}" if cleared["up"] else ""
    return note + (f"; schedule said down longer, trace shows a shorter downtime: {cleared['shorter']}" if cleared["shorter"] else "")


def w1_bounded_wait(run, tol):
    n = 0
    if run.traces:
        parts = [got(t["wait"]) for t in run.traces.values()]
        n = sum(c for c, _, _ in parts)
        if n:
            longest = max(m for c, m, _ in parts if c)
            nover, over = merge(o for _, _, o in parts)
    elif run.series and run.series.get("waitTime"):
        src = [(None, w["topic"], ms) for w in run.series["waitTime"] for ms in (w.get("admittedMs") or [])]
        n = len(src)
        if n:
            over = [(i, topic[:8], ms) for i, topic, ms in src if ms > run.wait_limit]
            nover = len(over)
            longest = max(ms for _, _, ms in src)
    if not n:
        return verdict("needs-data", "no admitted-wait samples")
    allowed = f"{run.wait_limit / 1000:.0f} s allowed ({run.attempt_timeout / 1000:.0f} s attempt timeout + {slack_pct(tol)} + clock skew)"
    return pf(nover, f"longest admitted wait {longest / 1000:.0f} s of {allowed}",
              f"{nover} admissions past the {allowed}", longest_ms=longest, limit_ms=run.wait_limit, slack=tol.attempt_slack, offenders=over[:10], samples=n)


def slack_pct(tol):
    return f"{tol.attempt_slack * 100:g} %"


def admissions(run, check):
    """A reduction of every node's admission events, in node order."""
    return [got(t[check]) for _, t in run.nodes()]


def e1_lifetime(run, tol):
    have = [(i, t) for i, t in run.nodes() if "stale_ad_max_ms" in t]
    if not have:
        return verdict("needs-data", "ad expiry is not in the traces (fork before testbed.10)")
    stale = max(t["stale_ad_max_ms"] for _, t in have)
    late = []
    for i, t in have:
        stop = t.get("ads_snapshot_ms") or t.get("stop_ms") or 0
        for topic, m in (t.get("ads_final_expiry_ms") or {}).items():
            for ad, exp in m.items():
                if exp - stop > run.ad_lifetime + tol.skew_ms:
                    late.append((i, ad[:8], (exp - stop) // 1000))
    fails = stale > tol.stale_ms or late
    return pf(fails, f"expired ads linger at most {stale / 1000:.1f} s; no ad expiry beyond a lifetime from the last snapshot",
              f"expired ads lingered {stale / 1000:.1f} s (tolerance {tol.stale_ms / 1000:.0f} s); {len(late)} ads with expiry beyond a lifetime", stale_max_ms=stale, offenders=late[:10])


def w2_no_early_admission(run, tol):
    if not run.has_admissions:
        return verdict("needs-data", "admission events are not in the traces (fork before testbed.10)")
    parts = admissions(run, "w2")
    n = sum(c for c, _ in parts)
    nearly, early = merge(b for _, b in parts)
    if n == 0:
        return verdict("not-exercised", "no admissions recorded")
    return pf(nearly, f"every one of {n} admissions waited at least the required time minus the {tol.floor_ms / 1000:.0f} s floor",
              f"{nearly} of {n} admissions before the required wait", offenders=early, admissions=n)


def w3_lower_bound(run, tol):
    if not run.has_admissions:
        return verdict("needs-data", "admission events are not in the traces (fork before testbed.10)")
    parts = admissions(run, "w3")
    n = sum(c for c, _ in parts)
    nbad, bad = merge(b for _, b in parts)
    if n == 0:
        return verdict("not-exercised", "no advertiser was quoted twice by one registrar")
    return pf(nbad, f"no quote undercut the previous one to the same advertiser and topic by more than the time elapsed ({n} pairs)",
              f"{nbad} of {n} repeated quotes undercut the lower bound", offenders=bad, pairs=n)


def w4_full_cache(run, tol):
    peak = 0
    if run.traces:
        peak = trace_peak(run)
    elif run.series:
        peak = max(s["cacheHeld"] for s in run.series["samples"]) / max(len(run.oh) or 1, 1)
    if run.has_admissions:
        parts = admissions(run, "w4")
        nfull, full = merge(f for f, _ in parts)
        at_cap = sum(c for _, c in parts)
        if at_cap == 0:
            return verdict("not-exercised", f"no REGTOPIC arrived at a full cache (peak {peak:.0f} of {run.cache_size})", peak=peak)
        return pf(nfull, f"{at_cap} requests at a full cache, none admitted", f"{nfull} admissions into a full cache", offenders=full)
    if peak < run.cache_size:
        return verdict("not-exercised", f"no cache reached capacity (peak {peak:.0f} of {run.cache_size})", peak=peak)
    return verdict("needs-data", "occupancy at each admission is not traced")


def w6_record_address(run, tol):
    if not run.has_admissions:
        return verdict("needs-data", "admission events are not in the traces (fork before testbed.10)")
    parts = admissions(run, "w6")
    n = sum(c for c, _ in parts)
    nbad, bad = merge(b for _, b in parts)
    if n == 0:
        return verdict("not-exercised", "no admissions recorded")
    return pf(nbad, f"record IP equals the packet source for all {n} admissions", f"{nbad} admissions with a record IP other than the source", offenders=bad)


def private_ip(ip):
    return ip.startswith(("10.", "192.168.", "127.")) or any(ip.startswith(f"172.{x}.") for x in range(16, 32))


def r2_bucket_ip_diversity(run, tol):
    have = [(i, t) for i, t in run.nodes() if any(b.get("nodes") for b in t.get("reg_buckets_final") or [])]
    if not have:
        return verdict("needs-data", "registrars per bucket are not in the traces (fork before testbed.10)")
    bad, n, public = [], 0, 0
    for i, t in have:
        for b in t["reg_buckets_final"]:
            nets = Counter()
            for nd in b.get("nodes") or []:
                ip = nd.get("ip") or ""
                if not ip or private_ip(ip):
                    continue
                public += 1
                nets[".".join(ip.split(".")[:3])] += 1
            n += 1
            for net, c in nets.items():
                if c > 1:
                    bad.append((i, b["dist"], net, c))
    if public == 0:
        return verdict("not-exercised", "every registrar has a private address; the /24 rule exempts LAN addresses")
    return pf(bad, f"one registrar per /24 in every one of {n} buckets", f"{len(bad)} buckets with two registrars in one /24", offenders=bad[:10])


def i1_no_topic_requests_to_legacy(run, tol):
    leg = {nid for nid, i in run.id_idx.items() if run.legacy.get(i)}
    if not any(run.legacy.values()):
        return verdict("not-exercised", "no legacy nodes in the run")
    ops = [(i, op) for i, t in run.traces.items() for op in got(t["ops"])]
    if not ops:
        return verdict("needs-data", "request destinations are not in the traces (fork before testbed.10)")
    bad = [(i, op["msg"], len(set(op["nodeIds"]) & leg)) for i, op in ops if set(op["nodeIds"]) & leg]
    return pf(bad, f"no REGTOPIC or TOPICQUERY went to a legacy node ({len(ops)} operations)", f"{len(bad)} operations sent topic requests to legacy nodes", offenders=bad[:10])



# --------------------------------------------------------------- advertiser side

def r1_bucket_size(run, tol):
    if not any("reg_buckets_final" in st for _, st in run.nodes()):
        return verdict("needs-data", "no registration bucket state (simnet: buckets.json)")
    over = [(i, b["dist"], b["registered"] + b["waiting"]) for i, t in run.nodes()
            for b in (t.get("reg_buckets_final") or []) if b["registered"] + b["waiting"] > max(b.get("target", 0), run.bucket_size)]
    return pf(over, f"registered plus waiting within {run.bucket_size} in every final bucket", f"{len(over)} buckets over size", offenders=over[:10])


def r3_state_maintained(run, tol):
    if not any("reg_buckets_final" in st for _, st in run.nodes()):
        return verdict("needs-data", "no registration bucket state (simnet: buckets.json)")
    empty, incomplete, idle = [], [], []
    n = 0
    for i, t in run.nodes():
        if not t.get("reg_buckets_final"):
            continue
        n += 1
        for b in t["reg_buckets_final"]:
            if b["registered"] == 0 and b["waiting"] == 0 and b["standby"] > 0:
                empty.append((i, b["dist"], b["standby"]))
        a = run.assign.get(i, {})
        search_at = a.get("phases", {}).get("search_at_ms") or t.get("search_at_ms")
        if "reg_complete_ms" in t and (not t.get("reg_complete_ms") or (search_at and t["reg_complete_ms"] > search_at + tol.skew_ms)):
            incomplete.append((i, t.get("reg_complete_ms")))
        # over time: a bucket idle with candidates for longer than the allowance, after the table first completed
        idle.append(got(t["idle"]))
    nidle, idle_runs = merge(idle)
    fails = empty or nidle
    over_time = "; over time: %d idle spells beyond %d s" % (nidle, tol.idle_bucket_ms // 1000) if any(
        got(t["sampled_buckets"]) for _, t in run.nodes()) else "; over time needs periodic bucket samples"
    return verdict("fail" if fails else "pass",
                   f"{len(empty)} final buckets idle with candidates left" + over_time + f"; {len(incomplete)} of {n} tables were still filling at search start (register_wait)",
                   empty=empty[:10], incomplete=incomplete[:10], idle=idle_runs, advertisers=n)


def e2_renewal_keeps_fanout(run, tol):
    cov = ((run.metrics or {}).get("registrationCoverage") or {}).get("byTopic")
    if not cov:
        return verdict("needs-data", "no registration coverage after the register wait")
    if not any("ads_final" in st for _, st in run.nodes()):
        return verdict("needs-data", "no final ad sets (simnet: ads.json)")
    final = Counter()
    for _, t in run.nodes():
        for ads in (t.get("ads_final") or {}).values():
            for ad in set(ads):
                final[ad] += 1
    stop = max((t.get("stop_ms") or 0 for _, t in run.nodes()), default=0)
    lost, kept, ratios = [], 0, []
    for topic, c in cov.items():
        for ad, n0 in (c.get("byRegistrant") or {}).items():
            j = run.id_idx.get(ad)
            if j is not None and run.down_since(j, stop) is not None:
                continue  # departed advertisers are K1's business
            n1 = final.get(ad, 0)
            if n0 > 0:
                ratios.append(n1 / n0)
                if n1 < tol.fanout_keep * n0:
                    lost.append((j, n0, n1))
                else:
                    kept += 1
    if not ratios:
        return verdict("needs-data", "no advertiser present in both coverage and final ad sets")
    med = statistics.median(ratios)
    return pf(lost, f"final fan-out is {med:.2f}× the post-register level at the median; every advertiser above {tol.fanout_keep:.0%}",
              f"{len(lost)} advertisers below {tol.fanout_keep:.0%} of their post-register fan-out (median ratio {med:.2f})",
              median_ratio=med, offenders=lost[:10], advertisers=len(ratios))


def e3_renewal_accounting(run, tol):
    if not run.oh:
        return verdict("needs-data", "no oh.json")
    tx, rx = totals(run, "REGTOPIC(renewal)/v5")
    if tx == 0:
        return verdict("not-exercised", "no renewals sent")
    if run.has_churn():
        # A restarted advertiser sends a fresh REGTOPIC, which a registrar
        # still holding its ad counts as a renewal; the two sides' counts
        # cannot match. A1 checks the REGTOPIC totals.
        return verdict("not-exercised", f"churn: a restarted advertiser's fresh REGTOPIC arrives as a renewal where its ad is still held ({tx} renewals sent, {rx} received); A1 checks the totals")
    # A renewal to a registrar whose session was evicted since the last
    # registration goes out as an undecryptable packet first and is repeated
    # after WHOAREYOU: the sender counts two, the receiver one. At 7,500
    # nodes a third of the renewals paid that; allow the run's own share of
    # requests that set up a session, plus the tolerance.
    setups = totals(run, "SESSION-SETUP(undecryptable)")[1]
    all_tx = sum(totals(run, t)[0] for t in REQUESTS)
    rate = setups / all_tx if all_tx else 0
    gap = (tx - rx) / tx
    limit = rate + tol.loss
    note = f"; {rate:.0%} of all requests paid a session setup, which the sender counts twice"
    return pf(gap > limit or -gap > tol.loss, f"renewals: {tx} sent, {rx} received ({gap:+.2%}, within the session-setup share{note})",
              f"renewals: {tx} sent, {rx} received ({gap:+.2%}, above the session-setup share plus tolerance {limit:.0%}{note})", sent=tx, received=rx, session_setups=setups, limit=limit)


# ----------------------------------------------------------------------- search

def lookup_results(run):
    """(searcher idx, topic hash, result id, found_at_ms) for every lookup result."""
    for i, t in run.traces.items():
        ids, ats = got(t["found"])
        topic = run.topic_of.get(i)
        for rid, at in zip(ids, ats):
            yield i, topic, rid, at


def l1_right_topic(run, tol):
    if not run.traces:
        extra = sum(r.get("foundExtra", 0) for r in (run.metrics or {}).get("results", []))
        if run.metrics is None:
            return verdict("needs-data", "no metrics.json")
        return pf(extra, "no result outside the registrant set (simnet foundExtra)", f"{extra} results outside the registrant set", found_extra=extra)
    bad, unknown, n = [], 0, 0
    for i, topic, rid, _ in lookup_results(run):
        n += 1
        j = run.id_idx.get(rid)
        if run.untraced(j) or run.topic_of.get(j) is None:
            unknown += 1
        elif run.topic_of[j] != topic:
            bad.append((i, j))
    complete = run.covered >= len(run.assign) > 0
    return verdict("fail" if bad or (unknown and complete) else "pass",
                   f"{len(bad)} of {n} results from nodes not registering the topic; {unknown} from ids without a trace or a known topic"
                   + ("" if complete or not unknown else " (traces missing, see H2)"), offenders=bad[:10], results=n, unknown_ids=unknown)


def l2_capable_results(run, tol):
    if not any(run.legacy.values()):
        return verdict("not-exercised", "no legacy nodes in the run")
    if not run.traces:
        return verdict("needs-data", "simnet has no legacy nodes")
    if all(run.legacy.values()):
        return verdict("not-exercised", "no TopDisc searchers in the run")
    # a plain searcher filters on the service entry, so its results are legacy nodes by design
    bad = [(i, run.id_idx[rid]) for i, _, rid, _ in lookup_results(run)
           if not run.legacy.get(i) and run.legacy.get(run.id_idx.get(rid, -1), False)]
    return pf(bad, "no legacy node among results", f"{len(bad)} results are legacy nodes", offenders=bad[:10])


def l3_no_expired(run, tol):
    if not run.has_churn():
        return verdict("not-exercised", "no churn in the run")
    if not run.traces:
        dead = (run.metrics or {}).get("deadResults")
        if not dead:
            return verdict("needs-data", "no dead-result snapshot in metrics.json")
        return verdict("needs-data", "simnet dead-result series is aggregated; per-result age not kept")
    stale, n, cleared, unknown = [], 0, Counter(), 0
    limit = run.ad_lifetime + tol.snapshot_ms + tol.skew_ms
    for i, _, rid, at in lookup_results(run):
        n += 1
        j = run.id_idx.get(rid)
        if run.untraced(j):
            unknown += 1
            continue
        d = run.down_since(j, at)
        if d is not None and d > limit:
            stale.append((i, j, d // 1000))
        else:
            cleared[run.cleared(j, at, limit)] += 1
    note = unknown_note(unknown, "results") + trace_note(cleared)
    return pf(stale, f"no result for a node down longer than a lifetime ({n} results)" + note, f"{len(stale)} results of long-expired nodes" + note,
              offenders=stale[:10], results=n, unknown_ids=unknown, schedule_down_trace_up=cleared["up"], schedule_down_trace_shorter=cleared["shorter"],
              churn_base=run.churn_base_note)


def l4_termination(run, tol):
    if not run.traces:
        return verdict("needs-data", "per-lookup records only on real backends")
    bad, n = [], 0
    for i, t in run.traces.items():
        a = run.assign.get(i, {})
        s = a.get("search", {})
        target, timeout = s.get("target_count", 0), s.get("request_timeout_ms", 0)
        for lk in got(t["lookups"]):
            n += 1
            hit = lk.get("results", 0) >= target > 0
            if bool(lk.get("hit_target")) != hit:
                bad.append((i, "hit flag", lk.get("results"), target))
            if timeout and not hit and lk.get("latency_ms", 0) > timeout + tol.skew_ms:
                bad.append((i, "past timeout", lk.get("latency_ms"), timeout))
            if timeout and hit and lk.get("latency_ms", 0) > timeout + tol.skew_ms:
                bad.append((i, "hit after timeout", lk.get("latency_ms"), timeout))
    if n == 0:
        return verdict("not-exercised", "no lookups recorded")
    return pf(bad, f"{n} lookups ended at their target or timeout with a consistent hit flag", f"{len(bad)} lookups terminated wrongly", offenders=bad[:10], lookups=n)


def l5_reply_limits(run, tol):
    limit = run.nodes_limit + run.aux_limit
    if not run.traces:
        return verdict("needs-data", "per-reply result counts are not traced")
    exact = [(i, lk["search"]) for i, t in run.traces.items() for lk in got(t["lookups"]) if "maxTopicPerReply" in (lk.get("search") or {})]
    if exact:
        bad = [(i, st["maxTopicPerReply"], st["maxAuxPerReply"]) for i, st in exact if st["maxTopicPerReply"] > run.nodes_limit or st["maxAuxPerReply"] > run.aux_limit]
        wt = max(st["maxTopicPerReply"] for _, st in exact)
        wa = max(st["maxAuxPerReply"] for _, st in exact)
        return pf(bad, f"largest reply carried {wt} registrants of {run.nodes_limit} and {wa} neighbours of {run.aux_limit}",
                  f"{len(bad)} searches saw a reply over the limit (largest {wt} registrants, {wa} neighbours)", offenders=bad[:10], max_topic=wt, max_aux=wa)
    worst, bad, n = 0, [], 0
    for i, t in run.traces.items():
        for lk in got(t["lookups"]):
            st = lk.get("search") or {}
            q, rec = st.get("queries", 0), st.get("received", 0)
            if q:
                n += 1
                per = rec / q
                worst = max(worst, per)
                if per > limit:
                    bad.append((i, round(per, 1)))
    if n == 0:
        return verdict("not-exercised", "no search statistics")
    return pf(bad, f"mean nodes per reply at most {worst:.1f} of {limit} (per-search average; exact per-reply counts not traced)",
              f"{len(bad)} searches average more than {limit} nodes per reply", worst=worst, limit=limit, offenders=bad[:10])


def l6_no_duplicate_results(run, tol):
    if not run.traces:
        return verdict("needs-data", "per-lookup result lists only on real backends")
    dup, n = [], 0
    for i, t in run.traces.items():
        for lk, distinct in zip(got(t["lookups"]), got(t["n_found"])):
            if distinct is None:  # legacy nodes record counts only
                continue
            n += lk.get("results", 0)
            if lk.get("results", 0) > distinct:
                dup.append((i, lk["results"] - distinct))
    if n == 0:
        return verdict("not-exercised", "no lookup results")
    return pf(dup, f"every yielded result distinct within its lookup ({n} results)", f"{len(dup)} lookups yielded a registrant more than once", offenders=dup[:10])


def i2_base_discovery(run, tol):
    leg = [i for i, l in run.legacy.items() if l]
    if not leg:
        return verdict("not-exercised", "no legacy nodes in the run")
    if not run.traces:
        return verdict("needs-data", "legacy lookups only on real backends")
    members = {}
    for i, t in run.topic_of.items():
        members[t] = members.get(t, 0) + 1
    # a node alone in its topic has no provider to find
    alone = [i for i in leg if members.get(run.topic_of.get(i), 0) <= 1]

    def brief(i):
        ph = run.assign.get(i, {}).get("phases") or {}
        start, stop = ph.get("search_at_ms"), ph.get("stop_at_ms")
        return start is not None and stop is not None and run.online_ms(i, start, stop) < tol.min_search_online_ms
    # a node up only briefly during its search had no time to find anyone
    traced = [i for i in leg if i in run.traces and i not in run.uncovered]
    away = [i for i in traced if i not in alone and brief(i)]
    found = {i: sum(lk.get("results", 0) for lk in got(run.traces[i]["lookups"]))
             + got(run.traces[i]["conn_found"]) for i in traced if i not in alone and i not in away}
    short = f"online under {tol.min_search_online_ms / 60000:.0f} min of their search"
    if not found and away:
        return verdict("not-exercised", f"{len(away)} searchers skipped: {short}" + (f", {len(alone)} alone in their topic skipped" if alone else ""),
                       skipped=len(away), churn_base=run.churn_base_note)
    if not found:
        return verdict("needs-data", "no traces from legacy nodes")
    none = [i for i, n in found.items() if n == 0]
    skipped = (f", {len(alone)} alone in their topic skipped" if alone else "") + (f", {len(away)} {short} skipped" if away else "")
    return pf(none, f"every legacy node found providers ({len(found)} nodes{skipped})", f"{len(none)} of {len(found)} legacy nodes found nothing{skipped}",
              offenders=none[:10], churn_base=run.churn_base_note)


# ----------------------------------------------------------- load and validity

REQUESTS = ["FINDNODE/v5", "PING/v5", "REGTOPIC/v5", "TOPICQUERY/v5", "REGTOPIC(renewal)/v5"]


def totals(run, t):
    tx = sum(n["byType"].get(t, {}).get("txMsgs", 0) for n in run.oh)
    rx = sum(n["byType"].get(t, {}).get("rxMsgs", 0) for n in run.oh)
    return tx, rx


def accounting(run, tol, types, what):
    if not run.oh:
        return verdict("needs-data", "no oh.json")
    tx = sum(totals(run, t)[0] for t in types)
    rx = sum(totals(run, t)[1] for t in types)
    if tx == 0:
        return verdict("not-exercised", f"no {what} sent")
    # A request to a peer without a session goes out as an undecryptable
    # packet first: the sender counts it as the request, the receiver as a
    # session setup, and the request is repeated after WHOAREYOU.
    setups = totals(run, "SESSION-SETUP(undecryptable)")[1]
    all_tx = sum(totals(run, t)[0] for t in REQUESTS)
    all_rx = sum(totals(run, t)[1] for t in REQUESTS)
    # a request to a ghost record (scenario.ghosts) is never received; each one timed out once
    ghosts = sum(n["byType"].get(t, {}).get("ghostTimeouts", 0) for n in run.oh for t in REQUESTS)
    ghosts_t = sum(n["byType"].get(t, {}).get("ghostTimeouts", 0) for n in run.oh for t in types)
    by = f" and the {ghosts} requests to ghosts" if ghosts else ""
    gev = {"ghost_requests": ghosts} if ghosts else {}
    unexplained = all_tx - all_rx - setups - ghosts
    share = (tx - rx) / tx
    loss = unexplained / all_tx if all_tx else 0
    limit = tol.loss_churn if run.has_churn() else tol.loss
    what += " (churn: requests to departed nodes are lost by design)" if run.has_churn() else ""
    if any(not n.get("byType") for n in run.oh if run.legacy.get(n["idx"])):
        # stock legacy nodes count no traffic, so only the topic requests balance and only up to the session setups
        return pf(tx < rx or tx - rx > setups + ghosts_t, f"{what}: {tx} sent, {rx} received, difference within the {setups} session setups{by} (legacy nodes count no traffic)",
                  f"{what}: {tx} sent, {rx} received, difference not within the {setups} session setups{by}", sent=tx, received=rx, session_setups=setups, **gev)
    return pf(abs(loss) > limit or (tx - rx > setups + ghosts_t and not run.has_churn()),
              f"{what}: {tx} sent, {rx} received (+{share:.1%} first packets before a session); requests unexplained by session setups{by} {loss:+.2%}",
              f"{what}: {tx} sent, {rx} received; {unexplained} requests neither received nor explained by session setups{by} ({loss:+.2%}, tolerance {limit:.0%})",
              sent=tx, received=rx, session_setups=setups, unexplained=unexplained, **gev)


def a1_request_accounting(run, tol):
    return accounting(run, tol, ["REGTOPIC/v5", "TOPICQUERY/v5"], "REGTOPIC and TOPICQUERY")


def a2_every_query_answered(run, tol):
    if not run.oh:
        return verdict("needs-data", "no oh.json")
    short = []
    for n in run.oh:
        bt = n["byType"]
        asked = bt.get("TOPICQUERY/v5", {}).get("rxMsgs", 0) + bt.get("FINDNODE/v5", {}).get("rxMsgs", 0)
        replied = bt.get("TOPICNODES/v5", {}).get("txMsgs", 0) + bt.get("NODES/v5", {}).get("txMsgs", 0)
        if asked and replied < asked * (1 - tol.loss):
            short.append((n["idx"], asked, replied))
    return pf(short, "every node sent at least one TOPICNODES or NODES per TOPICQUERY and FINDNODE received (a query with aux neighbours only is answered with NODES)",
              f"{len(short)} nodes replied to fewer queries than they received", offenders=short[:10])


def a3_load_bound(run, tol):
    if not run.oh:
        return verdict("needs-data", "no oh.json")
    # registrars are the TopDisc nodes: in a mixed run the plain ones would pull the median to 0
    rx = {n["idx"]: n["byType"].get("TOPICQUERY/v5", {}).get("rxMsgs", 0) for n in run.oh if not run.legacy.get(n["idx"])}
    # the bootnode sits in every routing table: its load is reported, not bounded
    q = sorted(v for i, v in rx.items() if i not in run.bootnodes)
    med, mx = (q[len(q) // 2], q[-1]) if q else (0, 0)
    boot = [{"idx": i, "rx": rx[i], "multiple": round(rx[i] / med, 1) if med else None} for i in sorted(run.bootnodes) if i in rx]
    note = "".join(f"; bootnode {b['idx']} left out: {b['rx']} queries" + (f", {b['multiple']}× the median" if b["multiple"] is not None else "")
                   for b in boot if b["rx"])
    if mx == 0:
        return verdict("not-exercised", f"no TOPICQUERY traffic{note}", bootnodes=boot)
    if med == 0:
        busy = sum(1 for v in q if v)
        return verdict("fail", f"TOPICQUERY concentrated: {busy} of {len(q)} registrars received any, the busiest {mx}, the median 0{note}",
                       max=mx, median=0, busy=busy, registrars=len(q), limit=tol.load_multiple, bootnodes=boot)
    return pf(mx > tol.load_multiple * med, f"busiest registrar at {mx / med:.1f}× the median ({mx} vs {med} queries){note}",
              f"busiest registrar at {mx / med:.1f}× the median, above {tol.load_multiple}×{note}", max=mx, median=med, limit=tol.load_multiple,
              bootnodes=boot)


def increase(samples, value):
    """Rise of a cumulative counter over the samples where value() reads it (None: not measured there).
    A fall between two readings is a reset: the later value is all rise."""
    tot, prev = 0, None
    for smp in samples:
        v = value(smp)
        if v is None:
            continue
        if prev is not None:
            tot += v - prev if v >= prev else v
        prev = v
    return tot


def netns_measured(h):
    """Whether each sample of host h read every node namespace."""
    top = max((smp.get("netns_sampled", 0) for smp in h), default=0)
    return [smp.get("netns") is not None and 0 < smp.get("netns_sampled", 0) == (smp.get("netns_expected") or top) for smp in h]


def ena_host(h):
    """An ENA host read its counters at least once, or still failed to at its last sample. One whose
    failures stopped without a reading found it has no ENA device."""
    return any(smp.get("ena_iface") for smp in h) or bool(h[-1].get("ena_error"))


ENA_ALLOWANCES = ("pps", "bw_in", "bw_out", "conntrack", "linklocal")


def host_drops(run):
    """Drops by where they happened, over the counters the samples carry; None for runs that carry none
    beyond the root namespace's UDP errors."""
    hosts = [h for h in run.host if h]
    has = {
        "netns": [any(smp.get("netns") is not None or smp.get("netns_expected") for smp in h) for h in hosts],
        "ARP": [any(isinstance(smp.get("arp"), dict) for smp in h) for h in hosts],
        "ENA": [ena_host(h) for h in hosts],
    }
    if not any(any(v) for v in has.values()):
        return None
    cats, info, gaps = {}, {}, {}

    def covered(h, ok):
        """Whether the run's whole window was measured on h: the first and last samples read."""
        return len(h) >= 2 and ok[0] and ok[-1]

    def gap(src, n):
        if n:
            gaps[src] = gaps.get(src, 0) + n

    empty = sum(1 for h in run.host if not h)
    if any(has["netns"]):
        ns = [(h, netns_measured(h)) for h, x in zip(hosts, has["netns"]) if x]
        gap("netns", sum(1 for x in has["netns"] if not x) + empty + sum(1 for h, ok in ns if not covered(h, ok)))

        def d(k):
            return sum(increase([smp for smp, m in zip(h, ok) if m], lambda smp: smp["netns"].get(k, 0)) for h, ok in ns)
        cats["netem"] = d("qdisc_drops")
        cats["namespace UDP receive buffer"] = d("ns_udp_rcvbuf_errors")
        info.update(netns_hosts=len(ns),
                    # netem's drops count as send errors too; sndbuf beyond them is a full socket buffer
                    ns_udp_sndbuf_errors=d("ns_udp_sndbuf_errors"), ns_udp_in_errors=d("ns_udp_in_errors"),
                    # rx_dropped also counts frames no protocol took, so it is reported, not failed on
                    veth_dropped=d("veth_rx_dropped") + d("veth_tx_dropped"),
                    max_backlog_bytes=max((smp.get("netns_max_backlog_bytes", 0) for h, ok in ns for smp, m in zip(h, ok) if m), default=0))
    if any(has["ARP"]):
        arp = [h for h, x in zip(hosts, has["ARP"]) if x]
        gap("ARP", sum(1 for x in has["ARP"] if not x) + empty
            + sum(1 for h in arp if not covered(h, [isinstance(smp.get("arp"), dict) for smp in h])))
        cats["ARP"] = sum(increase(h, lambda smp, k=k: smp["arp"].get(k, 0) if isinstance(smp.get("arp"), dict) else None)
                          for h in arp for k in ("unresolved_discards", "table_fulls"))
    if any(has["ENA"]):
        ena = [h for h, x in zip(hosts, has["ENA"]) if x]
        gap("ENA", empty + sum(1 for h in ena if not covered(h, [bool(smp.get("ena_iface")) for smp in h])))
        per = {a: sum(increase(h, lambda smp, a=a: smp.get(f"ena_{a}_allowance_exceeded", 0) if smp.get("ena_iface") else None) for h in ena)
               for a in ENA_ALLOWANCES}
        cats["ENA allowance"] = sum(per.values())
        info.update(ena_hosts=len(ena), ena_allowance=per)
    cats["host UDP receive buffer"] = sum(increase(h, lambda smp: smp.get("udp_rcvbuf_errors")) for h in hosts)
    return cats, info, gaps


def h1_no_drops(run, tol):
    if run.host:
        hd = host_drops(run)
        if hd is None:
            drops = 0
            for h in run.host:
                if h:
                    drops += max(0, h[-1].get("udp_rcvbuf_errors", 0) - h[0].get("udp_rcvbuf_errors", 0))
            return pf(drops, f"no UDP receive-buffer drops on {len(run.host)} hosts", f"{drops} UDP receive-buffer drops", drops=drops, hosts=len(run.host))
        cats, info, gaps = hd
        total = sum(cats.values())
        per = info.get("ena_allowance", {})
        part = lambda c, n: f"{c} {n}" + (" (" + ", ".join(f"{a} {x}" for a, x in per.items() if x) + ")" if c == "ENA allowance" and n else "")
        by = ", ".join(part(c, n) for c, n in cats.items())
        if "netns_hosts" in info:
            rate = int(run.params.get("testbed.wan.rate_kbps") or 0)
            by += f"; veth {info['veth_dropped']}; longest egress queue {info['max_backlog_bytes'] // 1000} kB" + (
                f" ({info['max_backlog_bytes'] * 8 / (rate * 1000):.1f} s at {rate} kbit/s)" if rate else "")
        missing = ", ".join(f"{src} on {n} hosts" for src, n in gaps.items())
        ev = dict(drops=total, hosts=len(run.host), by_category=cats, coverage_gaps=gaps, **info)
        if total:
            return verdict("fail", f"{total} packet drops: {by}" + (f"; not measured throughout: {missing}" if gaps else ""), **ev)
        if gaps:
            return verdict("needs-data", f"insufficient coverage, not measured throughout: {missing}; no drops where measured: {by}", **ev)
        return verdict("pass", f"no packet drops on {len(run.host)} hosts: {by}", **ev)
    log = os.path.join(run.rd, "run.log")
    if os.path.exists(log):
        txt = open(log, errors="replace").read()
        lines = [ln for ln in txt.splitlines() if ln.startswith("[buf]") and "dropped=" in ln]
        if lines:
            d = int(re.search(r"dropped=(\d+)", lines[-1]).group(1))
            return pf(d, "no simnet link drops", f"{d} simnet link drops", drops=d)
    return verdict("needs-data", "no host metrics or simnet buffer line")


def h2_complete_collection(run, tol):
    if run.simnet:
        return verdict("not-exercised", "simnet keeps every node in process")
    n, got, lost = len(run.assign), len(run.traces), sorted(run.uncovered)
    if n == 0:
        return verdict("needs-data", "no assignments")
    return pf(got < n or lost, f"{got} of {n} traces collected", f"{n - got} of {n} traces missing"
              + (f", {len(lost)} with no run coverage (only a restart after the stop)" if lost else ""), assigned=n, traces=got,
              no_coverage=len(lost), no_coverage_nodes=lost[:10])


def h3_hosts_under_load(run, tol):
    if not run.host:
        return verdict("needs-data", "no host metrics")
    cpu = [ns["cpu_pct"] for h in run.host for s in h for ns in s.get("nodes") or []]
    rss = [ns["rss_mb"] for h in run.host for s in h for ns in s.get("nodes") or []]
    lowmem = min((s["mem_avail_mb"] for h in run.host for s in h if s.get("mem_avail_mb")), default=None)
    if not cpu:
        return verdict("needs-data", "no node samples in host metrics")
    hot = max(cpu)
    fails = hot > tol.cpu_pct or (lowmem is not None and lowmem < tol.mem_avail_mb)
    mem = f"host memory never below {lowmem:.0f} MB" if lowmem is not None else "host memory not reported"
    return pf(fails, f"node CPU peak {hot:.0f}%, RSS peak {max(rss):.0f} MB, {mem}",
              f"host saturated: CPU peak {hot:.0f}% or free memory {lowmem} MB", cpu_peak=hot, rss_peak=max(rss), mem_avail_min=lowmem)


GHOST_FIELDS = ("ghosts_assigned", "ghosts_in_table", "table_size")


def ghosts_on(run):
    return (run.params.get("scenario.ghosts.share", "0") not in ("", "0") or bool(run.params.get("scenario.ghosts.tables"))
            or any(a["n_ghosts"] for a in run.assign.values()))


def g1_ghosts_pinned(run, tol):
    if not ghosts_on(run):
        return verdict("not-exercised", "no ghosts in this run")
    if run.simnet:
        return verdict("not-exercised", "simnet pins no ghosts")
    if not run.assign:
        return verdict("needs-data", "no assignments")
    # expected counts come from the assignments: a trace that drops or misreports them must not pass
    ran = {i: t for i, t in run.traces.items() if i in run.assign and i not in run.uncovered and got(t["first_record"]) is not None}
    expected = sum(run.assign[i]["n_ghosts"] for i in ran)
    if expected == 0:
        return verdict("fail", "ghosts configured but no node that ran was assigned one")
    missing, short = [], []
    for i, t in sorted(ran.items()):
        want = run.assign[i]["n_ghosts"]
        if not want:
            continue
        if any(k not in t for k in GHOST_FIELDS):
            missing.append(i)
        elif t["ghosts_assigned"] != want or t["ghosts_in_table"] != want:
            short.append((i, t["ghosts_in_table"], t["ghosts_assigned"], want))
    held = sum(t.get("ghosts_in_table", 0) for t in ran.values())
    size = sum(t.get("table_size", 0) for t in ran.values())
    share = f" ({held / size:.1%} of table entries)" if size else ""
    return pf(missing or short, f"{len(ran)} nodes hold all {expected} ghosts their assignments carry, at their last snapshot{share}",
              f"{len(missing)} nodes with ghosts trace no ghost fields, {len(short)} report or hold a different count than assigned",
              expected=expected, held=held, missing=missing[:10], offenders=short[:10])


def g2_ghost_timeouts(run, tol):
    if not ghosts_on(run):
        return verdict("not-exercised", "no ghosts in this run")
    if not run.oh:
        return verdict("needs-data", "no oh.json")
    ghost = own = calls = 0
    bad = []
    for n in run.oh:
        for k, c in n["byType"].items():
            g, o = c.get("ghostTimeouts", 0), c.get("ownGhostTimeouts", 0)
            ghost, own, calls = ghost + g, own + o, calls + c.get("timeouts", 0)
            if o > g:
                bad.append((n["idx"], k, o, g))
    if bad:
        return verdict("fail", f"{len(bad)} counters with more own-ghost than ghost timeouts", offenders=bad[:10])
    return pf(ghost == 0, f"{ghost} timeouts at ghosts ({own} at the caller's own) of {calls} timeouts",
              "no timeout at a ghost recorded", ghost_timeouts=ghost, own_ghost_timeouts=own, timeouts=calls)


CHECKS = [
    ("registration", [
        ("C1", "capacity: ads held never exceed the cache size", c1_capacity),
        ("C2", "no duplicates: one ad per advertiser and topic per registrar", c2_no_duplicates),
        ("C3", "only registrants: every held ad is from a node registering that topic", c3_only_registrants),
        ("C4", "only capable nodes: no legacy node in any cache", c4_only_capable),
        ("C5", "no self-ads: a registrar never holds its own ad", c5_no_self_ads),
        ("K1", "departed nodes leave caches within an ad lifetime", k1_departed_leave),
        ("E1", "lifetime: expired ads are swept promptly and no ad expires beyond a lifetime", e1_lifetime),
        ("E2", "renewal keeps fan-out: final fan-out against the post-register level", e2_renewal_keeps_fanout),
        ("E3", "renewal accounting: renewals received match renewals sent beyond session setups", e3_renewal_accounting),
        ("W1", "bounded wait: every admission within {slack} of the attempt timeout", w1_bounded_wait),
        ("W2", "no early admission: admitted only after the required wait", w2_no_early_admission),
        ("W3", "lower bound: a repeated quote never undercuts the previous one by more than the time elapsed", w3_lower_bound),
        ("W4", "full cache admits nothing", w4_full_cache),
        ("W5", "ticket validity: early, late, wrong-topic and bad-MAC tickets rejected", lambda r, t: verdict("unit-test", "TicketSealer tests in the fork; rejection counters not traced")),
        ("W6", "record address: admitted record IP equals the packet source", w6_record_address),
        ("R1", "bucket size: registered plus waiting never exceed the bucket size", r1_bucket_size),
        ("R2", "bucket IP diversity: one registrar per /24 per bucket", r2_bucket_ip_diversity),
        ("R3", "state maintained: tables complete by search start and no bucket left empty with candidates", r3_state_maintained),
    ]),
    ("search", [
        ("L1", "right topic: every result registers the requested topic", l1_right_topic),
        ("L2", "capable results: no legacy node among results", l2_capable_results),
        ("L3", "no expired results: no result down longer than a lifetime", l3_no_expired),
        ("L4", "termination: lookups end at the target or the timeout, hit flag consistent", l4_termination),
        ("L5", "reply limits: nodes per reply within the topic and aux limits", l5_reply_limits),
        ("L6", "no duplicate results within a lookup", l6_no_duplicate_results),
        ("I1", "no topic messages to legacy peers", i1_no_topic_requests_to_legacy),
        ("I2", "base discovery: legacy nodes still find providers", i2_base_discovery),
    ]),
    ("load", [
        ("A1", "request accounting: REGTOPIC and TOPICQUERY sent equal received", a1_request_accounting),
        ("A2", "every query answered: TOPICNODES sent at least TOPICQUERY received per node", a2_every_query_answered),
        ("A3", "registrar load bound: busiest registrar within a multiple of the median", a3_load_bound),
        ("H1", "no packet drops on any host", h1_no_drops),
        ("H2", "complete collection: a trace for every node", h2_complete_collection),
        ("H3", "hosts under load: node CPU and host memory within limits", h3_hosts_under_load),
        ("G1", "ghosts pinned: every node holds all its assigned ghosts at its last snapshot", g1_ghosts_pinned),
        ("G2", "ghosts cost timeouts: some ghostTimeouts recorded and every ownGhostTimeouts ≤ ghostTimeouts", g2_ghost_timeouts),
    ]),
]


# Checks that need TopDisc nodes: registration, topic replies and topic traffic.
TOPDISC_ONLY = {c for g, cs in CHECKS if g == "registration" for c, _, _ in cs} | {"L3", "L5", "I1", "A1", "A3"}


def markdown(results, run):
    L = ["| check | assertion | status | evidence |", "|---|---|---|---|"]
    for group, checks in results.items():
        for code_, desc, v in checks:
            L.append(f"| **{code_}** {group} | {desc} | {v['status']} | {v['summary']} |")
    counts = Counter(v["status"] for checks in results.values() for _, _, v in checks)
    head = ", ".join(f"{n} {s}" for s, n in sorted(counts.items(), key=lambda x: -x[1]))
    return f"{head}.\n\n" + "\n".join(L) + "\n"


def argparser():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_dir")
    ap.add_argument("--out", default="")
    ap.add_argument("--md", default="")
    ap.add_argument("--loss", type=float, default=0.02, help="tolerated share of requests lost between sent and received")
    ap.add_argument("--loss-churn", type=float, default=0.30, help="the same tolerance when the run has churn (requests to departed nodes are lost)")
    ap.add_argument("--skew-ms", type=int, default=5000, help="clock skew between hosts")
    ap.add_argument("--floor-ms", type=int, default=1000, help="admission floor: a remaining wait under this is admitted")
    ap.add_argument("--stale-ms", type=int, default=5000, help="how long an expired ad may stay in a table before the sweep")
    ap.add_argument("--idle-bucket-ms", type=int, default=120000, help="how long a bucket may sit without registrations or attempts while candidates remain")
    ap.add_argument("--snapshot-ms", type=int, default=30000, help="period of the node's cache samples")
    ap.add_argument("--fanout-keep", type=float, default=0.5, help="minimum final fan-out as a share of the post-register level")
    ap.add_argument("--incomplete-frac", type=float, default=0.01, help="tolerated share of advertisers whose table is not complete by search start")
    ap.add_argument("--load-multiple", type=float, default=10.0, help="busiest registrar at most this multiple of the median query count")
    ap.add_argument("--cpu-pct", type=float, default=80.0)
    ap.add_argument("--mem-avail-mb", type=float, default=64.0)
    ap.add_argument("--attempt-slack", type=float, default=0.02, help="share of the attempt timeout an admitted wait may exceed it by: the registrar's wall time adds each round's latency to the quoted waits")
    ap.add_argument("--min-search-online-ms", type=int, default=300000, help="a legacy searcher up less than this of its search window is not expected to find providers (I2)")
    return ap


def main():
    tol = argparser().parse_args()
    rd = os.path.abspath(tol.run_dir)
    run = Run(rd, tol)
    plain_only = bool(run.legacy) and all(run.legacy.values())
    results = {}
    for group, checks in CHECKS:
        results[group] = []
        for code_, desc, fn in checks:
            desc = desc.replace("{slack}", slack_pct(tol))
            try:
                if plain_only and code_ in TOPDISC_ONLY:
                    v = verdict("not-exercised", "every node runs plain discv5")
                else:
                    v = fn(run, tol)
            except Exception as e:  # a check must never take the report down
                v = verdict("error", f"{type(e).__name__}: {e}")
            results[group].append((code_, desc, v))
    out = tol.out or os.path.join(rd, "checks.json")
    with open(out, "w") as f:
        json.dump({"run": os.path.basename(rd), "backend": "simnet" if run.simnet else "real",
                   "parameters": {"ad_lifetime_ms": run.ad_lifetime, "cache_size": run.cache_size, "attempt_timeout_ms": run.attempt_timeout,
                                  "bucket_size": run.bucket_size, "topic_nodes_limit": run.nodes_limit, "aux_nodes_limit": run.aux_limit},
                   "tolerances": {k: v for k, v in vars(tol).items() if k not in ("run_dir", "out", "md")},
                   "checks": [{"id": c, "group": g, "assertion": d, **v} for g, cs in results.items() for c, d, v in cs]}, f, indent=1)
    md = markdown(results, run)
    if tol.md:
        open(tol.md, "w").write(md)
    print(md)
    failed = [c for cs in results.values() for c, _, v in cs if v["status"] in ("fail", "error")]
    if failed:
        print("FAILED:", ", ".join(failed))
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
