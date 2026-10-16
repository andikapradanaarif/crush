# Procedural Memory — Verified Skills Mined from Accepted Turns

> **Status:** Design proposal for #298 (Phase 3 of the 10-09 roadmap),
> v2. Defines the channel's data model, pipeline stages, guards, and
> measurement plan before any code lands. Sibling to `ROUTE_SPACE.md`
> (the decision contract this channel must satisfy) and
> `LEARNED_PARAMS.md` (the parameter discipline its knobs follow).
>
> **v1 → v2:** review found the mining stage was falsifiable in the
> wrong way — its input filter depended on `referent_episodes`, which
> hold 0 rows across all 25 nexus dogfooding sessions, so the census
> would have returned empty regardless of channel quality. v2 moves
> the evidence unit to per-user-turn outcome labels (the #340 label
> fix, applied at the unit mining needs), segments by turn, clusters
> on a procedure core instead of whole-session hashes, keeps real
> repo-relative paths in the project signature, relaxes the bijection
> to the core, and adds the operations section (spend, concurrency,
> purge/kill-switch, worktrees, review UX) plus production gates.

## Claim and honest calibration

The claim under test: *procedures that repeatedly succeed in this repo
can be distilled into reusable instructions that reduce discovery
effort on the next occurrence of the same task shape.*

What the literature gives us: Agent Workflow Memory (+24.6%/+51.1%
relative success on Mind2Web/WebArena), ASI (+23.5% vs static,
attributed to programmatic verification), Memp (strong-model skills
transfer to weaker models), ReasoningBank (mine failures too), ACE
(+10.6%; steps as pointers, not narrative — the anti-"brevity-bias"
constraint).

**Every one of those numbers is web-navigation, household, or game
benchmarks. Zero are long-horizon coding tasks.** The procedural-
memory gain on our task class is an extrapolation. The design
therefore treats measurement as a stage, not an afterthought: if the
LOO channel cannot measure an effect on #227's expensive-discovery
cells, the pipeline ships dormant behind its channel flag — the same
discipline the notebook channel earned.

## What exists already (substrate audit)

