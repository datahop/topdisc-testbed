#!/usr/bin/env python3
"""checks.py against golden output of its original, load-everything version.

The synthetic runs below exercise every check with offenders spread over
several nodes (more than the ten the evidence keeps), malformed traces and a
simnet run. testdata/checks-<kind>.json was written by checks.py before its
loader reduced the traces; regenerate it only from a checks.py known to be
correct:

    python3 figures/test_checks.py --golden <checks.py>
"""
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import bootnodes  # noqa: E402
import checks  # noqa: E402

GOLDEN = os.path.join(HERE, "testdata")
KINDS = ("real", "malformed", "simnet")

T0 = 1_800_000_000_000  # search start
TA, TB = "aa" * 32, "bb" * 32
UNKNOWN = "ff" * 32


def nid(i):
    return f"{i + 1:064x}"


def read(p):
    with open(p) as f:
        return json.load(f)


def write(rd, name, obj, **kw):
    p = os.path.join(rd, name)
    os.makedirs(os.path.dirname(p), exist_ok=True)
    with open(p, "w") as f:
        json.dump(obj, f, **kw)


def metrics(results, coverage, extra=None):
    m = {"findCountByTopic": [{"topic": 0, "counts": [1, 2, 3]}], "perTopic": [],
         "registrationCoverage": {"byTopic": coverage},
         "results": results, "topicIds": {TA: 0, TB: 1}}
    m.update(extra or {})
    return m


def result(i, topic, **kw):
    r = {"nodeIdx": i, "nodeId": nid(i)[:16], "topic": topic, "target": 3,
         "uniqueFoundIds": [nid(j)[:16] for j in range(3)], "uniqueFoundAtMs": [10, 20, 30],
         "searchStats": {"queries": 4, "byHop": [{"hop": 1, "n": [1, 2]}]}, "foundExtra": 0}
    r.update(kw)
    return r


def oh(n, legacy=()):
    out = []
    for i in range(n):
        bt = {} if i in legacy else {
            "REGTOPIC/v5": {"txMsgs": 100 + i, "rxMsgs": 95 + i, "txBytes": 1},
            "TOPICQUERY/v5": {"txMsgs": 50, "rxMsgs": 40 + 30 * (i == 2), "rxBytes": 7},
            "TOPICNODES/v5": {"txMsgs": 30 if i == 1 else 70},
            "FINDNODE/v5": {"txMsgs": 20, "rxMsgs": 20},
            "NODES/v5": {"txMsgs": 20},
            "PING/v5": {"txMsgs": 10, "rxMsgs": 10},
            "REGTOPIC(renewal)/v5": {"txMsgs": 12, "rxMsgs": 11},
            "SESSION-SETUP(undecryptable)": {"rxMsgs": 6},
        }
        out.append({"idx": i, "id": nid(i), "byType": bt, "ops": [{"msg": "PING/v5", "nodes": 3}], "txPkts": 9})
    return out


def admission(at, adv, topic=TA, **kw):
    ev = {"atMs": at, "registrar": nid(0), "advertiser": adv, "topic": topic, "recordIp": "8.8.8.8", "fromIp": "8.8.8.8",
          "requiredMs": 10_000, "waitedMs": 12_000, "quoteMs": 0, "admitted": True, "renewal": False, "held": 2, "topicHeld": 1}
    ev.update(kw)
    return ev


