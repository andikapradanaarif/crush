# Procedural Memory — Verified Skills Mined from Accepted Runs

> **Status:** Design proposal for #298 (Phase 3 of the 10-09 roadmap).
> Defines the channel's data model, pipeline stages, guards, and
> measurement plan before any code lands. Sibling to `ROUTE_SPACE.md`
> (the decision contract this channel must satisfy) and `LEARNED_PARAMS.md`
> (the parameter discipline its knobs follow).

## Claim and honest calibration

The claim under test: *procedures that repeatedly succeed in this repo
can be distilled into reusable instructions that reduce discovery effort
on the next occurrence of the same task shape.*

What the literature gives us: Agent Workflow Memory (+24.6%/+51.1%
relative success on Mind2Web/WebArena), ASI (+23.5% vs static, attributed
to programmatic verification), Memp (strong-model skills transfer to
weaker models), ReasoningBank (mine failures too), ACE (+10.6%; steps
as pointers, not narrative — the anti-"brevity-bias" constraint).

**Every one of those numbers is web-navigation, household, or game
benchmarks. Zero are long-horizon coding tasks.** The procedural-memory
gain on our task class is an extrapolation. The design therefore treats
measurement as a stage, not an afterthought: if the LOO channel cannot
measure an effect on #227's expensive-discovery cells, the pipeline
ships dormant behind its render flag — the same discipline the
notebook channel earned.

## What exists already (substrate audit)

