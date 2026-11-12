# eval/harbor — Crush as a Harbor installed agent (#388)

`crush_agent.py` wraps the crush CLI as a `BaseInstalledAgent` so
Harbor can drive it against containerized benchmarks — Terminal-Bench
2.1 (the frozen dev split, `eval/manifests/tb21-dev.json`) and
SWE-bench Pro V2 (the OOD gate, #226). One Harbor installed agent
covers both datasets; the paired-experiment machinery downstream —
records, compare, the gate, session-DB analysis — is unchanged.

## The shape

```
harbor run (host)
  └── task container
        /installed-agent/crush          pinned CGO_ENABLED=0 binary
        /installed-agent/crush.json     rendered .crush.json (arm config)
        crush run -q -y --data-dir /logs/agent/crush-data <instruction>
              │   CRUSH_EVAL_TELEMETRY=/logs/agent/crush-data/telemetry.json
              ▼
        /logs/agent/crush-data/         mounted → <trial>/agent/crush-data/
              ├── crush.db              session DB (VACUUM INTO'd by ingest)
              └── telemetry.json        steps, tokens, resolved_options, …

crush eval ingest (host)
  └── reads <trial>/result.json + <trial>/agent/crush-data/*
      → RunRecord per (task, arm, run_index) → eval/results/<exp>/
      → report-<inv>.json alarm snapshot
```

The `/logs/agent` dir is mounted from the host's `trial_dir/agent` —
pointing `--data-dir` inside it is what makes the copy-back free. The
ingester's `check_detail` carries the verifier reward; the instruction
the agent sees never contains verifier content (the task format keeps
tests out of the agent's view).

## Run a job

```sh
# Build the linux binary the arm under test runs (CGO_ENABLED=0 —
# the container is the isolation boundary, not the sandbox).
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o crush-linux-amd64 .

PYTHONPATH=$PWD harbor run --dataset <tasks-dir-or-registry> \
    --agent eval.harbor.crush_agent:Crush \
    --agent-kwarg binary_path="$PWD/crush-linux-amd64" \
    --agent-kwarg harness_revision=<short sha of the build> \
    --agent-kwarg config="$PWD/arm-control.crush.json" \
    --agent-kwarg env=LLM_API_KEY,LLM_BASE_URL \
    --environment-build-timeout-multiplier 4 \
    --agent-timeout-multiplier 2 \
    --verifier-timeout-multiplier 4 \
    -n 2 -k 1
```

Do **not** pass harbor's `--model`: model selection lives in the arm
config's top-level `models` block (`large` + `small`). Passing
`--model` makes crush derive the small model from the provider's
catalog default — silently switching auxiliary calls (compaction,
titles) to a different model than the arm under test. The `models`
block also carries the sampling pins (`temperature`, `top_p`) —
declare them there, not in the provider's `models[]` entry.

The multipliers are field-calibrated: task images that need >600s to
build, agent runs that exceed per-task timeouts on slow free-tier
models, and verifiers that re-run heavy workloads (e.g. retraining)
all show up under load.

- `binary_path` (required): the pinned binary — its build SHA is the
  experiment's `harbor.harness_revision`. Kwarg names are the
  pydantic field names (underscores), and `harness_revision` is the
  short sha substring `crush --version` reports (e.g. `0075c6e21b0d`),
  not the full git sha.
- `config` agent kwarg: path or inline JSON — the rendered `.crush.json`
  (model pins, the arm's `options` under test, a `providers` block
  referencing `$LLM_API_KEY`/`$LLM_BASE_URL`). One job per arm.
  (Harbor's top-level `--config` is the JobConfig file — the agent's
  native config travels via `--agent-kwarg config=`.)
- `env` agent kwarg: comma-separated host env-var names forwarded by
  name — the declared credential passthrough matching the eval
  manifest's env-admission rule. (`--agent-env`/`extra_env` takes
  `KEY=VALUE` pairs; the kwarg form keeps values out of argv.)
- `eval_flags` kwarg: manifest flag names → `CRUSH_EVAL_FLAGS`, so
  `resolved_options` lands in telemetry and baseline keys self-seed.

## Declare and ingest

```jsonc
// eval/experiments/tb21-<experiment>.json
{
  "name": "tb21-lever-read",
  "model": "hyper/<pin>", "temperature": 0.2,
  "corpus": ["harbor"],
  "arms": { "control": {"config": {"options": {}}},
            "treatment": {"config": {"options": {"lever": true}}} },
  "runs_per_trajectory": {"uncharacterized": 1},
  "harbor": {
    "dataset": "terminal-bench@2.1",
    "tb21_split": "dev",          // or "task_ids": [...], or "sample": {...}
    "k": 1,
    "harness_revision": "<git sha>"
  },
  "resample": "cluster",
  "decision_rule": { "hypothesis": "...", "ood": true, "primary": {...} }
}
```

`crush eval run` refuses harbor experiments — records arrive only
via ingest:

```sh
crush eval ingest eval/experiments/tb21-<experiment>.json \
    --arm control --invocation <run-id> jobs/<control-job>/*/ 
crush eval ingest eval/experiments/tb21-<experiment>.json \
    --arm treatment --invocation <run-id> jobs/<treatment-job>/*/
crush eval compare eval/experiments/tb21-<experiment>.json --invocation <run-id>
```

run_index is the per-task ordinal of each arm's sorted trial dirs —
attempt i in control pairs attempt i in treatment, the same drift-
matched pairing the in-house scheduler produces. A task with no
trial in either arm lands on the snapshot's `skipped` list and
voids the verdict (the corpus wasn't covered); a task with an
infra-error trial gets an error record — it ran.

## Ops notes from the field

- **LiteLLM's "could not resolve model" warning is cosmetic.** Harbor
  passes the model name through; resolution happens inside the agent
  against `.crush.json`'s `providers` block. No action needed.
- **Disk accumulates fast** — ~30 GB per 60 trials across images,
  build cache, and stopped envs. After a job drains:
  `docker builder prune -f && docker image prune -f`.
- **Env-start timeouts orphan containers.** Timed-out environments
  keep running (`<task>__env-main-1`, sometimes for hours). Sweep with
  `docker ps`, remove the orphans — their trials are already recorded
  as exceptions.
- **`mteb-retrieve` (terminal-bench@2.0) has a broken image** — it
  never finishes building, even at 4× env-build timeout. Exclude it
  from measured subsets rather than reporting it as an infra loss.
- **Agents can orphan tool-spawned processes.** A trial whose agent
  leaves a live subprocess (e.g. a sampling script) keeps the task
  container hot through artifact collection. Check `docker stats`
  when a trial lingers in "Collecting main service artifacts".

## Blind-safety note

`tb21_split: "sealed"` resolves through `LoadTB21Manifest` — the
greppable phase-exit enumeration point (#439). Dev-split manifests
are unrestricted; keep experiments on `dev` until the §6 acceptance
read.