def build_real(rd, malformed=False):
    """Six traced nodes (5 legacy), a seventh assigned without a trace, node 4 churned."""
    topic = {"ad_lifetime_ms": 60_000, "ad_cache_size": 10, "reg_attempt_timeout_ms": 90_000, "reg_bucket_size": 3,
             "topic_nodes_limit": 4, "aux_nodes_limit": 2}
    topics = {0: TA, 1: TA, 2: TA, 3: TB, 4: TB, 5: TA, 6: TB}
    for i in range(7):
        a = {"idx": i, "legacy": i == 5, "topic": topic, "phases": {"search_at_ms": T0, "start_at_ms": T0 - 600_000},
             "search": {"target_count": 3, "request_timeout_ms": 5_000}}
        if i == 4:
            a["churn"] = [{"DownAt": 10, "UpAt": 1_000}]
        write(rd, f"assignments/node{i}.json", a, indent=1)

    def samples(i, n, held, idle_buckets):
        out = []
        for k in range(n):
            rb = [{"registered": 0, "waiting": 0, "standby": 2} if b < idle_buckets else {"registered": 2, "waiting": 1, "standby": 0}
                  for b in range(3)]
            out.append({"at_ms": T0 - 200_000 + k * 60_000, "wire": {}, "cache_held": held[k % len(held)], "cache_cap": 10,
                        "cache_by_topic": {TA: 1}, "reg_buckets": rb})
        return out

    def adm(i):
        if i == 3:
            return None
        evs = []
        for k in range(6):  # early
            evs.append(admission(T0 + k, nid(k % 3), waitedMs=0, requiredMs=10_000))
        for k in range(4):  # into a full cache, and one turned away
            evs.append(admission(T0 + 100 + k, nid(1), held=10 + k))
        evs.append(admission(T0 + 200, nid(2), held=11, admitted=False))
        evs.append(admission(T0 + 201, nid(2), held=10, renewal=True))
        for k in range(5):  # record address other than the source
            evs.append(admission(T0 + 300 + k, nid(3), recordIp="8.8.4.4", fromIp=f"9.9.9.{k}"))
        for k in range(4):  # undercut quotes, interleaved advertisers
            evs.append(admission(T0 + 1_000 * k, nid(k % 2), quoteMs=50_000 - 40_000 * (k // 2), admitted=False))
        evs.append(admission(T0 + 9_000, nid(0), topic=TB, quoteMs=7_000, admitted=False))
        return evs

    for i in range(6):
        t = {"idx": i, "id": nid(i), "legacy": i == 5, "inbound": 3, "session": {"n": 1}, "topic_load": {TA: 3}}
        if i != 5:
            t["topic"] = topics[i]
            t["topics"] = [topics[i]]
        t["samples"] = samples(i, 12 if i < 4 else 3, [3, 12, 4] if i == 2 else [5, 11] if i == 3 else [4], 2 if i in (0, 1) else 0)
        if i == 4:
            t["samples"] = [dict(s, at_ms=T0 + 300_000) for s in t["samples"]]
        if i == 0:
            t["reg_complete_ms"] = T0 - 300_000
        elif i == 1:
            t["reg_complete_ms"] = 0
        t["search_at_ms"] = T0
        if i != 4:
            t["admissions"] = adm(i)
        t["wait"] = {TA: {"admittedMs": [1_000, 92_000, 96_000 + i, 120_000, 99_000.5]}, TB: {"admittedMs": None}} if i < 4 else {}
        t["ads_final"] = {
            0: {TA: [nid(1), nid(1), nid(0), nid(5), UNKNOWN], TB: [nid(3)]},
            1: {TA: [nid(2)], TB: [nid(0)]},
            2: {TA: [nid(0), nid(1)], TB: [nid(4)]},
            3: {TB: [nid(4), nid(3)]},
        }.get(i, {})
        if i == 0:
            t["ads_first_seen_ms"] = {TA: {nid(1): T0, nid(2): T0 + 1}, TB: {nid(3): T0}}
        elif i == 2:
            t["ads_first_seen_ms"] = {}
        stop = t["samples"][-1]["at_ms"]
        t["ads_final_expiry_ms"] = {TA: {nid(1): stop + 200_000, nid(2): stop + 10_000}} if i == 0 else {}
        t["ads_snapshot_ms"] = stop if i == 0 else 0
        t["stale_ad_max_ms"] = 7_000 if i == 0 else 100
        if i < 3:
            t["reg_buckets_final"] = [
                {"dist": 256, "registered": 0, "waiting": 0, "standby": 2, "target": 3,
                 "nodes": [{"id": nid(1), "ip": "8.8.8.1", "state": "standby"}, {"id": nid(2), "ip": "8.8.8.2", "state": "standby"}]},
                {"dist": 255, "registered": 3 + i, "waiting": 1, "standby": 0, "target": 3,
                 "nodes": [{"id": nid(3), "ip": "10.0.0.1"}, {"id": nid(4)}]},
                {"dist": 254, "registered": 1, "waiting": 0, "standby": 0},
            ]
        else:
            t["reg_buckets_final"] = None
        t["ops"] = [
            {"msg": "REGTOPIC/v5", "nodeIds": [nid(5), nid(1), nid(5)], "nodes": 3, "opid": 1},
            {"msg": "TOPICQUERY/v5", "nodeIds": [nid(2)], "nodes": 1, "opid": 2},
            {"msg": "PING/v5", "nodes": 1, "opid": 3},
        ]
        if i == 5:
            t["lookups"] = [{"start_ms": T0, "latency_ms": 100, "results": 0, "hit_target": False}]
            t["conn"] = None
        else:
            found = [{"id": nid(j), "at_ms": 10 * j} for j in (1, 3, 5)] + [{"id": nid(4), "at_ms": 200_000}, {"id": UNKNOWN, "at_ms": 7}]
            search = {"queries": 4, "received": 30 + i, "contacted": 9}
            if i == 1:
                search.update(maxTopicPerReply=5, maxAuxPerReply=1)
            t["lookups"] = [
                {"start_ms": T0 + 1_000, "latency_ms": 9_000, "first_ms": 5, "results": 6, "hit_target": True, "found": found, "queries": 4, "search": search},
                {"start_ms": T0 + 2_000, "latency_ms": 100, "results": 1, "hit_target": True, "found": found[:1], "search": None},
            ]
            t["conn"] = {"start_ms": T0 + 50, "found": [{"id": nid(2), "at_ms": 3}, "not-a-record"]} if i == 1 else None
        if malformed:
            if i == 1:
                del t["samples"][3]["cache_held"]
                t["ops"][0]["nodeIds"] = None
                t["admissions"][2].pop("held")
                t["lookups"][0]["found"][1] = {"at_ms": 5}
            if i == 2:
                t["wait"] = {TA: None}
                t["ads_first_seen_ms"] = {TA: None}
                t["samples"][0]["reg_buckets"] = [{"registered": 0}]
                t["admissions"][0].pop("waitedMs")
            if i == 3:
                t["lookups"][0]["found"] = 1  # fails L1-L3 and L6, not L4 or L5
        write(rd, f"traces/node{i}.json", t)
    res = [result(i, {TA: 0, TB: 1}[topics[i]]) for i in range(6)]
    cov = {"0": {"byRegistrant": {nid(0): 4, nid(1): 2, nid(2): 3, nid(4): 2, UNKNOWN: 1}}, "1": {"byRegistrant": {nid(3): 1}}}
    write(rd, "metrics.json", metrics(res, cov), indent=2)
    write(rd, "oh.json", oh(6, legacy=(5,)))
    write(rd, "series.json", {"samples": [{"cacheHeld": 99, "cacheCap": 1}]})
    host = [{"at_ms": T0 + k, "load1": 1.0, "mem_avail_mb": 900 - k, "udp_rcvbuf_errors": 3 * k,
             "nodes": [{"idx": j, "cpu_pct": 10.0 * k, "rss_mb": 20.5, "fds": 7} for j in range(3)]} for k in range(3)]
    write(rd, "hostmetrics/host0.json", host)
    write(rd, "hostmetrics/host1.json", [])
    with open(os.path.join(rd, "run.log"), "w") as f:
        f.write("starting\nPARAMS: scenario.topic.ad_lifetime=1m0s scenario.topic.reg_bucket_size=3\n")


def build_simnet(rd):
    n = 8
    stop = T0 + 500_000
    nodes = {str(i): {TA if i % 2 else TB: {nid(j): stop + 10_000 * j for j in range(n) if j != i}} for i in range(n)}
    nodes["0"][TB][nid(0)] = stop + 5_000_000  # its own ad, expiring beyond a lifetime
    write(rd, "ads.json", {"at_ms": stop, "stale_max_ms": 2_000, "nodes": nodes}, indent=2)
    write(rd, "buckets.json", {str(i): [{"dist": 256, "registered": i, "waiting": 1, "standby": 1 if i else 0, "target": 0}] for i in range(n)})
    write(rd, "admissions.json", {str(i): [admission(T0 + k, nid((i + k) % n), waitedMs=k * 3_000, quoteMs=40_000 - 9_000 * k, held=4998 + k)
                                           for k in range(4)] for i in range(n)})
    write(rd, "series.json", {"samples": [{"cacheHeld": 30, "cacheCap": 40000}, {"cacheHeld": 41, "cacheCap": 40000}],
                              "waitTime": [{"topic": TA, "admittedMs": [5, 1_400_000]}, {"topic": TB, "admittedMs": None}]})
    write(rd, "churn.json", {"nodes": {"3": [{"DownAt": 10, "UpAt": 20}]}})
    res = [result(i, i % 2, foundExtra=i // 4) for i in range(n)]
    cov = {"0": {"byRegistrant": {nid(i): 7 for i in range(0, n, 2)}}, "1": {"byRegistrant": {nid(i): 4 for i in range(1, n, 2)}}}
    write(rd, "metrics.json", metrics(res, cov, {"deadResults": {"byTopic": {"0": [1, 2]}}}), indent=2)
    write(rd, "oh.json", oh(n))
    with open(os.path.join(rd, "run.log"), "w") as f:
        f.write("[buf] sent=10 dropped=0\n[buf] sent=20 dropped=4\n")


def build(kind, rd):
    os.makedirs(rd)
    if kind == "simnet":
        build_simnet(rd)
    else:
        build_real(rd, malformed=kind == "malformed")


def run_checks(script, rd, out):
    p = subprocess.run([sys.executable, script, rd, "--out", out], capture_output=True, text=True)
    return p.returncode, read(out)


class TestChecks(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()

    def tearDown(self):
        self.tmp.cleanup()

    def test_golden(self):
        for kind in KINDS:
            with self.subTest(kind=kind):
                rd = os.path.join(self.tmp.name, f"synthetic-{kind}")
                build(kind, rd)
                rc, got = run_checks(os.path.join(HERE, "checks.py"), rd, os.path.join(self.tmp.name, f"{kind}.json"))
                want = read(os.path.join(GOLDEN, f"checks-{kind}.json"))
                self.assertEqual(rc, want.pop("exit"))
                self.assertEqual(got, want)

    def test_malformed_lookups(self):
        """A lookup entry that is not an object fails the checks that read lookups, as before."""
        rd = os.path.join(self.tmp.name, "synthetic-bad-lookups")
        build("real", rd)
        p = os.path.join(rd, "traces/node1.json")
        t = read(p)
        t["lookups"] = ["bad", ["x"]]
        write(rd, "traces/node1.json", t)
        _, out = run_checks(os.path.join(HERE, "checks.py"), rd, os.path.join(self.tmp.name, "bad.json"))
        c = {x["id"]: x for x in out["checks"]}
        for k in ("L1", "L2", "L3", "L4", "L5"):
            self.assertEqual((c[k]["status"], c[k]["summary"]), ("error", "AttributeError: 'str' object has no attribute 'get'"), k)

    def test_stream_metrics(self):
        for kind in KINDS:
            with self.subTest(kind=kind):
                rd = os.path.join(self.tmp.name, f"synthetic-{kind}")
                build(kind, rd)
                p = os.path.join(rd, "metrics.json")
                want = checks.slim_metrics(read(p))
                self.assertEqual(checks.stream_metrics(p), want)
                # a compact file is not streamable and loads whole
                for kw in ({}, {"indent": 2, "separators": (",", ":")}):
                    write(rd, "metrics.json", want, **kw)
                    self.assertRaises(checks.Unstreamable, checks.stream_metrics, p)
                    self.assertEqual(checks.load_metrics(p), want)

    def test_stream_metrics_layout(self):
        """Valid JSON that departs from the indented layout loads whole, not partly."""
        m = metrics([result(i, i % 2) for i in range(3)], {"0": {"byRegistrant": {nid(0): 1}}})
        text = json.dumps(m, indent=2)
        found_extra = '      "foundExtra": 0\n    },\n'
        assert text.count(found_extra) == 2
        variants = {
            "element after a closing brace":
                text.replace(found_extra, '      "foundExtra": 0\n    }, {"nodeIdx": 7, "nodeId": "x", "topic": 1},\n', 1),
            "key after a closing bracket":
                text.replace('"uniqueFoundAtMs": [\n        10,\n        20,\n        30\n      ],',
                             '"uniqueFoundAtMs": [\n        10,\n        20,\n        30\n      ], "foundExtra": 5,', 1),
            "key escaping a nested line":
                text.replace('\n        20,\n', '\n        20], "foundExtra": 6, "x": [\n', 1),
            "two keys on a line":
                text.replace('"target": 3,', '"target": 3, "foundExtra": 8,', 1),
            "data after the object": text + "\n{}\n",
        }
        with tempfile.TemporaryDirectory() as tmp:
            p = os.path.join(tmp, "metrics.json")
            for what, v in variants.items():
                with self.subTest(what):
                    self.assertNotEqual(v, text)
                    with open(p, "w") as f:
                        f.write(v)
                    self.assertRaises(checks.Unstreamable, checks.stream_metrics, p)
                    try:
                        want = checks.slim_metrics(json.loads(v))
                    except ValueError as e:
                        self.assertRaises(type(e), checks.load_metrics, p)
                    else:
                        self.assertEqual(checks.load_metrics(p), want)


class TestCheckLogic(unittest.TestCase):
    """Churn timing, topics and skips the goldens' fixture does not separate."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.rd = os.path.join(self.tmp.name, "synthetic-staggered")
        build_real(self.rd)
        # search starts staggered by two minutes, topics named as the coordinator does
        for i in range(7):
            p = os.path.join(self.rd, f"assignments/node{i}.json")
            a = read(p)
            a["phases"].update(search_at_ms=T0 + i * 120_000, stop_at_ms=T0 + 3_600_000)
            a["topics"] = ["topic-0" if i in (0, 1, 2, 5) else "topic-1"]
            if i == 2:
                a["churn"] = [{"DownAt": 0, "UpAt": 3_500}]  # scheduled down; its samples end 255 s into its search
            if i == 6:
                a["churn"] = [{"DownAt": 10, "UpAt": 1_000}]  # no trace: the schedule decides
            write(self.rd, f"assignments/node{i}.json", a, indent=1)
        m = read(os.path.join(self.rd, "metrics.json"))
        m["results"] = [r for r in m["results"] if r["nodeIdx"] != 5]  # no search result
        write(self.rd, "metrics.json", m, indent=2)
        self.resample(5, T0 - 600_000, T0 + 3_600_000)  # up through its search

    def resample(self, i, start, stop, gap=None, sessions=None):
        """Samples every 30 s from start to stop, none inside gap; sessions as recorded."""
        p = os.path.join(self.rd, f"traces/node{i}.json")
        t = read(p)
        s0 = t["samples"][0]
        t["samples"] = [dict(s0, at_ms=at) for at in range(start, stop + 1, 30_000) if not gap or not gap[0] < at < gap[1]]
        if sessions:
            t["sessions"] = [{"start_ms": a, "end_ms": b} for a, b in sessions]
        write(self.rd, f"traces/node{i}.json", t)

    def churn(self, i, events):
        a = read(os.path.join(self.rd, f"assignments/node{i}.json"))
        a["churn"] = [{"DownAt": d, "UpAt": u} for d, u in events]
        write(self.rd, f"assignments/node{i}.json", a, indent=1)

    def tearDown(self):
        self.tmp.cleanup()

    def plain(self):
        self.make_plain()
        out = os.path.join(self.tmp.name, "plain.json")
        run_checks(os.path.join(HERE, "checks.py"), self.rd, out)
        return {c["id"]: c for c in read(out)["checks"]}

    def make_plain(self):
        for d in ("assignments", "traces"):
            for f in os.listdir(os.path.join(self.rd, d)):
                t = read(os.path.join(self.rd, d, f))
                t["legacy"] = True
                write(self.rd, f"{d}/{f}", t)

    def h1(self, *hosts, rate=None):
        """H1 over the given host sample lists, replacing the fixture's hostmetrics."""
        d = os.path.join(self.rd, "hostmetrics")
        for f in os.listdir(d):
            os.remove(os.path.join(d, f))
        for i, h in enumerate(hosts):
            write(self.rd, f"hostmetrics/host{i}.json", h)
        if rate:
            log = os.path.join(self.rd, "run.log")
            txt = open(log).read().replace("PARAMS: ", f"PARAMS: testbed.wan.rate_kbps={rate} ")
            with open(log, "w") as f:
                f.write(txt)
        return checks.h1_no_drops(checks.Run(self.rd), checks.argparser().parse_args([self.rd]))

    @staticmethod
    def smp(k, netns=None, sampled=500, arp=None, **kw):
        s = {"at_ms": T0 + k, "udp_rcvbuf_errors": 0, "nodes": [], **kw}
        if netns is not None:
            s.update(netns=netns, netns_sampled=sampled, netns_expected=500)
        if arp is not None:
            s["arp"] = {"entries": 9, "unresolved_discards": arp, "table_fulls": 0, "forced_gc_runs": 0}
        return s

    def test_h1_shared_cases(self):
        """The host drop rules pkg/coordinator's console summary applies too (figures/testdata/h1-cases.json)."""
        for case in read(os.path.join(GOLDEN, "h1-cases.json"))["cases"]:
            with self.subTest(case["name"]):
                d = os.path.join(self.rd, "hostmetrics")
                for f in os.listdir(d):
                    os.remove(os.path.join(d, f))
                for i, h in enumerate(case["hosts"]):
                    write(self.rd, f"hostmetrics/host{i}.json", h)
                cats, _, gaps = checks.host_drops(checks.Run(self.rd))
                self.assertEqual((cats, gaps), (case["by_category"], case["coverage_gaps"]))
                self.assertEqual(list(cats), list(case["by_category"]))  # and in the same order

    def test_h1_netns_categories(self):
        """Namespace, ARP and ENA counters split H1's drops by where they happened."""
        host = [self.smp(k, netns={"qdisc_drops": 40 * k, "ns_udp_rcvbuf_errors": 7 * k, "ns_udp_sndbuf_errors": 41 * k, "veth_rx_dropped": 3 * k},
                         arp=5 * k, netns_max_backlog_bytes=250_000 * k, udp_rcvbuf_errors=2 * k,
                         ena_iface="ens5", ena_pps_allowance_exceeded=100 + 11 * k, ena_conntrack_allowance_exceeded=1) for k in range(2)]
        v = self.h1(host, rate=1000)
        self.assertEqual(v["status"], "fail")
        self.assertEqual(v["evidence"]["by_category"],
                         {"netem": 40, "namespace UDP receive buffer": 7, "ARP": 5, "ENA allowance": 11, "host UDP receive buffer": 2})
        self.assertEqual(v["evidence"]["drops"], 65)
        self.assertEqual((v["evidence"]["veth_dropped"], v["evidence"]["ns_udp_sndbuf_errors"]), (3, 41))
        self.assertEqual(v["evidence"]["ena_allowance"], {"pps": 11, "bw_in": 0, "bw_out": 0, "conntrack": 0, "linklocal": 0})
        self.assertEqual(v["evidence"]["coverage_gaps"], {})
        self.assertIn("ENA allowance 11 (pps 11), host UDP receive buffer 2", v["summary"])
        self.assertIn("longest egress queue 250 kB (2.0 s at 1000 kbit/s)", v["summary"])

    def test_h1_clean_run_passes(self):
        host = [self.smp(k, netns={"qdisc_drops": 3}, arp=1, ena_iface="ens5", ena_pps_allowance_exceeded=8) for k in range(3)]
        v = self.h1(host, host)
        self.assertEqual(v["status"], "pass", v["summary"])

    def test_h1_failed_read_is_not_a_reset(self):
        """Totals 0, 100, then a sample that read no namespace: the 100 stands, and the gap is reported."""
        host = [self.smp(0, netns={"qdisc_drops": 0}), self.smp(1, netns={"qdisc_drops": 100}),
                self.smp(2, netns={"qdisc_drops": 0}, sampled=0)]
        v = self.h1(host)
        self.assertEqual((v["status"], v["evidence"]["by_category"]["netem"]), ("fail", 100))
        self.assertEqual(v["evidence"]["coverage_gaps"], {"netns": 1})
        self.assertIn("not measured throughout: netns on 1 hosts", v["summary"])
        # a sample that read only some namespaces is no comparison point either
        host[2] = self.smp(2, netns={"qdisc_drops": 60}, sampled=499)
        self.assertEqual(self.h1(host)["evidence"]["by_category"]["netem"], 100)

    def test_h1_unmeasured_is_not_a_pass(self):
        """No namespace read in any sample: no evidence of zero drops."""
        v = self.h1([self.smp(k, netns={}, sampled=0) for k in range(3)])
        self.assertEqual(v["status"], "needs-data")
        self.assertTrue(v["summary"].startswith("insufficient coverage, not measured throughout: netns on 1 hosts"), v["summary"])
        # nor is a host that sent no samples, beside one that did
        v = self.h1([self.smp(k, netns={}) for k in range(3)], [])
        self.assertEqual((v["status"], v["evidence"]["coverage_gaps"]), ("needs-data", {"netns": 1}))

    def test_h1_unread_wan_monitor(self):
        """Namespaces expected but never entered (a non-root monitor): needs-data, not a pass."""
        v = self.h1([{"at_ms": T0 + k, "udp_rcvbuf_errors": 0, "nodes": [], "netns_expected": 2} for k in range(3)])
        self.assertEqual((v["status"], v["evidence"]["coverage_gaps"]), ("needs-data", {"netns": 1}))
        self.assertIn("not measured throughout: netns on 1 hosts", v["summary"])

    def test_h1_counter_reset(self):
        """A fall is a reset: 50 -> 80 -> 5 -> 9 rose 30 + 5 + 4."""
        v = self.h1([self.smp(k, netns={"qdisc_drops": q}) for k, q in enumerate((50, 80, 5, 9))])
        self.assertEqual(v["evidence"]["by_category"]["netem"], 39)

    def test_h1_ena(self):
        """A growing allowance counter fails H1, a flat one passes; a host that read ENA late still counts."""
        for grow, status in ((0, "pass"), (3, "fail")):
            v = self.h1([{"at_ms": T0 + k, "udp_rcvbuf_errors": 0, "ena_iface": "ens5", "ena_bw_out_allowance_exceeded": 7 + grow * k, "nodes": []}
                         for k in range(2)])
            self.assertEqual(v["status"], status)
            self.assertEqual(v["evidence"]["by_category"], {"ENA allowance": grow, "host UDP receive buffer": 0})
        self.assertEqual(v["summary"], "3 packet drops: ENA allowance 3 (bw_out 3), host UDP receive buffer 0")
        late = [self.smp(0, ena_error="no default route"), self.smp(1, ena_iface="ens5", ena_pps_allowance_exceeded=10),
                self.smp(2, ena_iface="ens5", ena_pps_allowance_exceeded=20)]
        v = self.h1(late)
        self.assertEqual((v["status"], v["evidence"]["ena_allowance"]["pps"], v["evidence"]["coverage_gaps"]), ("fail", 10, {"ENA": 1}))
        # still failing at the end: ENA unavailable, so not a pass
        v = self.h1([self.smp(k, ena_error="ens5: ethtool 0x1d: no such device") for k in range(2)])
        self.assertEqual((v["status"], v["evidence"]["coverage_gaps"]), ("needs-data", {"ENA": 1}))
        # failures that stopped without a reading: the host has no ENA device
        v = self.h1([self.smp(0, ena_error="no default route", arp=0), self.smp(1, arp=0)])
        self.assertEqual((v["status"], v["evidence"]["by_category"]), ("pass", {"ARP": 0, "host UDP receive buffer": 0}))

    def test_h1_arp_without_netns(self):
        """ARP drops count on hosts without namespace counters."""
        v = self.h1([self.smp(k, arp=100 * k) for k in range(2)])
        self.assertEqual((v["status"], v["evidence"]["by_category"]), ("fail", {"ARP": 100, "host UDP receive buffer": 0}))

    def test_h1_old_runs_unchanged(self):
        """A run whose samples predate the namespace counters keeps H1's root-namespace verdict."""
        run = checks.Run(self.rd)
        v = checks.h1_no_drops(run, checks.argparser().parse_args([self.rd]))
        self.assertEqual((v["status"], v["summary"], v["evidence"]), ("fail", "6 UDP receive-buffer drops", {"drops": 6, "hosts": 2}))

    def test_churn_from_node0_search_start(self):
        run = checks.Run(self.rd)
        self.assertEqual(run.offline_intervals(4), [(T0 + 10_000, T0 + 1_000_000)])
        self.assertEqual(run.offline_intervals(6), [(T0 + 10_000, T0 + 1_000_000)])
        self.assertEqual(run.down_since(6, T0 + 600_000), 590_000)
        self.assertEqual(run.online_ms(6, T0 + 720_000, T0 + 3_600_000), 2_600_000)
        self.assertEqual(run.churn_base_note, "node 0")

    def test_churn_base_without_node0(self):
        """Without node 0's assignment no other node stands in: each node's own search start."""
        os.remove(os.path.join(self.rd, "assignments/node0.json"))
        run = checks.Run(self.rd)
        self.assertEqual(run.offline_intervals(6), [(T0 + 730_000, T0 + 1_720_000)])
        self.assertEqual(run.churn_base_note, "per node (node 0 assignment missing)")
        tol = checks.argparser().parse_args([self.rd])
        for check in (checks.k1_departed_leave, checks.l3_no_expired):
            self.assertEqual(check(run, tol)["evidence"]["churn_base"], "per node (node 0 assignment missing)")

    def test_liveness_from_samples(self):
        """A kill that did not happen clears the node; a real sample gap condemns it."""
        self.resample(1, T0 - 600_000, T0 + 3_600_000)  # scheduled down below, never stopped sampling
        self.churn(1, [(100, 3_500)])
        self.resample(4, T0 - 600_000, T0 + 3_600_000, gap=(T0 + 50_000, T0 + 900_000))
        # an arrival: the process it ran before the search left no record
        self.resample(3, T0 + 1_200_000, T0 + 2_000_000, sessions=[(T0 + 1_195_000, T0 + 2_010_000), (T0 + 2_400_000, T0 + 2_410_000)])
        self.churn(3, [(0, 1_195), (2_010, 2_400), (2_410, 3_600)])
        t = read(os.path.join(self.rd, "traces/node0.json"))
        t["lookups"][0]["found"].append({"id": nid(1), "at_ms": 300_000})  # scheduled down 201 s by then
        write(self.rd, "traces/node0.json", t)
        run = checks.Run(self.rd)
        self.assertIsNone(run.down_since(1, T0 + 460_000))
        self.assertEqual(run.scheduled_down_since(1, T0 + 460_000), 360_000)
        self.assertEqual(run.down_since(4, T0 + 460_000), 460_000 - 65_000)  # the window of the last sample before the gap
        self.assertIsNone(run.down_since(4, T0 + 920_000))
        self.assertEqual(run.down_since(4, T0 - 700_000), 0)  # before its start
        self.assertEqual(run.online_ms(4, T0, T0 + 1_000_000), 1_000_000 - (900_000 - 35_000 - 30_000 - 35_000))
        self.assertEqual(run.down_since(3, T0 + 240_000), 240_000)  # before its first record: the schedule
        self.assertIsNone(run.down_since(3, T0 + 2_405_000))  # a session too short to sample
        self.assertEqual(run.down_since(3, T0 + 2_500_000), 90_000)
        self.assertEqual(run.online_ms(3, T0 + 360_000, T0 + 3_600_000), (1_980_000 + 35_000) - (1_200_000 - 35_000) + 10_000)
        tol = checks.argparser().parse_args([self.rd])
        for check in (checks.k1_departed_leave, checks.l3_no_expired):
            with self.subTest(check.__name__):
                v = check(run, tol)
                j = {o[1] for o in v["evidence"]["offenders"]}
                self.assertIn(4, j)
                self.assertNotIn(1, j)
                self.assertGreater(v["evidence"]["schedule_down_trace_up"], 0)
                self.assertIn("schedule said down, trace shows up: ", v["summary"])

    def test_topic_from_assignment(self):
        run = checks.Run(self.rd)
        self.assertEqual(run.topic_of[5], TA)
        self.assertEqual(run.topic_of[6], TB)
        tol = checks.argparser().parse_args([self.rd])
        before = checks.l1_right_topic(run, tol)["evidence"]
        del run.topic_of[3]
        after = checks.l1_right_topic(run, tol)["evidence"]
        from3 = sum(1 for _, _, rid, _ in checks.lookup_results(run) if rid == nid(3))
        self.assertGreater(from3, 0)
        self.assertEqual(after["unknown_ids"], before["unknown_ids"] + from3)
        self.assertFalse(any(j == 3 for _, j in after["offenders"]))

    def test_trace_without_run_coverage(self):
        """A trace holding only a restart after the stop counts as no trace."""
        p = os.path.join(self.rd, "traces/node3.json")
        t = read(p)
        t["samples"], t["sessions"] = [], [{"start_ms": T0 + 3_602_000, "end_ms": T0 + 3_603_000}]
        write(self.rd, "traces/node3.json", t)
        self.churn(3, [(100, 3_600)])  # the schedule alone would make its ads and results stale
        t = read(os.path.join(self.rd, "traces/node0.json"))
        t["lookups"][0]["found"].append({"id": nid(3), "at_ms": 2_000_000})
        write(self.rd, "traces/node0.json", t)
        run = checks.Run(self.rd)
        self.assertEqual(run.uncovered, {3})
        self.assertEqual(run.down_since(3, T0 + 2_001_000), 1_901_000)  # liveness: the schedule, as without a trace
        tol = checks.argparser().parse_args([self.rd])
        held = sum(ads.count(nid(3)) for _, st in run.nodes() for ads in (st.get("ads_final") or {}).values())
        found = sum(1 for _, _, rid, _ in checks.lookup_results(run) if rid == nid(3))
        self.assertGreater(held, 0)
        self.assertGreater(found, 0)
        for check, n in ((checks.k1_departed_leave, held), (checks.l3_no_expired, found), (checks.l1_right_topic, found)):
            with self.subTest(check.__name__):
                v = check(run, tol)
                self.assertNotIn(3, {o[1] for o in v["evidence"]["offenders"]})
                self.assertGreaterEqual(v["evidence"]["unknown_ids"], n)
        self.assertGreater(checks.c3_only_registrants(run, tol)["evidence"]["unknown_ids"], 0)
        h2 = checks.h2_complete_collection(run, tol)
        self.assertEqual((h2["status"], h2["evidence"]["no_coverage"], h2["evidence"]["no_coverage_nodes"]), ("fail", 1, [3]))
        self.assertEqual(h2["summary"], "1 of 7 traces missing, 1 with no run coverage (only a restart after the stop)")

    def test_k1_at_ad_snapshot(self):
        """K1 asks whether the advertiser was up when the registrar's ad set was taken."""
        p = os.path.join(self.rd, "traces/node2.json")
        t = read(p)
        t["ads_snapshot_ms"] = T0 + 2_000_000
        write(self.rd, "traces/node2.json", t)
        p = os.path.join(self.rd, "traces/node4.json")
        t = read(p)
        t["sessions"] = [{"start_ms": T0 + 1_990_000, "end_ms": T0 + 2_010_000}]  # up at node 2's snapshot, long gone at its last sample
        write(self.rd, "traces/node4.json", t)
        run = checks.Run(self.rd)
        v = checks.k1_departed_leave(run, checks.argparser().parse_args([self.rd]))
        self.assertNotIn((2, 4), {(o[0], o[1]) for o in v["evidence"]["offenders"]})
        self.assertIn((3, 4), {(o[0], o[1]) for o in v["evidence"]["offenders"]})

    def test_i2_every_searcher_skipped(self):
        self.make_plain()
        run = checks.Run(self.rd)
        v = checks.i2_base_discovery(run, checks.argparser().parse_args([self.rd, "--min-search-online-ms", str(10**9)]))
        self.assertEqual((v["status"], v["summary"]), ("not-exercised", "6 searchers skipped: online under 16667 min of their search"))
        self.assertEqual(v["evidence"], {"skipped": 6, "churn_base": "node 0"})

    def test_wait_slack(self):
        """W1 allows the registrar's wall-clock wait a share past the attempt timeout."""
        timeout = 1_350_000
        for i in range(7):
            a = read(os.path.join(self.rd, f"assignments/node{i}.json"))
            a["topic"]["reg_attempt_timeout_ms"] = timeout
            write(self.rd, f"assignments/node{i}.json", a, indent=1)
        for i in range(6):
            t = read(os.path.join(self.rd, f"traces/node{i}.json"))
            t["wait"] = {TA: {"admittedMs": [timeout * 101 // 100]}} if i == 0 else {TB: {"admittedMs": [timeout * 105 // 100]}} if i == 1 else {}
            write(self.rd, f"traces/node{i}.json", t)
        run = checks.Run(self.rd)
        v = checks.w1_bounded_wait(run, checks.argparser().parse_args([self.rd]))
        self.assertEqual(v["status"], "fail")
        self.assertEqual(v["evidence"]["offenders"], [(1, TB[:8], timeout * 105 // 100)])
        self.assertEqual((v["evidence"]["slack"], v["evidence"]["limit_ms"]), (0.02, timeout * 102 // 100 + 5_000))
        out = os.path.join(self.tmp.name, "w1.json")
        run_checks(os.path.join(HERE, "checks.py"), self.rd, out)
        w1 = next(c for c in read(out)["checks"] if c["id"] == "W1")
        self.assertEqual(w1["assertion"], "bounded wait: every admission within 2 % of the attempt timeout")

    def test_g1_against_assignments(self):
        """G1 counts what the assignments carry, not what the traces claim."""
        tol = checks.argparser().parse_args([self.rd])
        self.assertEqual(checks.g1_ghosts_pinned(checks.Run(self.rd), tol)["status"], "not-exercised")
        for i in range(7):
            a = read(os.path.join(self.rd, f"assignments/node{i}.json"))
            a["ghosts"] = ["enr:-ghost"] * (i % 3 + 1)
            write(self.rd, f"assignments/node{i}.json", a, indent=1)
            p = os.path.join(self.rd, f"traces/node{i}.json")
            if os.path.exists(p):
                t = read(p)
                t.update(ghosts_assigned=i % 3 + 1, ghosts_in_table=i % 3 + 1, table_size=20)
                write(self.rd, f"traces/node{i}.json", t)
        v = checks.g1_ghosts_pinned(checks.Run(self.rd), tol)
        self.assertEqual(v["status"], "pass", v["summary"])

        t = read(os.path.join(self.rd, "traces/node3.json"))
        for k in checks.GHOST_FIELDS:
            del t[k]
        write(self.rd, "traces/node3.json", t)
        t = read(os.path.join(self.rd, "traces/node4.json"))
        t["ghosts_in_table"] = 1  # holds one of its two
        write(self.rd, "traces/node4.json", t)
        t = read(os.path.join(self.rd, "traces/node5.json"))
        t.update(ghosts_assigned=1, ghosts_in_table=1)  # claims fewer than assigned
        write(self.rd, "traces/node5.json", t)
        v = checks.g1_ghosts_pinned(checks.Run(self.rd), tol)
        self.assertEqual(v["status"], "fail")
        self.assertEqual(v["evidence"]["missing"], [3])
        self.assertEqual(v["evidence"]["offenders"], [(4, 1, 2, 2), (5, 1, 1, 3)])

    def test_a3_topdisc_registrars(self):
        """A3 takes the load distribution over the TopDisc nodes only and tells absent traffic from concentrated traffic."""
        run = checks.Run(self.rd)
        tol = checks.argparser().parse_args([self.rd])

        def a3(rx, legacy=()):
            run.oh = [{"idx": i, "byType": {"TOPICQUERY/v5": {"rxMsgs": v}} if i not in legacy else {}} for i, v in enumerate(rx)]
            run.legacy = {i: i in legacy for i in range(len(rx))}
            return checks.a3_load_bound(run, tol)

        v = a3([40, 70, 0, 0, 0, 0], legacy=(2, 3, 4, 5))  # low share: the plain majority does not set the median
        self.assertEqual((v["status"], v["evidence"]["median"], v["evidence"]["max"]), ("pass", 70, 70))
        v = a3([0, 0, 0, 90])
        self.assertEqual((v["status"], v["evidence"]["busy"]), ("fail", 1))
        self.assertEqual(a3([0, 0, 0], legacy=(2,))["status"], "not-exercised")

    def test_a3_bootnode(self):
        """A3 leaves the bootnode out of the distribution and reports its load apart."""
        run = checks.Run(self.rd)
        tol = checks.argparser().parse_args([self.rd])
        run.oh = [{"idx": i, "byType": {"TOPICQUERY/v5": {"rxMsgs": v}}} for i, v in enumerate([1220, 100, 120, 90, 300, 110])]
        run.legacy = {}
        run.bootnodes = {0}
        v = checks.a3_load_bound(run, tol)
        self.assertEqual((v["status"], v["evidence"]["median"], v["evidence"]["max"]), ("pass", 110, 300))
        self.assertEqual(v["evidence"]["bootnodes"], [{"idx": 0, "rx": 1220, "multiple": 11.1}])
        self.assertIn("bootnode 0 left out: 1220 queries, 11.1× the median", v["summary"])
        for i in range(1, 6):  # only the bootnode served: not exercised, the bootnode still reported
            run.oh[i]["byType"]["TOPICQUERY/v5"]["rxMsgs"] = 0
        v = checks.a3_load_bound(run, tol)
        self.assertEqual((v["status"], v["summary"]), ("not-exercised", "no TOPICQUERY traffic; bootnode 0 left out: 1220 queries"))
        self.assertEqual(v["evidence"]["bootnodes"], [{"idx": 0, "rx": 1220, "multiple": None}])
        run.legacy = {0: True}  # a plain bootnode is no registrar: nothing to report
        run.oh[0]["byType"] = {}
        self.assertEqual(checks.a3_load_bound(run, tol)["evidence"]["bootnodes"], [])

    def test_bootnodes_from_assignments(self):
        """The bootnode is every node's first bootnode; the bootstrap sample after it is not."""
        def url(i):
            return f"enode://{'ab' * 64}@10.0.0.{1 + i // 4}:{30300 + i}"
        asg = {i: {"idx": i, "ip": f"10.0.0.{1 + i // 4}", "port": 30300 + i, "bootnodes": [url(3), url(i - 1)] if i != 3 else []}
               for i in range(8)}
        self.assertEqual(bootnodes.from_assignments(asg), {3})
        self.assertEqual(bootnodes.from_assignments({}), {0})

    def test_l3_plain_searchers_skipped(self):
        """A plain searcher's stale result is a routing-table entry, not an ad: L3 scores TopDisc searchers only."""
        for i in range(5):
            t = read(os.path.join(self.rd, f"traces/node{i}.json"))
            for lk in t["lookups"]:
                lk["found"] = [r for r in lk["found"] if r["id"] != nid(4)]
            write(self.rd, f"traces/node{i}.json", t)
        t = read(os.path.join(self.rd, "traces/node5.json"))
        t["lookups"][0]["found"] = [{"id": nid(4), "at_ms": 500_000}, {"id": nid(1), "at_ms": 600_000}]
        write(self.rd, "traces/node5.json", t)
        tol = checks.argparser().parse_args([self.rd])
        v = checks.l3_no_expired(checks.Run(self.rd), tol)
        self.assertEqual(v["status"], "pass")
        self.assertEqual(v["evidence"]["plain_results_skipped"], 2)
        self.assertIn("; 2 results of plain searchers skipped", v["summary"])
        run = checks.Run(self.rd)
        run.legacy[5] = False  # the same results from a TopDisc searcher are stale
        v = checks.l3_no_expired(run, tol)
        self.assertEqual(v["status"], "fail")
        self.assertEqual([o[:2] for o in v["evidence"]["offenders"]], [(5, 4), (5, 1)])
        self.assertNotIn("plain_results_skipped", v["evidence"])
        run.legacy = {i: True for i in run.legacy}
        self.assertEqual(checks.l3_no_expired(run, tol)["summary"], "no TopDisc searchers in the run")

    def test_plain_run(self):
        c = self.plain()
        self.assertEqual((c["L3"]["status"], c["L3"]["summary"]), ("not-exercised", "every node runs plain discv5"))
        self.assertEqual(c["I2"]["status"], "fail")
        self.assertIn(", 3 online under 5 min of their search skipped", c["I2"]["summary"])  # nodes 2, 3, 4: samples end early in their search
        self.assertEqual(c["I2"]["evidence"]["offenders"], [5])


def golden(script):
    with tempfile.TemporaryDirectory() as tmp:
        for kind in KINDS:
            rd = os.path.join(tmp, f"synthetic-{kind}")
            build(kind, rd)
            rc, out = run_checks(script, rd, os.path.join(tmp, f"{kind}.json"))
            out["exit"] = rc
            write(GOLDEN, f"checks-{kind}.json", out, indent=1)
            print(kind, rc, dict(sorted((c["id"], c["status"]) for c in out["checks"])))


if __name__ == "__main__":
    if sys.argv[1:2] == ["--golden"]:
        golden(sys.argv[2])
    else:
        unittest.main()
