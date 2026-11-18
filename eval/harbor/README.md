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
- `max_tokens` kwarg: per-trial cumulative token budget →
  `CRUSH_EVAL_MAX_TOKENS` — a runaway trial stops at the cap instead
  of burning to the agent timeout (observed: an 11.5M-token run).

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

**One Harbor job per arm — never ingest one job under two arms.**
Each arm gets its own `harbor run` with its own rendered config;
ingesting the same trial dir (or the same harbor trial id) under a
second arm or a second invocation is refused — a duplicated job is a
fake perfect A/A, not paired data (the pilot-b2 defect, #504).

Ingest also refuses records that contradict the manifest's pins: a
resolved `model` ≠ `model`, a resolved `model_small` ≠ `small_model`
when declared, or — once the child reports sampling params — a
request-side `temperature` ≠ the declared `temperature`. So the arm
config must carry what the manifest declares: the `models` block's
`large`/`small` pins **and** its `temperature` pin (declaring
`"temperature": 0` in the manifest while the arm config sends none
is exactly the pilot's silent-drift defect).

## Runbook — `tb21-smoke-deepseek` (#506, W3)

Owner-executed: ~20 paid trials on `opencode-go/deepseek-v4.1-flash`
feeding the D1 model-pin decision. Estimated spend at the manifest's
pinned rates: ≈ $0.30–$2.40. The agent prepares artifacts only.

```sh
# 1. Build the pinned binary — its `crush --version` sha is
#    harbor.harness_revision. Re-pin the manifest if main has moved.
git checkout 1715db5098   # manifest's harness_revision
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o crush-linux-amd64 .

# 2. Source credentials — OPENCODE_API_KEY reaches the container via
#    the env kwarg (never argv). Arm config: arm-deepseek.crush.json.
source ~/.config/crush/secrets.env

# 3. Single control-arm job: 10 tasks × k=2 = 20 trials.
PYTHONPATH=$PWD harbor run \
    -d terminal-bench@2.0 \
    --agent eval.harbor.crush_agent:Crush \
    --agent-kwarg binary_path="$PWD/crush-linux-amd64" \
    --agent-kwarg harness_revision=1715db5098 \
    --agent-kwarg config="$PWD/arm-deepseek.crush.json" \
    --agent-kwarg env=OPENCODE_API_KEY \
    --agent-kwarg max_tokens=8000000 \
    -i bn-fit-modify -i code-from-image -i db-wal-recovery \
    -i feal-linear-cryptanalysis -i kv-store-grpc -i mcmc-sampling-stan \
    -i path-tracing-reverse -i pytorch-model-cli -i regex-log \
    -i train-fasttext \
    --environment-build-timeout-multiplier 4 \
    --agent-timeout-multiplier 2 \
    --verifier-timeout-multiplier 4 \
    -n 2 -k 2

# 4. Ingest the job under a fresh invocation id, then the #506 table:
#    pass band [0.20, 0.90], ≥16/20 conclusive, sampling provenance,
#    measured $/trial, 429s, per-task 2-rep agreement (flips).
crush eval ingest eval/experiments/tb21-smoke-deepseek.json \
    --arm control --invocation deepseek-smoke-1 jobs/<job-dir>/*/
crush eval noise eval/experiments/tb21-smoke-deepseek.json \
    --cells deepseek-smoke-1 --passes
```

The manifest's `treatment` arm is a schema-required stub — the
loader demands the control/treatment pair, so it mirrors `control`
exactly (same shape as `tb21-smoke-longcat`). One job runs and
ingests under `--arm control` only; the #506 table needs no pair.

`cache_write` is pinned at the input rate ($0.30): opencode-zen
publishes no separate write price and billing treats writes as input
— pinning it at 0 would price that class free by default. The
`max_tokens` kwarg caps a runaway trial (observed: an 11.5M-token
run) instead of burning to the agent timeout; 8M is ~2× the observed
per-trial mix. Do **not** pass `--model` — the arm config's `models`
block carries the large/small pins (see the note above).

## Runbook — `tb21-krep-deepseek` (#503)

Owner-executed: ~464 paid trials (29 tasks × k=8 × 2 arms — the
`mteb-retrieve` exclusion is recorded in the manifest's `exclude`
block, so it is absent from the run's `-i` list rather than burned
on doomed env-builds). Estimated spend at the smoke's measured
$0.0275/trial: ≈ $13. The agent prepares artifacts only; run this
after D1 lands the deepseek pin.

The k=8 schedule runs as **8 invocation slices** of 1 rep/task/arm
(EVAL_HARNESS.md — `run_index` accrues per arm across ingests, and
`--cells` pools the slices). Each slice = 2 jobs, one per arm.

```sh
# 1. Build the pinned binary. Manifest harness_revision:
git checkout baa8bdf3af
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o crush-linux-amd64 .

# 2. Credentials + arm config (same as #506):
source ~/.config/crush/secrets.env

# 3. Eight slices; each runs both arms' jobs. `jobs/` grows a fresh
#    <timestamp> dir per harbor run — capture it for step 4.
TASKS="-i bn-fit-modify -i build-pov-ray -i chess-best-move \
  -i code-from-image -i constraints-scheduling -i crack-7z-hash \
  -i db-wal-recovery -i dna-insert -i extract-moves-from-video \
  -i feal-linear-cryptanalysis -i fix-git -i git-multibranch \
  -i kv-store-grpc -i log-summary-date-ranges -i make-doom-for-mips \
  -i mcmc-sampling-stan -i overfull-hbox -i path-tracing-reverse \
  -i polyglot-rust-c -i prove-plus-comm -i pytorch-model-cli \
  -i qemu-alpine-ssh -i query-optimize -i regex-log \
  -i schemelike-metacircular-eval -i torch-pipeline-parallelism \
  -i train-fasttext -i video-processing -i write-compressor"
for slice in 1 2 3 4 5 6 7 8; do
  for armjob in control treatment; do
    PYTHONPATH=$PWD harbor run \
        -d terminal-bench@2.0 \
        --agent eval.harbor.crush_agent:Crush \
        --agent-kwarg binary_path="$PWD/crush-linux-amd64" \
        --agent-kwarg harness_revision=baa8bdf3af \
        --agent-kwarg config="$PWD/arm-deepseek.crush.json" \
        --agent-kwarg env=OPENCODE_API_KEY \
        --agent-kwarg max_tokens=8000000 \
        $TASKS \
        --environment-build-timeout-multiplier 4 \
        --agent-timeout-multiplier 2 \
        --verifier-timeout-multiplier 4 \
        -n 2 -k 1
  done
done

#    Wall-clock: ~4 h per 29-trial job at -n 2 → ~64 h sequential.
#    Run slices in parallel shells (or raise -n) for the parallelism
#    the slicing exists for.

# 4. Per slice, ingest each arm's job under the slice invocation:
crush eval ingest eval/experiments/tb21-krep-deepseek.json \
    --arm control   --invocation krep-ds-1 jobs/<control-job-1>/*/
crush eval ingest eval/experiments/tb21-krep-deepseek.json \
    --arm treatment --invocation krep-ds-1 jobs/<treat-job-1>/*/
#    …repeat for slices 2–8…

# 5. Pool all slices into the 8-rep matrix per arm:
crush eval noise eval/experiments/tb21-krep-deepseek.json \
    --cells krep-ds-1,krep-ds-2,krep-ds-3,krep-ds-4,krep-ds-5,krep-ds-6,krep-ds-7,krep-ds-8
```

Both arms run the identical config deliberately: per-arm σ_within
measured on both sides lets a later read test the symmetric-within
assumption #501 currently plugs, and the A/A pairing is the
harness's own drift check. Re-pin `harness_revision` if main moves
before the run — the whole batch must pool under one binary.

## Runbook — `tb21-n1a-deepseek` (#510)

Owner-executed: 464 trials (29 tasks × k=8 × 2 arms — control is
harbor's built-in `terminus-2`, treatment is the Crush wrapper)
feeding the N1a anchor: pass non-inferiority at margin δ. Estimated
spend at the smoke's measured $0.0275/trial: ≈ $13 (terminus-side
trials price at the same pinned rates — same route).

**Model parity.** Terminus-2 takes its model as a LiteLLM string:
`openai/deepseek-v4.1-flash` + `api_base=https://opencode.ai/zen/go/v1`
hits the same route and weights as the crush arm's
`opencode-go/deepseek-v4.1-flash`. Temperature parity is exact —
terminus-2 accepts `temperature=0` as an agent kwarg. Remaining gap,
recorded not fixed: terminus-2 is a single-loop agent — it makes no
small-model/auxiliary calls, so the manifest's small-model pin
applies to the crush arm only. That asymmetry is the comparison's
subject, not a defect.

**Margin.** `decision_rule.guardrail.min_delta: -0.12` is pinned
from the smoke's δ̂ — **re-pin it to #503's measured k-rep δ̂
before running**; the manifest's pre-registration depends on the
characterization number.

```sh
# 1. Binary + credentials (same as #503):
git checkout 3d2ade856d   # manifest's harness_revision
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o crush-linux-amd64 .
source ~/.config/crush/secrets.env
export OPENAI_API_KEY="$OPENCODE_API_KEY"   # terminus reads litellm's
                                          # OPENAI_* env convention

# 2. Control job — terminus-2 on the same 29 effective tasks, k=8
#    ($TASKS is the list defined in the #503 section above):
PYTHONPATH=$PWD harbor run \
    -d terminal-bench@2.0 \
    --agent terminus-2 \
    --model openai/deepseek-v4.1-flash \
    --agent-kwarg api_base=https://opencode.ai/zen/go/v1 \
    --agent-kwarg temperature=0 \
    --ae OPENAI_API_KEY \
    $TASKS \
    --environment-build-timeout-multiplier 4 \
    --agent-timeout-multiplier 2 \
    --verifier-timeout-multiplier 4 \
    -n 2 -k 8

# 3. Treatment job — the crush wrapper, same tasks/k (the #503
#    treatment-arm command shape):
PYTHONPATH=$PWD harbor run \
    -d terminal-bench@2.0 \
    --agent eval.harbor.crush_agent:Crush \
    --agent-kwarg binary_path="$PWD/crush-linux-amd64" \
    --agent-kwarg harness_revision=3d2ade856d \
    --agent-kwarg config="$PWD/arm-deepseek.crush.json" \
    --agent-kwarg env=OPENCODE_API_KEY \
    --agent-kwarg max_tokens=8000000 \
    $TASKS \
    --environment-build-timeout-multiplier 4 \
    --agent-timeout-multiplier 2 \
    --verifier-timeout-multiplier 4 \
    -n 2 -k 8

# 4. Ingest under one invocation, then compare:
crush eval ingest eval/experiments/tb21-n1a-deepseek.json \
    --arm control   --invocation n1a-1 jobs/<terminus-job>/*/
crush eval ingest eval/experiments/tb21-n1a-deepseek.json \
    --arm treatment --invocation n1a-1 jobs/<crush-job>/*/
crush eval compare eval/experiments/tb21-n1a-deepseek.json \
    --invocation n1a-1
```

Ingest verifies agent identity both ways: terminus trials under
`--arm control` must report `terminus-2`, and a non-crush agent
under `--arm treatment` refuses. The reported `model_name` must
equal the manifest's declared `openai/deepseek-v4.1-flash` spelling.

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
