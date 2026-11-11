"""Harbor installed agent for Crush (#388).

Installs a pinned crush binary into the task container, runs one
``crush run`` per trial headless, and lands the session DB +
telemetry under the mounted logs dir so the host-side ingester
(``crush eval ingest``) turns each trial into a RunRecord.

Agent kwargs (--agent-kwarg name=value):

  binary-path   host path to the pinned CGO_ENABLED=0 linux crush
                binary (required — the build under test)
  data-dir      container data dir (default
                /logs/agent/crush-data — the /logs/agent mount is
                what lands the session DB + telemetry host-side;
                moving it outside breaks copy-back)
  env           comma-separated host env-var names forwarded into
                the run — provider credentials ride this allowlist,
                the same named-not-values discipline as the eval
                manifest's env admission
  eval-flags    comma-separated manifest flag names the child
                reports resolved (CRUSH_EVAL_FLAGS)
  harness-revision  git sha the binary must report — install()
                runs ``crush --version`` in-container and refuses a
                mismatched build, since env.crush_sha records what
                the experiment *declared*, not what actually ran

The built-in ``--config`` / ``config`` agent option carries a
rendered .crush.json (path or inline JSON): model pins, the arm's
options under test, providers block with $VAR api-key refs.
Provider credentials themselves pass through ``--agent-env`` or the
``env`` allowlist — values never enter the manifest.

Usage:

  harbor run --dataset terminal-bench@2.1 \
      --agent-import-path eval.harbor.crush_agent:Crush \
      --agent-kwarg binary-path=./crush-linux-amd64 \
      --config ./arm-control.crush.json \
      --agent-env LLM_API_KEY --model hyper/deepseek-v4-pro-0813
"""

import json
import os
import shlex
import tempfile
from pathlib import Path
from typing import override

from pydantic import Field

from harbor.agents.capabilities import AgentCapabilities
from harbor.agents.installed.base import (
    BaseInstalledAgent,
    with_prompt_template,
)
from harbor.agents.options import InstalledAgentOptions
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

_INSTALL_DIR = "/installed-agent"
_DEFAULT_DATA_DIR = "/logs/agent/crush-data"


class CrushOptions(InstalledAgentOptions):
    binary_path: str = Field(
        description="Host path to the pinned linux crush binary.",
    )
    data_dir: str = Field(
        default=_DEFAULT_DATA_DIR,
        description="Container data dir for the session DB + telemetry.",
    )
    env: str | None = Field(
        default=None,
        description="Comma-separated host env-var names to forward.",
    )
    eval_flags: str | None = Field(
        default=None,
        description="Manifest flag names the child reports resolved.",
    )
    harness_revision: str | None = Field(
        default=None,
        description="Git sha the binary's --version must report.",
    )


class Crush(BaseInstalledAgent):
    """Wraps the crush CLI as a harbor installed agent."""

    capabilities = AgentCapabilities(native_config=True)
    options_model = CrushOptions

    @staticmethod
    @override
    def name() -> str:
        return "crush"

    @property
    def _opts(self) -> CrushOptions:
        return self.options

    def _forward_env(self) -> dict[str, str]:
        """Declared credential passthrough — extra_env (--agent-env)
        is harbor's own declared channel; the env kwarg allowlists
        additional host names. Names only; values never enter
        config or the manifest."""
        out = {}
        # --agent-env rides the same allowlist discipline as env:
        # harness-controlled names can't enter through either
        # channel — CRUSH_EVAL_FLAGS smuggled via extra_env would
        # pollute resolved_options.
        for name, val in (self.extra_env or {}).items():
            if name.startswith(("CRUSH_", "EVAL_")):
                raise ValueError(
                    f"extra_env {name}: harness-controlled names are not passable"
                )
            out[name] = val
        allow = self._opts.env or ""
        for name in (n.strip() for n in allow.split(",")):
            if not name:
                continue
            if name.startswith(("CRUSH_", "EVAL_")):
                raise ValueError(
                    f"env {name}: harness-controlled names are not passable"
                )
            if name in os.environ:
                out[name] = os.environ[name]
        return out

    @override
    async def install(self, environment: BaseEnvironment) -> None:
        binary = Path(self._opts.binary_path)
        if not binary.is_file():
            raise ValueError(f"crush binary not found at {binary}")
        await environment.exec(
            f"mkdir -p {_INSTALL_DIR} {shlex.quote(self._opts.data_dir)}"
        )
        await environment.upload_file(binary, f"{_INSTALL_DIR}/crush")
        if self._config is not None:
            if isinstance(self._config, Path):
                await environment.upload_file(
                    self._config, f"{_INSTALL_DIR}/crush.json"
                )
            else:
                with tempfile.TemporaryDirectory() as tmp:
                    local = Path(tmp) / "crush.json"
                    local.write_text(json.dumps(self._config))
                    await environment.upload_file(local, f"{_INSTALL_DIR}/crush.json")
        await environment.exec(f"chmod +x {_INSTALL_DIR}/crush")
        if self._opts.harness_revision:
            res = await environment.exec(f"{_INSTALL_DIR}/crush --version")
            out = getattr(res, "stdout", None) or getattr(res, "output", None) or str(res)
            if self._opts.harness_revision not in out:
                raise ValueError(
                    f"crush --version reports {out.strip()!r} — expected "
                    f"harness_revision {self._opts.harness_revision}; the "
                    f"binary at {binary} is not the declared build"
                )

    @override
    @with_prompt_template
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        env = self._forward_env()
        env["CRUSH_EVAL_TELEMETRY"] = f"{self._opts.data_dir}/telemetry.json"
        if self._opts.eval_flags:
            env["CRUSH_EVAL_FLAGS"] = self._opts.eval_flags
        config_stage = (
            f"cp {_INSTALL_DIR}/crush.json \"$PWD/.crush.json\"; "
            if self._config is not None
            else ""
        )
        model_flag = (
            f"--model {shlex.quote(self.model_name)} " if self.model_name else ""
        )
        await self.exec_as_agent(
            environment,
            command=(
                "set -o pipefail; "
                f"mkdir -p {shlex.quote(self._opts.data_dir)}; "
                f"{config_stage}"
                f"{_INSTALL_DIR}/crush run -q -y "
                f"--data-dir {shlex.quote(self._opts.data_dir)} "
                f"{model_flag}"
                f"{shlex.quote(instruction)} "
                "2>&1 | tee /logs/agent/crush.out"
            ),
            env=env,
        )

    @override
    def populate_context_post_run(self, context: AgentContext) -> None:
        """Fill the trial's usage from the child's telemetry blob —
        the same CRUSH_EVAL_TELEMETRY file the in-house driver reads,
        landed host-side by the /logs/agent mount. The session DB is
        already beside it for the ingester."""
        tel_path = Path(self.logs_dir) / "crush-data" / "telemetry.json"
        try:
            tel = json.loads(tel_path.read_text())
        except (OSError, json.JSONDecodeError):
            return
        tokens = tel.get("tokens") or {}
        context.n_input_tokens = tokens.get("input", 0) + tokens.get(
            "cache_write", 0
        )
        context.n_cache_tokens = tokens.get("cache_read", 0)
        context.n_output_tokens = tokens.get("output", 0)
        context.metadata = {
            "session_id": tel.get("session_id"),
            "param_version": tel.get("param_version"),
            "resolved_options": tel.get("resolved_options"),
            "model": tel.get("model"),
        }
