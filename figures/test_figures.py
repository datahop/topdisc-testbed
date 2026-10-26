#!/usr/bin/env python3
"""The bootnode's load in the sweep table and cp_06 when the registrars carry none."""
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


if __name__ == "__main__":
    unittest.main()
