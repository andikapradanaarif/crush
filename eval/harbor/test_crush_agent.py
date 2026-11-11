"""Smoke tests for the crush harbor agent — harbor isn't installed
in the dev env, so these exercise the parts that don't need it:
telemetry parsing in populate_context_post_run and the env
allowlist discipline. The full install/run path is verified by the
harbor spike, not here."""

import json
import sys
import tempfile
import unittest
from pathlib import Path

try:
    from eval.harbor import crush_agent
except ImportError:
    try:
        import crush_agent
    except ImportError:
        crush_agent = None


@unittest.skipIf(crush_agent is None, "harbor package not installed")
class TestPopulateContext(unittest.TestCase):
    def test_telemetry_fills_usage(self):
        logs = Path(tempfile.mkdtemp()) / "crush-data"
        logs.mkdir(parents=True)
        (logs / "telemetry.json").write_text(json.dumps({
            "session_id": "s-1",
            "param_version": "pv1-x",
            "model": "llm/test-model",
            "resolved_options": {"a": 1},
            "tokens": {"input": 100, "output": 40, "cache_read": 10, "cache_write": 5},
        }))
        agent = crush_agent.Crush.__new__(crush_agent.Crush)
        agent.logs_dir = str(logs.parent)
        agent.model_name = "llm/test-model"
        ctx = crush_agent.AgentContext()
        agent.populate_context_post_run(ctx)
        self.assertEqual(ctx.n_input_tokens, 105)  # input + cache_write
        self.assertEqual(ctx.n_cache_tokens, 10)
        self.assertEqual(ctx.n_output_tokens, 40)
        self.assertEqual(ctx.metadata["session_id"], "s-1")

    def test_missing_telemetry_leaves_context_empty(self):
        agent = crush_agent.Crush.__new__(crush_agent.Crush)
        agent.logs_dir = tempfile.mkdtemp()
        ctx = crush_agent.AgentContext()
        agent.populate_context_post_run(ctx)
        self.assertTrue(ctx.is_empty())

    def test_env_allowlist_rejects_harness_names(self):
        import os

        os.environ["CRUSH_SECRET"] = "x"
        agent = crush_agent.Crush.__new__(crush_agent.Crush)
        agent._extra_env = {}
        opts = crush_agent.CrushOptions(binary_path="/x", env="CRUSH_SECRET")
        agent.options = opts
        with self.assertRaises(ValueError):
            agent._forward_env()

    def test_extra_env_rejects_harness_names(self):
        # --agent-env can't smuggle CRUSH_EVAL_FLAGS past the
        # allowlist — the kwarg isn't the only forwarding channel.
        agent = crush_agent.Crush.__new__(crush_agent.Crush)
        agent._extra_env = {"CRUSH_EVAL_FLAGS": "fold-turns"}
        agent.options = crush_agent.CrushOptions(binary_path="/x")
        with self.assertRaises(ValueError):
            agent._forward_env()


if __name__ == "__main__":
    sys.path.insert(0, str(Path(__file__).parent.parent.parent))
    unittest.main()
