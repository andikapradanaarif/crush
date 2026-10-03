# 2026-10-04 — Roadmap: fixed skeleton, learned variables

**Supersedes
[2026-10-01-roadmap-after-mask](2026-10-01-roadmap-after-mask.md) as the
plan of record.** The prior roadmap's evidence corrections and its Stage
A–E work items remain the execution base and are not re-litigated here.
What changed is the *architecture*: the harness is not a fixed mechanism
that remembers, and not a learned router. It is a fixed safety skeleton
whose variables learn per project, with a slow user-level tier above it.

## Why a new roadmap

Three forces converged:

1. **Multi-project reality.** One installation maintains several
   projects. Project memory must not leak across repositories; user
   knowledge should transfer. The 10-01 plan never distinguished the
   tiers.
2. **The adaptivity question.** "Smarter every time" cannot mean a
   learned router — that forfeits per-decision auditability, offline
   benchmarkability, and fail-closed safety. It also cannot mean a
   frozen harness — that forfeits per-project fit. The resolution is a
   fixed skeleton with learned variables inside it.
3. **Literature anchors.** The agent-harness survey (ETCLOVG taxonomy)
   and HarnessX (arXiv:2606.14249) sharpen the position. Both treat the
   trace as the primary object; neither argues for learned safety
   ordering. Details below.

## The four tiers

| Tier | Fixed/learned | Contents | Store |
|---|---|---|---|
| Global skeleton | **Fixed** | Route shape, safety ordering, veto precedence, abstention, verdict taxonomy, partition rule, provenance + measurement contracts | Code |
| User level | **Slow learned** | Preferences, correction style, parameter *priors* | User-level store, screened promotion only |
| Project level | **Fast learned** | Memory rows + learned parameter *values* | `crush.db` (per project) |
| Session | Ephemeral | Working set, artifacts | Session state |

Merge rule: a cold project initializes from user-level priors (which
initialize from global defaults); project evidence overrides the prior
as it accumulates — per-project posterior, not shared mutation. Evidence
never flows back to user level without screened promotion.

## What the skeleton owns (unlearnable)

- **Safety ordering** — veto before inference; abstention is always a
  valid outcome; fail-closed on unsupported language/unparseable input.
- **Verdict taxonomy** — the closed vocabulary of decision reasons and
  outcome classes. Fixed vocabulary is what makes `tail.decisions`
  comparable across runs.
- **Project partition** — `project_key(candidate) = project_key(current)`
  is an admissibility clause decided *before* relevance logic: a join
  condition, not a ranking signal. Leakage is a measurable, veto-class
  failure.
- **Provenance** — every recorded decision carries session, tool call,
  repo state, and the parameter version that produced it.
- **Measurement contracts** — paired evidence, coverage contract, gate
  alarms, sealed pools. Telemetry may retire a *mechanism*; it may not
  relax any of the above.

The enumerated route space — every decision path's checks, exits, and
guards, as a diffable document — is `docs/design/ROUTE_SPACE.md`.

## What learns (per-project variables, user-level priors)

| Variable | Evidence source | Issue |
|---|---|---|
| Selector thresholds, admit margins | Binding benchmark + #206 telemetry | #228 |
| TTL / staleness windows | Resolution outcomes | #228 |
| Referent promotion counts, phrase→referent maps | #165 acceptance events | #165, #228 |
| Artifact-recency windows (L2) | Binding benchmark | #216, #228 |
| File-heat weights | Attribution-corrected reads | #228 |

All of it hangs off two new items:

- **#228 learned-params substrate** — the `crush.db` store: name, value,
  provenance, version, last-verified, evidence window. Learned state
  never lives in user config (config is authored intent; learned params
  are harness state). Decay is required — params without decay become
  another stale-memory channel.
- **#229 user-level memory** — the slow tier: what transfers
  (preferences, correction style) vs what never does (code/work memory).
  Promotion requires evidence across ≥2 projects plus a contamination
  screen. Global defaults must be safe alone — priors only nudge, so a
  fresh install degrades gracefully.

## Literature anchors

