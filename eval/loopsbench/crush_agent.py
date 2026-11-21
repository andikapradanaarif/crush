"""Crush agent adapter for LoopsBench (#446, R2.2/N2).

Loads via the harness's custom-agent import path, same shape as the
harbor adapter (eval/harbor/crush_agent.py): installs the pinned
linux crush binary into the task container, materializes the arm
config as .crush.json, runs one `crush run` over the unit
instruction, and lands the session DB + telemetry under a container
dir the harness copies to agent-logs/.

  loopsbench run \
      --agent-import-path eval.loopsbench.crush_agent:CrushAgent \
      -k binary_path=./crush-linux-amd64 \
      -k config=./arm-deepseek.crush.json \
      -k harness_revision=c153a99463 \
      -k api_key_env=OPENCODE_API_KEY \
      -k base_url=https://opencode.ai/zen/go/v1 \
      -k max_tokens=8000000 \
      -t task_dist_sys_raft_go ...  --n-attempts 1

Kwargs mirror the harbor contract: binary_path (required),
harness_revision (sha the binary must report), config (path or
inline JSON), api_key_env (host env whose value is staged via an
env file — never through send_keys, which logs commands),
max_tokens, eval_flags, data_dir.
"""

from __future__ import annotations

import json
import os
import re
import shlex
import tempfile
from pathlib import Path

from loopsbench.agents.base_agent import AgentResult, BaseAgent
from loopsbench.terminal.docker_compose_manager import DockerComposeManager
from loopsbench.terminal.tmux_session import TmuxSession

_CRUSH_BIN = "/usr/local/bin/crush"
_CRUSH_CONFIG = "/tmp/loopsbench-crush.json"
_CRUSH_ENV = "/tmp/loopsbench-agent.env"
_DEFAULT_DATA_DIR = "/tmp/crush-data"
_CRUSH_OUT = "/tmp/crush.out"
_WORKSPACE = "/workspace"


class CrushAgent(BaseAgent):
    """Run the pinned crush binary inside the task container."""

    def __init__(self, **kwargs):
        super().__init__(**kwargs)
        self._binary_path = kwargs.get("binary_path")
        if not self._binary_path or not Path(self._binary_path).is_file():
            raise ValueError("binary_path kwarg must point to the pinned crush binary")
        self._harness_revision = kwargs.get("harness_revision")
        self._config = self._load_config(kwargs.get("config"))
        self._api_key_env = kwargs.get("api_key_env", "OPENCODE_API_KEY")
        self._api_key = kwargs.get("api_key") or os.environ.get(self._api_key_env, "")
        self._data_dir = kwargs.get("data_dir", _DEFAULT_DATA_DIR)
        self._max_tokens = kwargs.get("max_tokens")
        self._eval_flags = kwargs.get("eval_flags")
        self._extra_env: dict[str, str] = dict(kwargs.get("env") or {})

    @staticmethod
    def name() -> str:
        return "crush"

    @staticmethod
    def _load_config(config):
        if config is None:
            return None
        if isinstance(config, dict):
            return config
        text = str(config)
        if os.path.exists(text):
            return json.loads(Path(text).read_text())
        return json.loads(text)

    def install(self, session: TmuxSession, logging_dir: Path | None = None) -> None:
        DockerComposeManager.copy_to_container(
            container=session._container,
            paths=[self._binary_path],
            container_dir="/usr/local/bin",
            container_filename="crush",
        )
        session.send_keys(f"chmod +x {_CRUSH_BIN} Enter", block=True, max_timeout_sec=10.0)
        if self._harness_revision:
            session.send_keys(f"{_CRUSH_BIN} --version Enter", block=True, max_timeout_sec=10.0)
            out = session.capture_pane()
            if self._harness_revision not in out:
                raise ValueError(
                    f"crush binary at {self._binary_path} does not report "
                    f"declared harness_revision {self._harness_revision}"
                )
        if self._config is not None:
            with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as tmp:
                json.dump(self._config, tmp)
                tmp_path = tmp.name
            DockerComposeManager.copy_to_container(
                container=session._container,
                paths=[tmp_path],
                container_dir="/tmp",
                container_filename="loopsbench-crush.json",
            )
        if self._api_key or self._extra_env:
            env_lines = [f"{self._api_key_env}={shlex.quote(self._api_key)}"] if self._api_key else []
            env_lines += [f"{k}={shlex.quote(v)}" for k, v in self._extra_env.items()]
            with tempfile.NamedTemporaryFile("w", delete=False) as tmp:
                tmp.write("\n".join(env_lines) + "\n")
                env_path = tmp.name
            DockerComposeManager.copy_to_container(
                container=session._container,
                paths=[env_path],
                container_dir="/tmp",
                container_filename="loopsbench-agent.env",
            )

    def prepare(self, session: TmuxSession, logging_dir: Path | None = None) -> None:
        exports = [
            f"set -a; . {_CRUSH_ENV}; set +a" if (self._api_key or self._extra_env) else ":",
            f"export CRUSH_EVAL_TELEMETRY={self._data_dir}/telemetry.json",
        ]
        if self._eval_flags:
            exports.append(f"export CRUSH_EVAL_FLAGS={shlex.quote(self._eval_flags)}")
        if self._max_tokens:
            exports.append(f"export CRUSH_EVAL_MAX_TOKENS={int(self._max_tokens)}")
        for cmd in exports:
            session.send_keys(f"{cmd} Enter", block=True, max_timeout_sec=10.0)

    def perform_task(
        self,
        instruction: str,
        session: TmuxSession,
        logging_dir: Path | None = None,
        timeout_sec: float | None = None,
    ) -> AgentResult:
        config_stage = f"cp {_CRUSH_CONFIG} \"$PWD/.crush.json\"; " if self._config is not None else ""
        cmd = (
            "set -o pipefail; "
            f"cd {_WORKSPACE}; "
            f"mkdir -p {shlex.quote(self._data_dir)}; "
            f"{config_stage}"
            f"{_CRUSH_BIN} run -q -y "
            f"--data-dir {shlex.quote(self._data_dir)} "
            f"{shlex.quote(instruction)} "
            f"2>&1 | tee {_CRUSH_OUT}"
        )
        session.send_keys(f"{cmd} Enter", block=True, max_timeout_sec=timeout_sec or 86400.0)
        return self._read_telemetry(session)

    def _read_telemetry(self, session: TmuxSession) -> AgentResult:
        """Best-effort token readout — the durable copy lands via
        get_trajectory_paths for post-hoc accounting."""
        session.send_keys(
            f"cat {self._data_dir}/telemetry.json Enter",
            block=True,
            max_timeout_sec=10.0,
        )
        pane = session.capture_pane(capture_entire=True)
        match = re.search(r"\{.*\"tokens\".*\}", pane, re.DOTALL)
        if not match:
            return AgentResult()
        try:
            tokens = (json.loads(match.group(0)).get("tokens") or {})
        except json.JSONDecodeError:
            return AgentResult()
        return AgentResult(
            total_input_tokens=tokens.get("input", 0) + tokens.get("cache_write", 0) + tokens.get("cache_read", 0),
            total_output_tokens=tokens.get("output", 0),
        )

    def get_trajectory_paths(self) -> list[str]:
        return [self._data_dir, _CRUSH_OUT]