| Component | Role in this design | Location |
|---|---|---|
| `Skill`/`SKILL.md` + discovery | Render surface — mined skills join as ordinary project skills | `internal/skills` |
| `Manager` + `Tracker` | Listing + in-session loaded tracking | `internal/skills` |
| `view` tool skill path | Progressive disclosure — model loads instructions on demand | `internal/agent/tools/view.go` |
| `ProjectSkillsDir` | Discovery path seam a managed dir joins | `internal/config/load.go` |
| `messages` tool-call parts | The ordered per-turn trace — mining input | `internal/db` |
| Artifact labels (#294) | Survival/commit/tests evidence — extended to turn scope by #340 | `internal/cmdlog/labels.go` |
| `project_rates` (#296) | Channel-level decayed Beta gate — `signal="skill"` joins | `internal/cmdlog/rates.go` |
| Suggested marks | Contamination screen — rendered skill ≠ evidence | `MarkSuggested*` |
| Snapshot replay (#108) | Verify tier where boundaries exist | `internal/eval/replay.go` |
| small→large model fallback | Induction consult pattern (proved in #335) | `internal/agent/scope_resolver.go` |
| `mvdan.cc/sh/v3/syntax` | Shell parse for fenced-command validation | `internal/shell/dispatch.go` |
| `params.Memory` | Channel knobs + param_version stamping | `internal/params` |
| `redact` | Secret stripping on induced text | `internal/redact` |
| `crush.db` + project_key | Partition + durability | `internal/cmdlog` |

**Gaps this design must build:** turn-level outcome labels (part of
#340), the skill store + lifecycle states, the turn segmentation +
core clustering pass, the induction contract + validators, the
staged-approval surface, durable `skill_usage` (replacing the
in-memory `Tracker` as the evidence record — the tracker stays as
the runtime mechanism), and the render/channel params.

## Outcome labels — the input the channel actually has

v1 filtered on `referent_episodes` artifact labels. That table
holds **zero rows** in nexus after 25 sessions — episodes only
record on vague-prompt + single-edit turns, a shape real usage
rarely takes. Mining needs a label at the unit it clusters:
**the user turn** (prompt → agent's tool calls → stop).

`turn_outcomes` (built under #340, this channel's G1 gate):

- **Unit:** one row per (session, turn's user-message ID) — the
  boundary message is the key, stable under compaction where a
  slice-relative index would shift between the live tail and the
  full-transcript backfill. Recording the turn's edited-file set,
  added-line fingerprints, and command list.
- **Settled once**, at a fixed window: the start of the next session
  provably in the project, or created_at + 24h, whichever comes
  first. `sessions` carries no project column, so membership is
  ledger proof — a session counts as a boundary only after it has
  written a project-keyed row (`command_memory`, `failure_memory`,
  `referent_episodes`). A foreign session in a shared store closes
  no window; a session with no rows yet delays, never corrupts.
  After settlement the verdict never re-derives — fold and
  calibration read only settled rows (fixes the label-drift defect
  from the loop review). Referent promotion is settlement-driven
  the same way: `CountCleanReferentAcceptances` counts frozen rows
  under the artifact-first outcome rule, so a phrase→target earns
  `referent_memory` only when its Nth clean acceptance *freezes* —
  fired from `settleReferent` on the crossing row, and from the
  record path when older settled evidence already clears the floor
  (a fresh episode can never count itself).
- **Survival per hunk:** the diff hunks the turn's edits produced,
  checked line-presence at settlement — unrelated edits elsewhere in
  the same file do not count as divergence. Both sides normalize to
  trimmed non-empty unique lines (fingerprints store trimmed text,
  so the file side must match or indented code reads diverged).
  Out-of-workspace paths abstain (NULL) — distinct from an in-scope
  deleted file, which is real divergence.
- **Commit evidence filtered:** commits made *by* crush (recorded in
  `command_memory` with timestamps, including `&&`/`;`/`||` chains
  and `git -C`/`-c`/`--git-dir` forms) do not count toward
  acceptance. User-side commit inside the window is a positive
  signal only when no divergence precedes it (revised-then-committed
  is a revision). Evidence is prefetched per settle batch — one git
  log over the union window — and paths strip the repo prefix, so
  workspaces in repo subdirectories still see their commits.
- **Verdict vocabulary:** `accepted` | `revised` | `failed` |
  `unknown`. `tests_green` at session end and absence of a
  revision-cue verdict on the *next* user turn feed the decision.
  Unobservable cases land `unknown`, never default-accepted.
- **Live prompt excluded:** the initiating user message persists
  before the record pass reads the transcript, so the pass carries
  its ID and skips it — otherwise the in-flight prompt mints a
  phantom turn whose unique key blocks its own evidence row.
- **Backfill placeholders repair:** a concurrent session's in-flight
  final turn backfills with an empty verdict; a separate guarded
  repair update refreshes evidence and judgment while the row is
  unsettled and unjudged (`next_verdict = ''` only — `'unknown'` is
  a resolved cue, not a placeholder). A judged or frozen row is
  never rewritten. The corpus itself is convergent: only sessions
  with some unrecorded real user turn are scanned, so fully covered
  history drains instead of re-sliding over the newest slice.
- **Detached record pass:** the whole turn-outcome record — tail
  extraction, full-transcript fallback, and corpus backfill — runs
  on a detached slot off the prompt path; none of it feeds the
  render and settlement tolerates late rows. Every agent field the
  pass touches (`messages`, `sessions`, `cmdlog`, `configStore`'s
  working dir) is snapshotted on the request goroutine before the
  spawn, the same contract the settle pass uses.

Mining keeps only `accepted` turns (v1; `failed`/`revised` traces are
logged for a later ReasoningBank-style pass). This is the label the
whole channel inherits — its correctness is G1.

## Vocabulary

- **turn episode** — one user turn's ordered, normalized tool calls,
  retained only when the turn's settled outcome is `accepted`.
- **procedure core** — the subsequence of mutating and verifying
  steps: edit/write calls plus `bash` commands classified
  build/test/generate/lint/migrate. Reads (`view`, `grep`, `glob`,
  `ls`-class) are *context*, not procedure — they leave the
  signature entirely. (Consultation evidence is recovered later as
  pointers, per validate.)
- **step** — one normalized call: `(class, kind, target)`.
  Edit/write → `(edit, "", relpath)`; core bash →
  `(bash, kind, normalizeCommand)`; other tools `(name, "", "")`.
- **project signature** — `sha256(core steps with real repo-relative
  paths)`. The primary key. Within a project the paths *are* the
  procedure ("`config.go` → `shellconfig/options.go` → `schema.json`
  → test" is not interchangeable with any two-file Go edit).
- **abstract signature** — `sha256(core steps, paths→(dir-class,ext),
  args blanked)`. Cross-project *evidence* only: it backs a
  human-flagged-global candidate's origin count. Never a render key.
- **cluster** — turn episodes grouped by the auditable rule:
  identical ordered core, or LCS ≥ `skill_cluster_lcs` (default 0.8)
  of the longer core. Not embeddings — "these turns share steps
  A→B→C" must stay checkable.
- **constants / slots** — across a cluster's aligned cores, values
  all episodes agree on are constants; positions that differ are
  slots (the skill's parameters, mechanically derived).
- **candidate** — a `skills` row in `mined`/`inducted`/`staged`;
  never rendered, never file-backed.
- **skill** (active) — an approved candidate materialized to the
  managed dir, listed in `<available_skills>` with a `mined-` name
  prefix so it can never shadow a hand-written skill.

## Pipeline

Seven stages. Every boundary fails closed: ambiguous input produces
no artifact; a failed check leaves the candidate in place with its
rejection reason recorded.

```
turns ──► [1 mine] ──► core clusters (≥N accepted turn episodes)
clusters ─► [2 induce] ──► SKILL.md draft (small→large consult)
drafts   ─► [3 validate] ──► core bijection + param variance +
                             leak/injection/shell-parse screens
valid    ─► [4 verify] ──► evidence tier always; replay where
                           snapshots exist; shadow tier live
verified ─► [5 stage] ──► staged row; human approval (or eval
                           auto_admit) → active
active   ─► [6 retrieve] ──► materialize → <available_skills> → view
                           → MarkLoaded → skill_usage rows
usage    ─► [7 lifecycle] ──► Beta-posterior deprecate, rate fold,
                              re-induction as new version
```

### 1. Mine — offline, never the hot path

`crush skills mine` is a batch command with an explicit budget:

- **Scan scope:** sessions newer than the mining cursor, `--limit`,
  `--since`; cursor-persisted so re-runs are incremental. A
  `--max-turns` cap bounds the messages scan per run.
- **Turn segmentation:** messages cut at each user-prompt boundary;
  the turn's tool calls form the episode. (Answers v1's open
  question: per-turn, not per-session — whole sessions conflate
  unrelated work and almost never collide exactly.)
- **Filter:** settled `turn_outcomes` = `accepted` only; turns driven
  by `suggested`-marked commands/paths are echo, not evidence.
- **Core extraction** then both signatures; episode rows insert
  idempotent on `(session_id, turn_idx, signature)`.
- **Cluster emission:** cores satisfying the cluster rule across
  `≥ skill_promote_sessions` (default 2) **distinct sessions** emit
  candidate rows in state `mined` — no LLM call, no files, no prompt
  surface.
- **LLM budget:** induction is a separate phase inside `mine` gated
  by `--spend-cap` (max consult tokens per run, shown in `--dry`
  output up front). If the consult provider is unavailable,
  candidates stay `mined` — the state is the retry marker, not an
  error.

### 2. Induce — narrate a skeleton, never invent one

Per candidate, the consult receives: the consensus core (constants
filled, slots marked `{param}`), up to 3 source-episode digests with
differing slot values inlined, and the output contract — steps as
**pointers + commands**, not narrative (ACE). Frontmatter whitelisted
to `name` (the model proposes; we enforce the `mined-` prefix),
`description`, `user-invocable`, `metadata.signature`. Small-model
slot first, large on failure — the #335 attempt-loop shape.

### 3. Validate — the core is the spec

Deterministic, fail-closed:

- `skills.ParseContent` succeeds; name/description bounds; SKILL.md
  validates against the same spec version as hand-written skills
  (test: parse every mined body with the production parser).
- **Core bijection:** every instruction step maps to a consensus
  *core* position and every core position appears — hallucinated or
  truncated procedures reject. Reads are exempt from the bijection:
  a read step may appear as a "consult `X`" pointer **only if every
  source episode made that read** — otherwise the skill must not
  narrate exploration at all.
- **Parameter honesty:** every `{param}` shows ≥2 distinct values
  across episodes; a literal that isn't a consensus constant is
  rejected.
- **Leakage:** token-overlap of the body against stored episode
  *outputs* stays under threshold (a skill embedding one run's
  answer is contamination).
- **Safety:** `redact` secrets pass; no absolute paths outside the
  workspace; fenced commands parse under `mvdan.cc/sh/v3/syntax`;
  destructive commands (`rm`, `git push`, `reset --hard`) don't
  reject but are flagged on the review card.
- **Allowlist content rule:** the body may reference only paths and
  commands observed in the episodes — no hostnames, ticket IDs, or
  prose carrying session specifics (privacy rule, see Operations).

Reject vocabulary (closed): `hallucinated_step`, `truncated_skeleton`,
`consult_not_universal`, `constant_slot`, `literal_leak`,
`output_leak`, `secret_hit`, `bad_frontmatter`, `parse_fail`,
`shell_parse_fail`, `off_allowlist`.

### 4. Verify — three tiers, honest about which applies where

- **Evidence tier (always required):** `≥N` distinct sessions ran
  this core to accepted verdicts — N independent verifications,
  free.
- **Replay tier (evals):** boundary snapshots + `executeReplayRun`
  render vs suppress the skill on a held-out instance; `≥3`
  instances before claiming generalization.
- **Shadow tier (production):** prospective — the next
  signature-matched turn renders the skill **or not** under a
  per-(session,skill) randomized listing (propensity logged on the
  `skill_usage` row). Analysis is intent-to-treat on *listed vs
  hidden*, not on loaded-only — the load decision is the model's,
  post-randomization. Fixes the v1 shadow design's missing
  randomization unit.

Each tier stamps `verify_evidence` (JSON: tier, instances, verdicts,
refs) — "why is this active" is answerable permanently.

### 5. Stage — the human gate is the load-bearing control

`staged` renders nothing, writes no files, and is visible two ways:
`crush skills review` (CLI evidence card: rendered SKILL.md, source
sessions, tier verdicts, destructive-command flags) **and a TUI
surfacing** ("2 candidate skills await review") — CLI-only staging
means candidates sit unreviewed and the gate means nothing ships.
Approval flips `active`; dismissal keeps the row so the signature
never re-induces. This is the only stage crossing a trust boundary:
`admit_scope=global` is set here, by a person — the #229 leakage
decision lands where judgment exists.

**Conventions are not stored here.** A "fact" kind in a procedural
store is a category error; v2 routes discovered conventions as
staged **AGENTS.md patch suggestions** — visible in diff form,
already loaded every session, zero new retrieval surface. The
`skills` table drops `kind` accordingly.

### 6. Retrieve — pull-first, with a pre-registered fallback

V1 was pull-only — but the notebook channel's own data says the
model rarely pulls from memory. v2 keeps pull-first **and pre-
commits the fallback measurement**:

- Active skills materialize to `<data_dir>/skills/<name>/`
  (temp-dir write + atomic rename), joined to `SkillsPaths`, listed
  in `<available_skills>` as source `mined`.
- The model `view`s on description match; `MarkLoaded` writes
  `skill_usage(loaded)`; listing writes `listed` (or `hidden` under
  the shadow holdout) — all `suggested`-marked.
- **Load rate is Phase B's first metric.** Pre-registered fallback:
  if `loaded/listed` over the first measurement window is below
  `skill_load_floor` (default 0.3), the deferred route activates —
  a one-line `<learned_skills>` pointer rendered when the prompt's
  target paths intersect the skill's path set (deterministic match,
  enumerable route, its own ROUTE_SPACE entry, propensity-logged).
  We do not debate pull-vs-push again; the load rate decides.
- Channel control: `memory_procedural` flag is the kill switch —
  off means no mining output renders and the managed dir is removed.
  `skill_render_limit` bounds count (0 suppresses the arm for LOO
  but is *not* the kill switch — usage rows and mining still run,
  which LOO measurement requires).

### 7. Lifecycle — posterior deprecation, never auto-revise

- `skill_usage` ledger: `(skill_id, session_id, event, tool_call_id,
  suggested, propensity, project_key, param_version, created_at)`.
  Events: `listed|hidden|loaded|verify_pass|verify_fail|deprecated|
  revived`.
- **Deprecation via per-skill Beta posterior** (reuses
  `project_rates` fold shape), not `fails=1`: a `verify_fail` counts
  against the skill **only when the failing command is one the skill
  prescribed** — flaky tests and harder tasks are unattributed.
  `skill_deprecate_min` (default 3 attributed fails) + lower-bound
  rule returns the row to `staged` with the trace attached.
- `signal="skill"` aggregate + `skill_rate_floor` render gate: a
  project whose skills keep failing stops listing them.
- **Re-induction:** `≥N` new episodes on a signature queues a new
  `version` with `supersedes`; active rows never mutate in place.

## Data model

```sql
-- Turn-level outcome labels (#340, shipped): settled once at fixed
-- window — newer session provably in the project, or created_at +
-- 24h, whichever first.
CREATE TABLE turn_outcomes (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id   TEXT NOT NULL,
  turn_id      TEXT NOT NULL,               -- the turn's user-message ID
  project_key  TEXT NOT NULL,
  edited_paths TEXT NOT NULL DEFAULT '[]',  -- JSON repo-relpaths
  edit_hunks   TEXT NOT NULL DEFAULT '{}',  -- JSON path→added-lines map
  commands     TEXT NOT NULL DEFAULT '[]',  -- JSON normalized cmds
  next_verdict TEXT NOT NULL DEFAULT '',    -- cue verdict of next prompt
  verdict      TEXT,                        -- NULL=unsettled; settled:
                                            -- accepted|revised|failed|unknown
  settle_at    INTEGER NOT NULL,            -- created_at + 24h cap
  settled_at   INTEGER,
  param_version TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  UNIQUE(session_id, turn_id)
);

-- turn_id is the boundary message's ID, not an ordinal: a
-- slice-relative index shifts between the live post-summary tail
-- and the full-transcript backfill, which would let a compacted
-- session's rows collide under the unique key. Settlement
-- boundaries count only parent_session_id IS NULL sessions — a
-- delegated child is work inside the parent turn, not a new
-- session, and it must not close the window early — and only
-- sessions provably in the row's project: sessions carries no
-- project column, so membership is ledger proof (a project-keyed
-- command_memory / failure_memory / referent_episodes row). The
-- backfill corpus filters identically, so foreign sessions never
-- mint turns under this project's key; a real session→project
-- mapping remains the longer-term answer.
--
-- Two evidence-scope choices, recorded: sessionTestsGreen is
-- session-scoped by design (the ledger has no per-turn last-exit),
-- so a red session end marks every non-empty turn in it failed and
-- outranks commit evidence; and survival is conjunctive over
-- readable fingerprints — a path without a fingerprint abstains
-- rather than vetoing, so acceptance can rest on the paths the
-- turn provably added. Turn boundaries are real user messages:
-- repair prompts and compaction summaries are not, hidden
-- continuations (programmatic runs) are — they drove work like any
-- other prompt.

-- One row per (signature, version) mined candidate.
CREATE TABLE skills (
  id              TEXT PRIMARY KEY,            -- sk_<ulid>
  sig_project     TEXT NOT NULL,               -- sha256, real paths
  sig_abstract    TEXT NOT NULL,               -- sha256, path-stripped
  name            TEXT NOT NULL,               -- enforced mined- prefix
  description     TEXT NOT NULL DEFAULT '',
  body            TEXT NOT NULL DEFAULT '',
  state           TEXT NOT NULL,               -- mined|inducted|staged|
                                               -- active|deprecated|rejected
  reject_reason   TEXT NOT NULL DEFAULT '',
  version         INTEGER NOT NULL DEFAULT 1,
  supersedes      TEXT NOT NULL DEFAULT '',
  origin_projects TEXT NOT NULL DEFAULT '[]',
  induced_by      TEXT NOT NULL DEFAULT '',
  verify_evidence TEXT NOT NULL DEFAULT '{}',
  admit_scope     TEXT NOT NULL DEFAULT 'project',
  project_key     TEXT NOT NULL,
  param_version   TEXT NOT NULL,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  UNIQUE(sig_project, version)
);
CREATE INDEX idx_skills_render ON skills(state, project_key, admit_scope);
CREATE INDEX idx_skills_abs    ON skills(sig_abstract, state);

CREATE TABLE skill_episodes (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  sig_project   TEXT NOT NULL,
  sig_abstract  TEXT NOT NULL,
  session_id    TEXT NOT NULL,
  turn_id       TEXT NOT NULL,    -- turn's user-message ID, not an
                                  -- ordinal (compaction shifts those)
  project_key   TEXT NOT NULL,
  core          TEXT NOT NULL,    -- JSON core steps
  slot_values   TEXT NOT NULL,    -- JSON {slot: [values]}
  verdict       TEXT NOT NULL,    -- accepted|failed|revised|unknown
  suggested     INTEGER NOT NULL DEFAULT 0,
  param_version TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  UNIQUE(session_id, turn_id, sig_project)
);

CREATE TABLE skill_usage (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  skill_id      TEXT NOT NULL,
  session_id    TEXT NOT NULL,
  event         TEXT NOT NULL,    -- listed|hidden|loaded|verify_pass|
                                  -- verify_fail|deprecated|revived
  tool_call_id  TEXT NOT NULL DEFAULT '',
  propensity    REAL NOT NULL DEFAULT 1.0,  -- shadow-tier listing prob
  suggested     INTEGER NOT NULL DEFAULT 0,
  project_key   TEXT NOT NULL,
  param_version TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);
CREATE INDEX idx_skill_usage ON skill_usage(skill_id, event);
```

Plus a `mining_cursor` meta key (scan watermark) and a down
migration dropping all three tables. Provenance pack matches the
#220 contract.

## Command surface

```
crush skills mine [--project P] [--limit N] [--since T]
                  [--max-turns M] [--spend-cap TOK] [--dry]
   — stages 1–4 batch; prints cluster/candidate/reject counts,
     projected consult spend; never touches active state.
crush skills review [--project P]
   — staged candidates with evidence cards + TUI review prompt.
crush skills approve|dismiss|deprecate <name> [--global]
   — the only transitions reaching render.
crush skills list     — active + staged + deprecated, per project.
crush skills purge    — remove all mined artifacts: rows, managed
                        dir, cursor. The uninstall path.
```

`crush stats` surfaces channel observability: counts per state,
reject-reason distribution, load rate (`loaded/listed`),
verify-failure rate, cumulative mining spend.

## Params, telemetry, coverage

| Field | Default | Role |
|---|---|---|
| `memory_procedural` | 0 | channel kill switch — mining output never renders; managed dir removed |
| `skill_promote_sessions` | 2 | distinct-session evidence floor |
| `skill_cluster_lcs` | 0.8 | core-cluster similarity threshold |
| `skill_render_limit` | 3 | materialized active cap (0 = LOO arm, not kill switch) |
| `skill_deprecate_min` | 3 | attributed verify_fails before posterior deprecate |
| `skill_rate_floor` | 0.6 | channel render gate via project_rates |
| `skill_load_floor` | 0.3 | pull-rate floor; below → fallback pointer route activates |
| `skill_shadow_p` | 0.8 | shadow-tier listing propensity |
| `skill_mine_max_turns` | 5000 | scan budget per `mine` run |
| `skill_mine_spend_cap` | 200K tok | consult budget per run, printed in `--dry` |
| `skill_auto_admit` | 0 | eval-only staged→active |

Tail stamps (resolver-discipline vocabulary):

- `tail.skills.active` — count materialized this turn.
- `tail.skills.listed` / `tail.skills.loaded` / `tail.skills.hidden`
  — names.
- Coverage: `min_tail.skills.listed` asserts engagement;
  `max_tail.skills.dead: 0` catches listed-but-never-loaded.
- `tail.decisions` unchanged — selection is the model's
  description-match; `skill_usage` carries what the tail can't.

## Measurement plan

1. **Phase A census (falsification first):** labels + miner over
   nexus and preserved eval DBs. **Ground-truth recall test:** nexus
   already ships hand-written `backend-add-entity` and
   `shared-ts-sync-api` skills — written because agents kept needing
   them. The miner passes iff it rediscovers cores equivalent to
   these. **Precision:** fraction of mined candidates the user
   approves (target ~70%). If equivalent cores don't exist across
   distinct sessions, the channel stops here — cheaply, before any
   render code.
2. **Load rate (Phase B's first metric):** `loaded/listed` on live
   use; under `skill_load_floor` → the pre-registered fallback
   pointer route, not a re-design.
3. **LOO on #227 + sealed pool:** `memory_procedural` arm vs off-arm
   on expensive-discovery cells and the new blind pool (#343) —
   capability effect, out-of-sample.
4. **Placebo + distractor arms:** an irrelevant-but-plausible mined
   skill listed (presence-vs-content, the mask lesson); an arm
   listing only irrelevant skills (distractor lesson). Both gates,
   not footnotes.
5. **Replay A/B + #225 transfer:** held-out instance skill-on vs
   off; skills induced under model A rendered under model B.
6. **Guardrails:** pass-rate no-harm, step/token deltas, verify_fail
   rate, deprecate churn, holdout screens unchanged.

## Operations

- **Spend:** `skill_mine_max_turns` + `skill_mine_spend_cap` bound
  each run; `--dry` prints projected consult tokens before any call.
  Consult-unavailable → candidates stay `mined`, never error.
- **Concurrency:** mining and approval run as single transactions
  against `crush.db` (sqlite serializes with live sessions);
  materialization writes temp dir + rename so a running session
  never sees a half-written skill; sessions pick up newly active
  skills at next discovery refresh.
- **Names:** `mined-` prefix enforced at induction; hand-written
  skills always win name collisions (discovery already prefers
  earlier `SkillsPaths`; mined joins last).
- **Uninstall:** `crush skills purge` (rows + managed dir + cursor);
  `memory_procedural: 0` kill switch; down migration; purge ≠
  `render_limit: 0` (that still mines and logs usage).
- **Privacy:** mined bodies pass a literal allowlist — paths and
  commands observed in episodes only, session specifics rejected
  (`off_allowlist`); skills never leave the project's data dir; no
  mem0/external sync of `skills`/`skill_episodes` content.
- **Worktrees:** linked worktrees share `project_key` but have
  separate `.crush` dirs — each worktree mines, approves, and
  materializes from its own db. Same signature may stage twice
  across worktrees; that's per-worktree evidence, not a bug.
  Cross-worktree pooling would need a shared store — open question.
- **Format compatibility:** mined SKILL.md parsed by the production
  parser in tests; spec-version stamped on the row.

## Production gates

| Gate | Pass criterion |
|---|---|
| G1 Data | `turn_outcomes` labels settle correctly (window, hunk survival, agent-commit exclusion) and populate on real nexus sessions |
| G2 Offline quality | Census rediscovers equivalents of `backend-add-entity` + `shared-ts-sync-api`; ~70% of candidates approvable |
| G3 Safety | Tests cover injection, secrets, output leak, destructive-command flags, `scripts/` rejection (managed dir is SKILL.md-only), hand-written collision, cross-project render blocked by default |
| G4 Benefit | Load rate measured then met (or fallback route engaged per pre-registration); LOO + sealed pool show step/pass gains with no pass harm; placebo + distractor arms run |
| G5 Operations | Spend caps, atomic materialization, kill switch + purge + down migration, `crush stats`, TUI review prompt — all live |
| G6 Dogfood | ≥2 weeks real nexus use, telemetry showing loads and outcomes |

## Phasing

- **A — labels + miner + census (offline-only):** `turn_outcomes`,
  tables, miner CLI, induction contract, validators, screens.
  Nothing renders. Scored against nexus's two hand-written skills —
  the first deliverable is whether this channel has anything to
  learn at all.
- **B — retrieval + lifecycle:** materialization, `admit_scope`,
  usage ledger + shadow holdout, posterior deprecate, TUI review.
  First metric: load rate.
- **C — measure:** LOO + sealed pool, placebo/distractor, replay
  A/B, transfer. Unmeasurable → dormant (`memory_procedural: 0`
  default), pipeline stays as instrumentation.

## Open questions

- Cross-worktree pooling: separate dbs mean `project_key`-equal
  stores don't actually share rows — a shared location (or explicit
  export) is a later decision; v1 accepts per-worktree evidence.
- `skill_usage` verdict backlink vs lazy session_id join (v1 lazy;
  denormalized column is a migration away if analytics need it).
- Fallback pointer route's prompt-matcher: path-intersection is the
  v1 rule; anything semantic re-enters route-space design.

## Route-space checklist (for the record)

- [x] Enumerable path: seven stages and their exits, above.
- [x] Closed vocabulary: states (6), reject_reasons (11), usage
      events (7), verdicts (4), tail stamps (4) — all listed.
- [x] Decision records: `verify_evidence` cards + usage rows +
      propensities + `tail.skills.*`.
- [x] Offline benchmark: Phase A census scored against known-good
      hand-written skills — falsifiable before render exists.
- [x] Learned params: eleven slots in `params.Memory`, versioned.
- [x] Topology flag: new channel = mechanism change; this doc is the
      proposal stage of the evolution loop.