| Source | What transfers | What doesn't |
|---|---|---|
| **Survey** (ETCLOVG) | The layer map — this fork builds in its thinnest layers (context/memory: 9 projects vs lifecycle: 47; observability + governance mostly commercial). Open problems #2/#3/#5 ≈ our #220 provenance/staleness, trace-native diagnosis, #198 re-verification | — |
| **RRSI** | The fixed *evolution* loop — attributable edits, noise-adjusted acceptance, cost-justified gain, periodic pruning. It governs which mechanisms stay installed; it says nothing about per-turn routing | The "converged" reading (Table 6 = four hand-picked decisions); the circular cost rule (#197, already corrected) |
| **HarnessX** (arXiv:2606.14249) | Typed primitives + substitution over a slot schema — the honest ceiling if slot *contents* ever need structural evolution. "Gains largest where baselines lowest" (+14.5% avg, up to +44%) confirms mechanism value is conditional on baseline gaps | Structural rewriting of the skeleton itself — that is un-auditable learned routing, worse in multi-project where it can smuggle cross-context associations no deterministic check catches. Trajectory→model-training loop is out of scope |

The convergence worth stating: all three treat **the trace as the
primary object**. Our preserved session DBs, `AnalyzeSessionDB`, and
`tail.decisions` are exactly the infrastructure a principled evolution
loop needs — per-decision attribution is what separates evolution from
blind search.

## What changes vs the 10-01 plan

**Unchanged (execution base — see the prior doc for detail):**

- Stage A trust items: #206 telemetry (+holdout/snapshot), #151, #152,
  #138, #109, #115-split, planning-MDE vs shipping-MDE
- Stage B selectivity: #216 layered resolver, #218 reconciliation edge,
  #219 injection screening, #220 provenance, #221 observability
- Stage C: #222 capacity → #220 → #221 → #223 ladders
- Stage D dispositions; Stage E: #224 sealed pool, #225 qwen, #226
  SWE-bench, #227 sub-ceiling corpus

**New or re-scoped:**

- **#228** learned-params substrate — new; first tenants are the
  selector/TTL/referent variables above.
- **#229** user-level memory — new; Stage D-class item, gated on
  provenance + multi-project evidence actually existing.
- **#220** amended (10-04): `project_key` + `param_version` join the
  provenance schema — cheap (columns, not a feature) but load-bearing
  for everything above.
- **Evolution skeleton, named** — #197 acceptance + #198
  re-verification constitute a *second* fixed skeleton governing the
  mechanism layer (which components stay installed). Runtime skeleton
  governs per-turn decisions; evolution skeleton governs the component
  set. Both fixed; the variables inside both learn.

## Next steps, in order

1. **Notebook default-off (#205)** — still the only item affecting
   every user today; unchanged.
2. **Write-side injection screening (#219)** — memory is a persistent
   prompt-injection channel; security item, cheap.
3. **Real-usage telemetry (#206)** — opt-in logging + randomized
   on/off holdout + session-start snapshots. Lead time is still the
   cost, and #228's learning needs the data.
4. **Trustworthy CI (#138)** — selector PRs went through five review
   rounds with no automated gating.
5. **Merge #217** (stale-seed gate, open) → powered selector run on
   the repaired mask corpus (bar: pass ~1.00 *and* effort savings
   retained).
6. **Reconciliation edge (#218)** — test on the same corpus.
7. **Layered resolver (#216)** — L0–L1 + offline binding benchmark;
   L2 artifacts; L3 only if deterministic layers leave measurable
   recall unclaimed.
8. **#222 capacity → #220 provenance (+project_key/param_version) →
   #221 → #228 params substrate → #223 ladders.**
9. **#224 sealed pool + #225 qwen + #226 SWE-bench + #227
   sub-ceiling corpus** — evidence pools and OOD checks.
10. **#229 user-level memory** — Stage D; starts earning promotion
    only once ≥2 projects of screened evidence exist.

## Standing risks

All prior risks stand (measurement-before-mechanism, abstention
invisibility, self-reinforcing heat, corpus ceiling, absolute
magnitude). Three new:

- **Cross-project leakage** — a foreign-project row in the prompt is a
  harm class *worse* than stale memory: it anchors a referent that can
  never bind. The partition clause is the mitigation; leakage detection
  must be a scored benchmark metric, not a hope.
- **Parameter staleness** — learned params without decay are the next
  stale-memory channel, one layer down. Repo-shape change must erode
  evidence support automatically.
- **Prior poisoning** — user-level memory is a persistent channel into
  *every* project. The ≥2-project + contamination-screen promotion bar
  is the mitigation; the failure mode to watch is a single eccentric
  project minting a global prior.
