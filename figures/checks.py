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
import glob
import json
import os
import re
import statistics
import sys
from collections import Counter, defaultdict

AD_LIFETIME_MS = 900_000
CACHE_SIZE = 5000
REG_BUCKET_SIZE = 5
TOPIC_NODES_LIMIT = 16
AUX_NODES_LIMIT = 8


class Run:
    def __init__(self, rd):
        self.rd = rd
        self.params = self.read_params()
        self.metrics = self.load("metrics.json")
        self.series = self.load("series.json")
        self.oh = self.load("oh.json") or []
        self.churn = self.load("churn.json")
        self.assign = {}
        for p in glob.glob(os.path.join(rd, "assignments", "node*.json")):
            a = json.load(open(p))
            self.assign[a["idx"]] = a
        self.traces = {}
        for p in glob.glob(os.path.join(rd, "traces", "node*.json")):
            t = json.load(open(p))
            self.traces[t["idx"]] = t
        self.host = []
        for p in glob.glob(os.path.join(rd, "hostmetrics", "host*.json")):
            self.host.append(json.load(open(p)))
        one = self.load("hostmetrics.json")  # the local backend: one host
        if one:
            self.host.append(one if isinstance(one, list) else one.get("samples") or [])
        self.simnet = not self.traces
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
            self.has_admissions = bool(adm) or os.path.exists(os.path.join(rd, "admissions.json"))
        else:
            self.has_admissions = any("admissions" in t for t in self.traces.values())
        self.topic_ids = self.metrics.get("topicIds", {}) if self.metrics else {}
        # scenario-level topic parameters: 0 means the fork default
        t = next(iter(self.assign.values()))["topic"] if self.assign else {}
        self.ad_lifetime = t.get("ad_lifetime_ms") or self.param_ms("scenario.topic.ad_lifetime") or AD_LIFETIME_MS
        self.cache_size = t.get("ad_cache_size") or int(self.params.get("scenario.topic.ad_cache_size") or 0) or CACHE_SIZE
        self.attempt_timeout = t.get("reg_attempt_timeout_ms") or self.param_ms("scenario.topic.reg_attempt_timeout") or int(1.5 * self.ad_lifetime)
        self.bucket_size = t.get("reg_bucket_size") or int(self.params.get("scenario.topic.reg_bucket_size") or 0) or REG_BUCKET_SIZE
        self.nodes_limit = t.get("topic_nodes_limit") or int(self.params.get("scenario.topic.topic_nodes_limit") or 0) or TOPIC_NODES_LIMIT
        self.aux_limit = t.get("aux_nodes_limit") or int(self.params.get("scenario.topic.aux_nodes_limit") or 0) or AUX_NODES_LIMIT
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
            if self.simnet and "topic" in r:
                self.topic_of.setdefault(r["nodeIdx"], self.topic_hash(r["topic"]))
        for n in self.oh:
            if n.get("id"):
                self.id_idx.setdefault(n["id"], n["idx"])
        self.legacy = {i: a.get("legacy", False) for i, a in self.assign.items()}
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
                st["stop_ms"] = t["samples"][-1]["at_ms"] if t.get("samples") else None
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
        """[(down_ms, up_ms)] absolute, from the assignment (real) or churn.json (simnet)."""
        if idx in self.assign and self.assign[idx].get("churn"):
            a = self.assign[idx]
            base = a["phases"]["search_at_ms"]
            return [(base + int(e["DownAt"] * 1000), base + int(e["UpAt"] * 1000)) for e in a["churn"]]
        if self.churn and str(idx) in self.churn.get("nodes", {}):
            return [(int(e["DownAt"] * 1000), int(e["UpAt"] * 1000)) for e in self.churn["nodes"][str(idx)]]
        return []

    def down_since(self, idx, at_ms):
        """ms the node had been offline at at_ms, or None when it was up."""
        for down, up in self.offline_intervals(idx):
            if down <= at_ms < up:
                return at_ms - down
        return None


def verdict(status, summary, **evidence):
    return {"status": status, "summary": summary, "evidence": evidence}


def pf(fails, summary_ok, summary_fail, **ev):
    return verdict("fail" if fails else "pass", summary_fail if fails else summary_ok, **ev)


# ---------------------------------------------------------------- registrar side

