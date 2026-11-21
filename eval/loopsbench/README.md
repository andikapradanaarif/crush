# LoopsBench adapter — N2 baseline (#446)

`crush_agent.py` plugs crush into Microsoft's LoopsBench harness via
its custom-agent import path — the same contract as
`eval/harbor/crush_agent.py` transplanted onto
`loopsbench.agents.base_agent.BaseAgent` + `TmuxSession`.

Unlike terminal-bench, LoopsBench owns the long-horizon mechanics:
the watcher container releases unit tests along the ready frontier
and keeps completed units live as regression obligations. The
adapter only has to drive the agent and land telemetry — resolve
rate and regression events come from the harness's own result
files, so "regression obligations wired into scoring" is native,
not something we build.

## Setup

```sh
# LoopsBench harness (separate checkout — not vendored):
git clone https://github.com/microsoft/loopsbench /tmp/loopsbench
cd /tmp/loopsbench && uv sync   # or: pip install -e .

# Pinned crush binary — same build the dev-split runs use:
cd /Users/andikaarif/Documents/p/playground/crush
git checkout c153a99463
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o crush-linux-amd64 .
```

## N2 subset (24 tasks)

Two per LoopsBench category, median-and-harder unit counts, bounded
at ≤300 units (the repo-scale `*_seg0*` tasks with 4–8K units are
excluded — they would dominate the budget on a first baseline).
13 hard / 9 medium / 2 easy, 363 units total — difficulty-skewed on
purpose: the published best resolve rate is 25%, so the subset must
contain the tasks that produce it.

```
task_graph_processing              task_datastructs_java
task_calcite_query_optimization    task_mysql_sql_labs
task_dist_sys_raft_go              task_distributed_fs_chfs
task_boogie_dafny                  task_frama_c
task_raylib_textures_hard          task_monogame_math2d_medium
task_sklearn_ml_toolchain          task_deep_learning_three_assignments
task_ns3_network_simulation        task_dcqcn_rdma_evolution
task_mlir_circuit_ir               task_compiler_cmm_mips
task_afl_fuzzing                   task_angr_concolic_execution
task_perses_program_reduction      task_evosuite_test_generation
task_gem5_architecture_simulation  task_os_c_fs_labs
task_spring_course_mgmt_fullstack  task_django_seg01
```

## Run (owner-executed)

```sh
cd /tmp/loopsbench
source ~/.config/crush/secrets.env
PYTHONPATH=/Users/andikaarif/Documents/p/playground/crush loopsbench run \
    --agent-import-path eval.loopsbench.crush_agent:CrushAgent \
    -k binary_path=/Users/andikaarif/Documents/p/playground/crush/crush-linux-amd64 \
    -k config=/Users/andikaarif/Documents/p/playground/crush/arm-deepseek.crush.json \
    -k harness_revision=c153a99463 \
    -k api_key_env=OPENCODE_API_KEY \
    -k base_url=https://opencode.ai/zen/go/v1 \
    -k max_tokens=8000000 \
    -t task_graph_processing -t task_datastructs_java \
    -t task_calcite_query_optimization -t task_mysql_sql_labs \
    -t task_dist_sys_raft_go -t task_distributed_fs_chfs \
    -t task_boogie_dafny -t task_frama_c \
    -t task_raylib_textures_hard -t task_monogame_math2d_medium \
    -t task_sklearn_ml_toolchain -t task_deep_learning_three_assignments \
    -t task_ns3_network_simulation -t task_dcqcn_rdma_evolution \
    -t task_mlir_circuit_ir -t task_compiler_cmm_mips \
    -t task_afl_fuzzing -t task_angr_concolic_execution \
    -t task_perses_program_reduction -t task_evosuite_test_generation \
    -t task_gem5_architecture_simulation -t task_os_c_fs_labs \
    -t task_spring_course_mgmt_fullstack -t task_django_seg01 \
    --n-attempts 1 --n-concurrent 2
```

Model parity: `base_url` declares the opencode route for the
network policy; the model/temperature pins live in
`arm-deepseek.crush.json` exactly as the dev-split runs.

## Reading the result

- **Resolve rate + regression events**: the harness's per-run
  result files (resolve non-inferior, regression events ≤ baseline
  on later improvement reads — N2 §4).
- **Telemetry**: `agent-logs/crush-data/telemetry.json` per trial —
  the same `CRUSH_EVAL_TELEMETRY` blob the in-house driver and
  harbor ingester read; token fields also surface on the trial's
  `AgentResult`.
- **Cost caveat**: LoopsBench tasks are longer than TB2.1 trials —
  no measured $/task exists yet. The first run IS the cost
  characterization; record spend with the baseline.

Cluster bootstrap (#407) applies at analysis time.