| Component | Role in this design | Location |
|---|---|---|
| `Skill`/`SKILL.md` + discovery | Render surface — mined skills join as ordinary project skills | `internal/skills` |
| `Manager` + `Tracker` | Listing + in-session loaded tracking | `internal/skills` |
| `view` tool skill path | Progressive disclosure — model loads instructions on demand | `internal/agent/tools/view.go` |
| `ProjectSkillsDir` | Discovery path seam a managed dir joins | `internal/config/load.go` |
| `messages` tool-call parts | The ordered per-session trace — mining input | `internal/db` |
| Artifact labels (#294) | "Accepted run" verdict — hash survival, commit, tests_green | `internal/cmdlog/labels.go` |
| `project_rates` (#296) | Channel-level decayed Beta gate — `signal="skill"` joins | `internal/cmdlog/rates.go` |
| Suggested marks | Contamination screen — rendered skill ≠ evidence | `MarkSuggested*` |
| Snapshot replay (#108) | Verify tier where boundaries exist | `internal/eval/replay.go` |
| small→large model fallback | Induction consult pattern (proved in #335) | `internal/agent/scope_resolver.go` |
| `params.Memory` | Channel knobs + param_version stamping | `internal/params` |
| `redact` | Secret stripping on induced text | `internal/redact` |
| `crush.db` + project_key | Partition + durability | `internal/cmdlog` |

**Gaps this design must build:** the skill store + lifecycle states,
the mining clustering pass, the induction contract + validators, the
staged-approval surface, durable `skill_usage` (replacing the
in-memory `Tracker` as the evidence record — the tracker stays as the
runtime mechanism), and the render-gate params.

## Vocabulary

- **episode** — one session's ordered, normalized tool-call skeleton,
  retained only when the session carries acceptance evidence.
- **step** — one normalized tool call: `(class, kind, target_skel)`.
  Bash → `(bash, kind, cmd_skel)`; edit/write → `(edit, "", path_skel)`;
  view/grep/glob → `(read, "", path_skel_or_pattern)`; other tools →
  `(tool_name, "", "")`.
- **skeleton** — the episode as a step sequence after literal stripping:
  paths → `(top_dir_class, ext)`, commands → `normalizeCommand` +
  argument positions blanked, URLs/issue numbers/commit SHAs → `<lit>`.
- **signature** — `sha256(skeleton)`; the cluster key. Path-prefix-free
  by construction, so the same procedure in two repos collides — which
  is the point: *evidence* pools cross-project even though *rendering*
  stays partitioned.
- **consensus skeleton** — the longest common subsequence of a
  signature-cluster's episode skeletons: positions where all episodes
  agree are **constants**; positions where they differ are **slots**
  (the skill's parameters).
- **candidate** — a `skill` row in state `mined`/`inducted`/`staged`;
  never rendered, never file-backed.
- **skill** (active) — an approved candidate materialized to the
  managed dir, listed in `<available_skills>` like any project skill.
- **convention** — kind=`convention`: a semantic fact ("options flow
  through `optionSpecs` → `schema.json`"), inspection-gated instead of
  replay-gated. Same store, same lifecycle, different verify path.

## Pipeline

Seven stages. Every stage boundary fails closed: an ambiguous input
produces no artifact, a failed check leaves the candidate where it was
with its rejection reason recorded.

```
sessions ──► [1 mine] ──► signature clusters (≥N clean episodes)
clusters ──► [2 induce] ──► SKILL.md draft (small→large consult)
drafts   ──► [3 validate] ──► skeleton bijection + param variance +
                              leak/injection screens
valid    ──► [4 verify] ──► evidence tier always; replay tier where
                            snapshots exist; shadow tier live
verified ──► [5 stage] ──► staged row; human approval (or eval
                            auto_admit) → active
active   ──► [6 retrieve] ──► materialize → <available_skills> → view
                            → MarkLoaded → skill_usage rows
usage    ──► [7 lifecycle] ──► deprecate-on-verify-fail, rate fold,
                               re-induction as new version
```

### 1. Mine — offline, never the hot path

Mining is a batch operation (`crush skills mine` — see Command
surface), not a per-turn pass: clustering plus LLM induction has no
business inside `MatureReferentLabels`'s per-turn budget. It scans the
project's `messages` for every session newer than the last mining
cursor, extracts each session's step skeleton, and:

1. Drops sessions whose acceptance evidence fails: any of
   `tests_green=0`, a revision-cue verdict on its judged turns, or —
   strongest signal — none of the artifact labels settled at all
   (unlabeled sessions are *unknown*, not accepted; ReasoningBank says
   failures are minable, but v1 mines only verified success and logs
   failed-session signatures for a later pass).
2. Drops steps the contamination screen owns: tool calls a
   `suggested`-marked command or path drove (echo, not evidence).
3. Normalizes to the skeleton and inserts under
   `(signature_hash, project_key)` episode rows — idempotent on
   `(session_id, signature)`.
4. Emits cluster candidates: signatures with `≥ skill_promote_sessions`
   (default 2) episodes across **distinct sessions** — same discipline
   as `referent_promote_hits`, sessions not observations.

Output is `skill_episodes` rows plus a `skills` candidate row in
state `mined` per qualifying signature — no LLM call, no files, no
prompt surface yet. Bounded: `--limit`, `--since`, cursor-persisted
so re-runs are incremental.

**Why signatures, not semantic clustering:** deterministic
fingerprints are auditable — "these 4 sessions share a skeleton" is a
checkable claim. Embedding-clustered episodes could produce
unverifiable groups (why is this session in this cluster? "the weights
said so" — forbidden by route-space rule 3). V1 trades recall for
auditability; looser clustering is a later learned-param question.

### 2. Induce — narrate a skeleton, never invent one

The induction consult gets, per candidate:

- the consensus skeleton (steps with constants filled, slots marked
  `{param}`),
- up to 3 source-episode summaries (session digest + step list with
  the differing slot values inlined),
- a fixed output contract.

The contract is the anti-collapse discipline (ACE): steps as
**pointers + commands**, not narrative prose — "run
`{test_cmd}`" not "verify your changes using the project's test
suite". Frontmatter whitelisted to `name`, `description`,
`user-invocable`, plus `metadata.kind` (`procedure`|`convention`) and
`metadata.signature`. Small-model slot first, large on failure — the
#335 pattern, reusing `scopeResolver`'s attempt loop shape. Consult
failures leave the candidate `mined` (retryable next pass), never
`staged` with a husk.

### 3. Validate — the skeleton is the spec

Deterministic, fail-closed — this is what keeps induction inside
route-space rule 3:

- `skills.ParseContent` succeeds; `name` matches `^[a-z0-9-]+$`,
  `description ≤ MaxDescriptionLength`.
- **Bijection**: every instruction step must map onto a consensus
  position (the validator walks both sequences; a step with no
  skeleton anchor is a hallucination — reject). Every consensus
  position must appear (a skipped step is a truncated procedure —
  reject).
- **Parameter honesty**: every `{param}` slot must show ≥2 distinct
  values across source episodes — a "parameter" that never varied is
  a literal in disguise. A literal value appearing in the skill that
  is not a consensus constant is rejected (one episode's specifics
  leaking through).
- **Leakage screen**: the skill body is checked for strings lifted
  from episode *outputs* (test failure text, file contents) — a
  procedure that embeds one run's answer is contamination. Token-set
  overlap against stored output snippets must stay under threshold.
- **Injection screen**: `redact` secrets pass on the body; no
  absolute paths outside the workspace; fenced commands parse as
  shell (`shell.Components` exists for this).

Rejection writes `verify_state=rejected, reject_reason=<code>` —
closed vocabulary (`hallucinated_step`, `truncated_skeleton`,
`constant_slot`, `literal_leak`, `output_leak`, `secret_hit`,
`bad_frontmatter`, `parse_fail`), recorded so the induce prompt's
failure modes are enumerable and fixable.

### 4. Verify — three tiers, honest about which applies where

The ASI insight — verification is what makes induced skills reliable —
adapts to what state we can actually reconstruct:

- **Evidence tier (always required).** The admission floor is the
  mining gate itself: `≥N` distinct sessions independently ran this
  skeleton to a clean verdict. That is already N independent
  verifications — stronger than one replayed run, weaker than a
  controlled replay, and *free*.
- **Replay tier (where boundaries exist).** In evals,
  `recordReplayPrefix`-style snapshots let a held-out instance replay
  with the skill rendered vs suppressed — the controlled
  `executeReplayRun` primitive measures sufficiency directly
  (`skill on` vs `skill off` on the same boundary). For eval-mined
  candidates this runs before auto-admission. Generalization needs
  `≥3` instances, claimed honestly — 2 instances show replayability,
  not generalization.
- **Shadow tier (production, post-approval).** Dogfooded sessions
  have no per-turn snapshots — the pre-turn state cannot be
  reconstructed, and pretending otherwise is dishonest. Production
  verification is *prospective*: the next signature-matched session
  renders the skill under propensity-logged marks, and its outcome
  folds into `skill_usage` — live verification rather than replay.

Each verified tier stamps `verify_evidence` (JSON: tier, instance
count, verdicts, snapshot/record refs) on the row — the audit trail
answers "why is this active" permanently.

### 5. Stage — the human gate is the load-bearing control

`staged` candidates are visible to the user (`crush skills review`),
render nothing, and write no files. Approval (`crush skills approve
<name>` or eval `skill_auto_admit`) flips `active`. This is the only
stage that may cross a trust boundary, so it is where two decisions
live:

- **Admissibility scope.** Rows carry `origin_project_keys` (every
  project whose episodes fed the signature). Rendering defaults to
  same-`project_key` only; approval may set `admit_scope=global`,
  making cross-project rendering an explicit human decision — the
  #229 leakage screen lands where a person can judge it, not in a
  clustering heuristic.
- **Skill-vs-convention sanity.** The reviewer sees the rendered
  SKILL.md plus its evidence card (source sessions, tier verdicts).
  Human time is the cost asymmetry: one skim beats a mis-admitted
  procedure running for months.

Rejected/dismissed candidates keep their rows (with `staged_reason`)
— a dismissed signature must not re-induce on the next mining pass;
the row is the memory of "we looked."

### 6. Retrieve — the model's disclosure, not our injection

V1 renders through the **existing skills mechanism**, not a tail
section:

- On session start (and after approval events), `active` skills whose
  `admit_scope` admits this `project_key` materialize as
  `SKILL.md` under a managed dir — `<data_dir>/skills/<name>/` —
  appended to `DiscoveryConfig.SkillsPaths` where `ProjectSkillsDir`
  already joins. Source label: `project` (a new `SourceMined`
  `SourceType` so catalogs and prompts can mark them).
- The model sees name+description in `<available_skills>` exactly
  like a human-authored skill; it `view`s the file when its
  description matches the task — progressive disclosure we don't
  have to build, and the distractor finding is respected
  automatically (only a line each, never the body).
- `MarkLoaded` (view.go already calls it) becomes the durable signal:
  the load path writes `skill_usage(event=loaded)` rows; listing
  writes `event=listed` per session.
- Both events mark `suggested` (`skill:`-namespaced) so an outcome
  a loaded skill drove never feeds its own evidence — the
  self-reinforcing-heat screen, third channel.
- Render cap: `skill_render_limit` bounds the materialized set
  (newest-active-first; 0 = channel absent for LOO, suppressing at
  the materialization seam — no rows listed, no usage events, same
  contract as the other zero-limits).

**Deliberately absent in v1:** proactive `<learned_skills>` tail
injection. That is a selector route of its own (needs a relevance
check before spending tokens — signature-match-the-prompt is a
different, harder problem than cluster-the-trace) and earns its own
route-space entry when designed. The listing mechanism gets skills in
front of the model with zero new injection surface.

### 7. Lifecycle — decay, deprecate, never auto-revise

- `skill_usage` is the durable event ledger: `(skill_id, session_id,
  event, tool_call_id, suggested, project_key, param_version,
  created_at)`. Events: `listed`, `loaded`, `verify_pass`,
  `verify_fail`, `deprecated`, `revived`. The in-memory `Tracker`
  stays the runtime mechanism; the table is the evidence.
- **Deprecate-on-verification-fail:** a session that loaded a skill
  and then failed its verify gate writes `verify_fail`; at the
  threshold (param `skill_deprecate_fails`, default 1 for
  replay-evidenced fails, higher for weak signals) the row returns to
  `staged` *with the failing trace attached* — never silently
  revised, per the issue's rule. Re-approval requires human look at
  the failure.
- **Rate fold:** `project_rates` gains `signal="skill:<id>"` rows via
  the same decayed-Beta fold (`UpdateProjectRates` shape, same
  `rate_decay`/`rate_quantile` params); a channel-level `signal="skill"`
  aggregate powers a `skill_rate_floor` render gate — a project whose
  skills keep failing stops listing them without a human having to
  clean house.
- **Re-induction:** a signature that accrues `≥N` *new* episodes since
  its skill's induction queues a fresh candidate (new `version`,
  linked `supersedes` pointer) — the active row is never mutated
  in place; swap is atomic at approval.

## Data model

```sql
-- One row per (signature, kind) mined candidate. Lifecycle states:
-- mined → inducted → staged → active → deprecated; rejected is
-- terminal with reject_reason. Same signature may chain versions
-- via supersedes.
CREATE TABLE skills (
  id              TEXT PRIMARY KEY,           -- sk_<ulid>
  signature_hash  TEXT NOT NULL,
  kind            TEXT NOT NULL,              -- procedure|convention
  name            TEXT NOT NULL,
  description     TEXT NOT NULL DEFAULT '',
  body            TEXT NOT NULL DEFAULT '',   -- full SKILL.md text
  state           TEXT NOT NULL,              -- mined|inducted|staged|
                                            -- active|deprecated|rejected
  reject_reason   TEXT NOT NULL DEFAULT '',
  version         INTEGER NOT NULL DEFAULT 1,
  supersedes      TEXT NOT NULL DEFAULT '',   -- prior skill id
  origin_projects TEXT NOT NULL DEFAULT '[]', -- JSON project_keys
  induced_by      TEXT NOT NULL DEFAULT '',   -- provider/model
  verify_evidence TEXT NOT NULL DEFAULT '{}', -- JSON tier card
  admit_scope     TEXT NOT NULL DEFAULT 'project', -- project|global
  project_key     TEXT NOT NULL,
  param_version   TEXT NOT NULL,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  UNIQUE(signature_hash, kind, version)
);
CREATE INDEX idx_skills_render ON skills(state, project_key, admit_scope);
CREATE INDEX idx_skills_sig    ON skills(signature_hash, state);

-- Episodes feeding signatures (mining evidence + param variance).
CREATE TABLE skill_episodes (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  signature_hash TEXT NOT NULL,
  session_id     TEXT NOT NULL,
  project_key    TEXT NOT NULL,
  skeleton       TEXT NOT NULL,   -- JSON normalized steps
  slot_values    TEXT NOT NULL,   -- JSON {slot: [values seen]}
  verdict        TEXT NOT NULL,   -- clean|failed|unknown
  suggested      INTEGER NOT NULL DEFAULT 0,
  param_version  TEXT NOT NULL,
  created_at     INTEGER NOT NULL,
  UNIQUE(session_id, signature_hash)
);

-- Durable usage ledger — the in-memory Tracker's evidence twin.
CREATE TABLE skill_usage (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  skill_id      TEXT NOT NULL,
  session_id    TEXT NOT NULL,
  event         TEXT NOT NULL,    -- listed|loaded|verify_pass|
                                  -- verify_fail|deprecated|revived
  tool_call_id  TEXT NOT NULL DEFAULT '',
  suggested     INTEGER NOT NULL DEFAULT 0,
  project_key   TEXT NOT NULL,
  param_version TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);
CREATE INDEX idx_skill_usage ON skill_usage(skill_id, event);
```

Plus a tiny `mining_cursor` row (or a `meta` key) for the incremental
scan watermark. Provenance pack (session/tool_call/project_key/
param_version/suggested) matches the #220 contract every other memory
table carries.

## Command surface

```
crush skills mine [--project P] [--limit N] [--since T] [--dry]
   — stage 1+2+3+4 batch; prints cluster/candidate/reject counts
     and writes audit rows; never touches active state.
crush skills review [--project P]
   — staged candidates with evidence cards (source sessions,
     tier verdicts, diff vs superseded version).
crush skills approve|dismiss|deprecate <name> [--global]
   — the only state transitions that reach render; --global sets
     admit_scope on cross-project candidates.
crush skills list   — active + staged + deprecated, per project.
```

Mining is deliberately a command, not a daemon pass — it runs when
evidence exists to mine, its LLM spend is explicit, and `--dry` makes
the pipeline inspectable end-to-end before it writes anything.
(Eval-side, `crush eval` gains a `skill_mine` probe that runs the same
code over preserved session DBs.)

## Params, telemetry, coverage

New `params.Memory` fields (each addition rotates `param_version` —
intended):

| Field | Default | Role |
|---|---|---|
| `skill_promote_sessions` | 2 | distinct-session evidence floor |
| `skill_render_limit` | 3 | materialized active cap (0 = channel off — LOO) |
| `skill_deprecate_fails` | 1 | verify_fail count that returns to staged |
| `skill_rate_floor` | 0.6 | channel render gate via project_rates |
| `skill_auto_admit` | 0 | eval-only: staged→active without human |

Tail stamps mirror the resolver's engagement vocabulary — the same
absent/engaged/dead discipline that fixed mix-cs:

- `tail.skills.active` — active skills materialized this turn (count).
- `tail.skills.listed` / `tail.skills.loaded` — names.
- Coverage predicates: `min_tail.skills.listed` asserts the channel
  engaged; `max_tail.skills.dead: 0` catches active-but-never-loaded
  (description failing to attract) without starving cells that have
  no active skills to list.
- `tail.decisions` does **not** extend — v1 selection is the model's
  own description-match, so there is no per-candidate dispatch to
  record. Honest gap: same as session digests' known thin spot;
  the usage table carries what the tail can't.

## Measurement plan

1. **LOO channel on #227 cells** — `skill_render_limit: 0` arm vs
  armed arm on the expensive-discovery corpus. The only place
  "capability, not just speed" can show; the issue's own
  precondition for building the full lifecycle.
2. **Replay A/B** — for eval-mined candidates: held-out instance,
  boundary snapshot, arm A skill-rendered vs arm B suppressed →
  pass/step delta with the standard estimator. This *is* the replay
  verify tier, so admission and measurement share machinery.
3. **#225 transfer test** — skills induced under model A rendered
  under model B (Memp's prediction); a `skills from` provenance read
  on the usage rows quantifies cross-model reuse.
4. **Guardrails** — pass-rate no-harm (≤10% drop), step/token cost
  deltas, `verify_fail` rate, deprecate churn, holdout screens
  unchanged (~10% sessions still suppress marks).

## Phasing

- **A — store + mining (offline-only):** tables, miner CLI, induction
  contract, validators, screens. Nothing renders; `crush skills
  review` shows what it *would* mint. Dogfooding DBs + preserved eval
  artifacts are the corpus.
- **B — retrieval + lifecycle:** materialization, `admit_scope`,
  usage table, deprecate path, rate fold. Human-approved skills reach
  prompts in production.
- **C — measure:** LOO arm on #227, replay A/B, transfer. The
  calibration-honesty exit: unmeasurable → ship dormant
  (`skill_render_limit: 0` default), pipeline stays as mining
  instrumentation.

Phase A is buildable against today's data — nexus's 135-row command
ledger + ~31 dogfooding sessions + every preserved eval session DB.
A's own deliverable is the answer to "do signatures ≥2 even exist in
real usage" — the cheapest falsification of the whole channel,
runnable before any render code exists.

## Open questions

- Signature granularity: does the step skeleton need turn-boundary
  markers (per-prompt sub-episodes) or is whole-session the right
  unit? Whole-session risks conflating "fixed A then built B"; the
  miner may need per-prompt segmentation — resolve against real
  traces in Phase A before freezing the schema.
- Convention kind's verify path: inspection gate only, or also
  stale-check (claim paths that no longer exist → auto-flag)? v1:
  approval-time inspection + periodic `path_gone` flagging.
- Cross-project mining vs partition discipline: evidence pooling is
  allowed (signatures are path-free), but should `origin_projects`
  require *all* projects' keys present for a global claim, or is one
  enough? Conservative default: same-project render regardless;
  global requires human flag.
- Whether `skill_usage` needs the verdict backlink (which verify gate
  outcome the session's loads produced) or joins via session_id only.
  v1 joins lazily; a denormalized verdict column is a schema
  migration away if analytics need it.

## Route-space checklist (for the record)

- [x] Enumerable path: the seven stages and their exits, above.
- [x] Closed vocabulary: states (6), reject_reasons (7), usage
      events (6), tail stamps (4) — all listed.
- [x] Decision records: `verify_evidence` cards + usage rows +
      `tail.skills.*` stamps.
- [x] Offline benchmark: Phase A produces the signature census —
      falsifiable before render exists.
- [x] Learned params: five new slots in `params.Memory`, versioned.
- [x] Topology flag: new channel = mechanism change; this doc *is*
      the proposal stage of the evolution loop.
