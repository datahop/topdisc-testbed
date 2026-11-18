#!/usr/bin/env python3
"""The bootnode's load in the sweep table and cp_06 when the registrars carry none;
the sweep's all-providers scoring rows and misses list."""
import json
import os
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import deploy_sweep  # noqa: E402
import pairfigs  # noqa: E402


class TestBootnodeLoad(unittest.TestCase):
    def test_sweep_boot_cell(self):
        self.assertEqual(deploy_sweep.boot_cell({"boot_load": 1200, "load_p50": 100}), "1,200 (12.0×)")
        self.assertEqual(deploy_sweep.boot_cell({"boot_load": 1200, "load_p50": 0}), "1,200")
        self.assertEqual(deploy_sweep.boot_cell({"boot_load": 1200, "load_p50": float("nan")}), "1,200")
        self.assertEqual(deploy_sweep.boot_cell({"load_p50": 100}), "–")

    def test_serving_load_bootnode_only(self):
        with tempfile.TemporaryDirectory() as tmp:
            r = pairfigs.Run("a", tmp, 0)
            r.traffic, r.boot = True, {0}
            r.oh = [{"idx": i, "byType": {"PING/v5": {"rxMsgs": 1200 if i == 0 else 0}, "PONG/v5": {"txBytes": 5e6 if i == 0 else 0}}}
                    for i in range(4)]
            ctx = pairfigs.Ctx([r], tmp)
            figs = []
            ctx.save = lambda fig, stem, caption: figs.append(fig)
            pairfigs.serving_load(ctx)
            labels = [[ln.get_label() for ln in ax.get_lines()] for ax in figs[0].axes]
            self.assertEqual(labels, [["a: bootnode 0, left out, 1,200.0"], ["a: bootnode 0, left out, 5.0 MB"]])



class TestScoreAllProviders(unittest.TestCase):
    def build(self, rd):
        ids = [str(i + 1) * 64 for i in range(4)]
        os.makedirs(os.path.join(rd, "assignments"))

        def write(name, obj):
            with open(os.path.join(rd, name), "w") as f:
                json.dump(obj, f)
        for i in range(4):
            write(f"assignments/node{i}.json", {"idx": i, "legacy": i >= 2, "topics": ["topic-0"], "search": {"model": "continuous", "score_all_providers": True}})
        write("nodes.json", [{"idx": i, "id": ids[i], "first_capable_ms": -1} for i in range(4)])
        write("oh.json", [{"idx": i, "id": ids[i], "byType": {"FINDNODE/v5": {"txMsgs": 100 * (i + 1)}}} for i in range(4)])
        res = [
            {"nodeIdx": 0, "topic": 0, "target": 3, "uniqueFoundAtMs": [10, 20, 30], "uniqueFoundSrc": ["topic", "walk", "topic+walk"], "lookupWalkQueries": [5], "missed": 0},
            {"nodeIdx": 1, "topic": 0, "target": 3, "uniqueFoundAtMs": [10, 20], "uniqueFoundSrc": ["topic", "walk"], "lookupWalkQueries": [7],
             "missed": 1, "missedIds": [ids[3][:16]]},
            {"nodeIdx": 2, "legacy": True, "topic": 0, "target": 3, "uniqueFoundAtMs": [0, 5, 6], "missed": 0},
            {"nodeIdx": 3, "legacy": True, "topic": 0, "target": 3, "uniqueFoundAtMs": [0, 5, 6], "missed": 0},
        ]
        write("metrics.json", {"results": res, "perTopic": [{"topic": 0, "target": 3}]})
        with open(os.path.join(rd, "run.log"), "w") as f:
            f.write("PARAMS: scenario.population.nodes=4\n")

    def test_stats_and_misses(self):
        with tempfile.TemporaryDirectory() as tmp:
            self.build(tmp)
            r = deploy_sweep.load_run(50, tmp, False)
            td, pl = r["classes"]["all"]["types"]["TopDisc"], r["classes"]["all"]["types"]["plain"]
            self.assertEqual((td["full_share"], pl["full_share"]), (0.5, 1.0))
            self.assertAlmostEqual(td["recall_min"], 2 / 3)
            self.assertEqual((td["src_topic"], td["src_walk"], td["src_both"]), (0.4, 0.4, 0.2))
            self.assertEqual((pl["src_topic"], pl["src_walk"], pl["src_both"]), (0.0, 1.0, 0.0))
            self.assertEqual((td["findnode_p50"], pl["findnode_p50"]), (150, 350))
            self.assertEqual(r["misses"], [("1 (TopDisc)", 0, "2/3", "3 (plain)")])
            with open(deploy_sweep.write_md([r], tmp)) as f:
                md = f.read()
            self.assertIn("Every searcher, TopDisc or plain, is scored against every member", md)
            self.assertIn("| 1 (TopDisc) | 0 | 2/3 | 3 (plain) |", md)
            self.assertIn("| providers found via topic only / walk only / both | 40.0 / 40.0 / 20.0 % | 0.0 / 100.0 / 0.0 % |", md)
            self.assertIn("| found 100 % of topic, share of searchers | 50.0 % | 100.0 % |", md)
            self.assertIn("| FINDNODE sent per node, whole run, median | 150 | 350 |", md)
            self.assertIn("per-search counters (registrars contacted per search, plain FINDNODE per walk) are unreliable in continuous runs", md)

if __name__ == "__main__":
    unittest.main()