def c1_capacity(run, tol):
    if run.traces:
        over = [(i, max(s["cache_held"] for s in t["samples"]), t["samples"][0]["cache_cap"])
                for i, t in run.traces.items() if t.get("samples") and max(s["cache_held"] for s in t["samples"]) > t["samples"][0]["cache_cap"]]
        peak = max((s["cache_held"] for t in run.traces.values() for s in t.get("samples", [])), default=0)
        cap = next((t["samples"][0]["cache_cap"] for t in run.traces.values() if t.get("samples")), run.cache_size)
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
        for topic, seen in (t.get("ads_first_seen_ms") or t.get("ads_final") or {}).items():
            for ad in seen:
                j = run.id_idx.get(ad)
                if j is None:
                    unknown += 1
                elif run.topic_of.get(j) != topic:
                    bad.append((i, topic[:8], j))
    complete = run.simnet or len(run.traces) >= len(run.assign) > 0
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
    stale = []
    for i, t in run.nodes():
        stop = t.get("stop_ms")
        if stop is None:
            continue
        for topic, ads in (t.get("ads_final") or {}).items():
            for ad in ads:
                j = run.id_idx.get(ad)
                if j is None:
                    continue
                d = run.down_since(j, stop)
                if d is not None and d > run.ad_lifetime + tol.snapshot_ms + tol.skew_ms:
                    stale.append((i, j, d // 1000))
    return pf(stale, "no ad for a node down longer than a lifetime", f"{len(stale)} ads of long-departed nodes", offenders=stale[:10])


def w1_bounded_wait(run, tol):
    src = None
    if run.traces:
        src = [(i, topic, ms) for i, t in run.traces.items() for topic, st in (t.get("wait") or {}).items() for ms in (st.get("admittedMs") or [])]
    elif run.series and run.series.get("waitTime"):
        src = [(None, w["topic"], ms) for w in run.series["waitTime"] for ms in (w.get("admittedMs") or [])]
    if not src:
        return verdict("needs-data", "no admitted-wait samples")
    over = [(i, topic[:8], ms) for i, topic, ms in src if ms > run.attempt_timeout + tol.skew_ms]
    longest = max(ms for _, _, ms in src)
    return pf(over, f"longest admitted wait {longest / 1000:.0f} s of {run.attempt_timeout / 1000:.0f} s allowed",
              f"{len(over)} admissions past the attempt timeout", longest_ms=longest, limit_ms=run.attempt_timeout, offenders=over[:10], samples=len(src))


def admissions(run):
    for i, t in run.nodes():
        for ev in t.get("admissions") or []:
            yield i, ev


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
    early, n = [], 0
    for i, ev in admissions(run):
        if not ev.get("admitted") or ev.get("renewal"):
            continue
        n += 1
        if ev["waitedMs"] < ev["requiredMs"] - tol.floor_ms:
            early.append((i, ev["advertiser"][:8], ev["waitedMs"], ev["requiredMs"]))
    if n == 0:
        return verdict("not-exercised", "no admissions recorded")
    return pf(early, f"every one of {n} admissions waited at least the required time minus the {tol.floor_ms / 1000:.0f} s floor",
              f"{len(early)} of {n} admissions before the required wait", offenders=early[:10], admissions=n)


def w3_lower_bound(run, tol):
    if not run.has_admissions:
        return verdict("needs-data", "admission events are not in the traces (fork before testbed.10)")
    bad, n = [], 0
    for i, t in run.nodes():
        seq = defaultdict(list)
        for ev in t.get("admissions") or []:
            if ev.get("quoteMs", 0) > 0:
                seq[(ev["topic"], ev["advertiser"])].append((ev["atMs"], ev["quoteMs"]))
        for key, q in seq.items():
            q.sort()
            for (t1, q1), (t2, q2) in zip(q, q[1:]):
                n += 1
                if q2 < q1 - (t2 - t1) - tol.floor_ms:
                    bad.append((i, key[1][:8], q1, q2, t2 - t1))
    if n == 0:
        return verdict("not-exercised", "no advertiser was quoted twice by one registrar")
    return pf(bad, f"no quote undercut the previous one to the same advertiser and topic by more than the time elapsed ({n} pairs)",
              f"{len(bad)} of {n} repeated quotes undercut the lower bound", offenders=bad[:10], pairs=n)


def w4_full_cache(run, tol):
    peak = 0
    if run.traces:
        peak = max((s["cache_held"] for t in run.traces.values() for s in t.get("samples", [])), default=0)
    elif run.series:
        peak = max(s["cacheHeld"] for s in run.series["samples"]) / max(len(run.oh) or 1, 1)
    if run.has_admissions:
        full = [(i, ev["held"]) for i, ev in admissions(run) if ev.get("admitted") and not ev.get("renewal") and ev["held"] >= run.cache_size]
        at_cap = sum(1 for _, ev in admissions(run) if ev["held"] >= run.cache_size)
        if at_cap == 0:
            return verdict("not-exercised", f"no REGTOPIC arrived at a full cache (peak {peak:.0f} of {run.cache_size})", peak=peak)
        return pf(full, f"{at_cap} requests at a full cache, none admitted", f"{len(full)} admissions into a full cache", offenders=full[:10])
    if peak < run.cache_size:
        return verdict("not-exercised", f"no cache reached capacity (peak {peak:.0f} of {run.cache_size})", peak=peak)
    return verdict("needs-data", "occupancy at each admission is not traced")


def w6_record_address(run, tol):
    if not run.has_admissions:
        return verdict("needs-data", "admission events are not in the traces (fork before testbed.10)")
    bad, n = [], 0
    for i, ev in admissions(run):
        if not ev.get("admitted"):
            continue
        n += 1
        if ev.get("recordIp") and ev.get("fromIp") and ev["recordIp"] != ev["fromIp"]:
            bad.append((i, ev["advertiser"][:8], ev["recordIp"], ev["fromIp"]))
    if n == 0:
        return verdict("not-exercised", "no admissions recorded")
    return pf(bad, f"record IP equals the packet source for all {n} admissions", f"{len(bad)} admissions with a record IP other than the source", offenders=bad[:10])


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
    ops = [(i, op) for i, t in run.traces.items() for op in t.get("ops") or [] if "nodeIds" in op]
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
    empty, incomplete, idle_runs = [], [], []
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
        done = t.get("reg_complete_ms") or 0
        idle_since = {}
        for smp in t.get("samples") or []:
            if smp["at_ms"] < done:
                continue
            for k, b in enumerate(smp.get("reg_buckets") or []):
                if b["registered"] == 0 and b["waiting"] == 0 and b["standby"] > 0:
                    idle_since.setdefault(k, smp["at_ms"])
                    if smp["at_ms"] - idle_since[k] > tol.idle_bucket_ms:
                        idle_runs.append((i, k, (smp["at_ms"] - idle_since[k]) // 1000))
                        idle_since[k] = smp["at_ms"]
                else:
                    idle_since.pop(k, None)
    fails = empty or idle_runs
    over_time = "; over time: %d idle spells beyond %d s" % (len(idle_runs), tol.idle_bucket_ms // 1000) if any(
        (t.get("samples") or [{}])[0].get("reg_buckets") for _, t in run.nodes()) else "; over time needs periodic bucket samples"
    return verdict("fail" if fails else "pass",
                   f"{len(empty)} final buckets idle with candidates left" + over_time + f"; {len(incomplete)} of {n} tables were still filling at search start (register_wait)",
                   empty=empty[:10], incomplete=incomplete[:10], idle=idle_runs[:10], advertisers=n)


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
    loss = (tx - rx) / tx
    limit = tol.loss_churn if run.has_churn() else tol.loss
    note = "; renewals reuse the session, so none are lost to setup" + ("; churn: renewals to departed registrars are lost by design" if run.has_churn() else "")
    return pf(abs(loss) > limit, f"renewals: {tx} sent, {rx} received ({loss:+.2%}{note})",
              f"renewals: {tx} sent, {rx} received ({loss:+.2%}, tolerance {limit:.0%}{note})", sent=tx, received=rx, loss=loss)


# ----------------------------------------------------------------------- search

def lookup_results(run):
    """(searcher idx, topic hash, result id, found_at_ms) for every lookup result."""
    out = []
    if run.traces:
        for i, t in run.traces.items():
            for lk in t.get("lookups") or []:
                for r in lk.get("found") or []:
                    out.append((i, t["topic"], r["id"], lk["start_ms"] + r["at_ms"]))
            conn = t.get("conn") or {}
            for r in conn.get("found") or []:
                if isinstance(r, dict):
                    out.append((i, t["topic"], r["id"], conn["start_ms"] + r["at_ms"]))
    return out


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
        if j is None:
            unknown += 1
        elif run.topic_of.get(j) != topic:
            bad.append((i, j))
    complete = len(run.traces) >= len(run.assign) > 0
    return verdict("fail" if bad or (unknown and complete) else "pass",
                   f"{len(bad)} of {n} results from nodes not registering the topic; {unknown} from ids without a trace"
                   + ("" if complete or not unknown else " (traces missing, see H2)"), offenders=bad[:10], results=n, unknown_ids=unknown)


def l2_capable_results(run, tol):
    if not any(run.legacy.values()):
        return verdict("not-exercised", "no legacy nodes in the run")
    if not run.traces:
        return verdict("needs-data", "simnet has no legacy nodes")
    bad = [(i, run.id_idx[rid]) for i, _, rid, _ in lookup_results(run) if run.legacy.get(run.id_idx.get(rid, -1), False)]
    return pf(bad, "no legacy node among results", f"{len(bad)} results are legacy nodes", offenders=bad[:10])


def l3_no_expired(run, tol):
    if not run.has_churn():
        return verdict("not-exercised", "no churn in the run")
    if not run.traces:
        dead = (run.metrics or {}).get("deadResults")
        if not dead:
            return verdict("needs-data", "no dead-result snapshot in metrics.json")
        return verdict("needs-data", "simnet dead-result series is aggregated; per-result age not kept")
    stale, n = [], 0
    for i, _, rid, at in lookup_results(run):
        n += 1
        j = run.id_idx.get(rid)
        if j is None:
            continue
        d = run.down_since(j, at)
        if d is not None and d > run.ad_lifetime + tol.snapshot_ms + tol.skew_ms:
            stale.append((i, j, d // 1000))
    return pf(stale, f"no result for a node down longer than a lifetime ({n} results)", f"{len(stale)} results of long-expired nodes", offenders=stale[:10], results=n)


def l4_termination(run, tol):
    if not run.traces:
        return verdict("needs-data", "per-lookup records only on real backends")
    bad, n = [], 0
    for i, t in run.traces.items():
        a = run.assign.get(i, {})
        s = a.get("search", {})
        target, timeout = s.get("target_count", 0), s.get("request_timeout_ms", 0)
        for lk in t.get("lookups") or []:
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
    exact = [(i, lk["search"]) for i, t in run.traces.items() for lk in t.get("lookups") or [] if "maxTopicPerReply" in (lk.get("search") or {})]
    if exact:
        bad = [(i, st["maxTopicPerReply"], st["maxAuxPerReply"]) for i, st in exact if st["maxTopicPerReply"] > run.nodes_limit or st["maxAuxPerReply"] > run.aux_limit]
        wt = max(st["maxTopicPerReply"] for _, st in exact)
        wa = max(st["maxAuxPerReply"] for _, st in exact)
        return pf(bad, f"largest reply carried {wt} registrants of {run.nodes_limit} and {wa} neighbours of {run.aux_limit}",
                  f"{len(bad)} searches saw a reply over the limit (largest {wt} registrants, {wa} neighbours)", offenders=bad[:10], max_topic=wt, max_aux=wa)
    worst, bad, n = 0, [], 0
    for i, t in run.traces.items():
        for lk in t.get("lookups") or []:
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
        for lk in t.get("lookups") or []:
            if "found" not in lk:  # legacy nodes record counts only
                continue
            distinct = len(lk.get("found") or [])
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
    found = {i: sum(lk.get("results", 0) for lk in run.traces[i].get("lookups") or []) for i in leg if i in run.traces}
    if not found:
        return verdict("needs-data", "no traces from legacy nodes")
    none = [i for i, n in found.items() if n == 0]
    return pf(none, f"every legacy node found providers ({len(found)} nodes)", f"{len(none)} of {len(found)} legacy nodes found nothing", offenders=none[:10])


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
    unexplained = all_tx - all_rx - setups
    share = (tx - rx) / tx
    loss = unexplained / all_tx if all_tx else 0
    limit = tol.loss_churn if run.has_churn() else tol.loss
    what += " (churn: requests to departed nodes are lost by design)" if run.has_churn() else ""
    if any(run.legacy.values()):
        # legacy nodes count no traffic, so only the topic requests balance and only up to the session setups
        return pf(tx < rx or tx - rx > setups, f"{what}: {tx} sent, {rx} received, difference within the {setups} session setups (legacy nodes count no traffic)",
                  f"{what}: {tx} sent, {rx} received, difference not within the {setups} session setups", sent=tx, received=rx, session_setups=setups)
    return pf(abs(loss) > limit or (tx - rx > setups and not run.has_churn()),
              f"{what}: {tx} sent, {rx} received (+{share:.1%} first packets before a session); requests unexplained by session setups {loss:+.2%}",
              f"{what}: {tx} sent, {rx} received; {unexplained} requests neither received nor explained by session setups ({loss:+.2%}, tolerance {limit:.0%})",
              sent=tx, received=rx, session_setups=setups, unexplained=unexplained)


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
    q = sorted(n["byType"].get("TOPICQUERY/v5", {}).get("rxMsgs", 0) for n in run.oh)
    if not q or q[len(q) // 2] == 0:
        return verdict("not-exercised", "no TOPICQUERY traffic")
    med, mx = q[len(q) // 2], q[-1]
    return pf(mx > tol.load_multiple * med, f"busiest registrar at {mx / med:.1f}× the median ({mx} vs {med} queries)",
              f"busiest registrar at {mx / med:.1f}× the median, above {tol.load_multiple}×", max=mx, median=med, limit=tol.load_multiple)


def h1_no_drops(run, tol):
    if run.host:
        drops = 0
        for h in run.host:
            if h:
                drops += max(0, h[-1].get("udp_rcvbuf_errors", 0) - h[0].get("udp_rcvbuf_errors", 0))
        return pf(drops, f"no UDP receive-buffer drops on {len(run.host)} hosts", f"{drops} UDP receive-buffer drops", drops=drops, hosts=len(run.host))
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
    n, got = len(run.assign), len(run.traces)
    if n == 0:
        return verdict("needs-data", "no assignments")
    return pf(got < n, f"{got} of {n} traces collected", f"{n - got} of {n} traces missing", assigned=n, traces=got)


def h3_hosts_under_load(run, tol):
    if not run.host:
        return verdict("needs-data", "no host metrics")
    cpu = [ns["cpu_pct"] for h in run.host for s in h for ns in s.get("nodes", [])]
    rss = [ns["rss_mb"] for h in run.host for s in h for ns in s.get("nodes", [])]
    lowmem = min((s["mem_avail_mb"] for h in run.host for s in h if s.get("mem_avail_mb")), default=None)
    if not cpu:
        return verdict("needs-data", "no node samples in host metrics")
    hot = max(cpu)
    fails = hot > tol.cpu_pct or (lowmem is not None and lowmem < tol.mem_avail_mb)
    mem = f"host memory never below {lowmem:.0f} MB" if lowmem is not None else "host memory not reported"
    return pf(fails, f"node CPU peak {hot:.0f}%, RSS peak {max(rss):.0f} MB, {mem}",
              f"host saturated: CPU peak {hot:.0f}% or free memory {lowmem} MB", cpu_peak=hot, rss_peak=max(rss), mem_avail_min=lowmem)


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
        ("E3", "renewal accounting: renewals sent equal renewals received", e3_renewal_accounting),
        ("W1", "bounded wait: no admission after the attempt timeout", w1_bounded_wait),
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
    ]),
]


def markdown(results, run):
    L = ["| check | assertion | status | evidence |", "|---|---|---|---|"]
    for group, checks in results.items():
        for code_, desc, v in checks:
            L.append(f"| **{code_}** {group} | {desc} | {v['status']} | {v['summary']} |")
    counts = Counter(v["status"] for checks in results.values() for _, _, v in checks)
    head = ", ".join(f"{n} {s}" for s, n in sorted(counts.items(), key=lambda x: -x[1]))
    return f"{head}.\n\n" + "\n".join(L) + "\n"


def main():
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
    tol = ap.parse_args()
    rd = os.path.abspath(tol.run_dir)
    run = Run(rd)
    results = {}
    for group, checks in CHECKS:
        results[group] = []
        for code_, desc, fn in checks:
            try:
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
