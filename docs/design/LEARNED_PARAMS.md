# Learned Parameters — Contract and Substrate

> **Status:** Substrate shipped (#228). The parameter set exists as a
> typed, versioned, overridable value (`internal/params`); the
> `learned_params` table and the promotion/decay machinery land with
> the first real tenant, not speculatively.
> **Ship when:** the store lands when a parameter demonstrably earns
> a per-project value — see the tuning ladder.
> **Measured by:** `param_version` on every memory row and telemetry
> record; eval cohorts split by resolved snapshot.

## Goal

Make the memory subsystem's numeric knobs *addressable* — one typed
set, one resolved snapshot, one stamped version — so that:

- an eval arm can vary them without touching code (#223's depth
  ladders are the first consumer),
- every decision record attributes to the parameter set that
  produced it (#220's `param_version` column),
- and a future learned value plugs into the same resolution seam
  instead of growing a parallel mechanism.

## The parameter set

`params.Memory` — consumed by the failure-memory selector and the
turn-context renderer; resolved once at `app.New`, passed to both
`cmdlog.NewService` (TTL + `param_version` stamping) and the
coordinator (selector behavior). One snapshot, two consumers — the
version a row carries and the behavior that produced it cannot
drift apart.

| Key | Default | Bound | What it bounds |
|---|---|---|---|
| `working_set_limit` | 10 | ≥ 0 | Working-set lines rendered per turn |
| `vague_prompt_max_words` | 12 | ≥ 0 | Vagueness pre-filter prompt length |
| `intent_max_bytes` | 4096 | ≥ 0 | Rendered intent record size |
| `file_heat_limit` | 5 | ≥ 0 | Cross-session heat hint lines |
| `fetch_limit` | 50 | ≥ 0 | Selector candidate pool, all three pools |
| `open_render_limit` | 5 | ≥ 0 | Open-failure tail rows |
| `resolved_render_limit` | 3 | ≥ 0 | Resolved-knowledge tail rows |
| `command_render_limit` | 3 | ≥ 0 | Command-knowledge tail rows |
| `failure_file_hints` | 3 | ≥ 0 | File hints per failure row |
| `failure_cmd_runes` | 200 | ≥ 0 | Command text echoed per row |
| `failure_headline_runes` | 140 | ≥ 0 | Headline text echoed per row |
| `open_failure_ttl` | 720h | 0 or [24h, 720h] | Open-failure staleness window |

Zero on a cap means "suppress the section" — the tighten
direction, always allowed. `open_failure_ttl` accepts a Go
duration string (`"168h"`) or a number of seconds in the overlay.

## Versioning

`Memory.Version()` = `pv1-<sha256[:8]>` of the canonical JSON
snapshot — field additions rotate the hash automatically, which is
correct: a new knob is a new snapshot semantics. `pv0` remains the
marker on rows written before the substrate landed: same effective
values, but written by a harness with no parameter mechanism.

## The override channel (what exists now)

`options.memory_params` is a strict overlay onto
`params.DefaultMemory()`:

```json
{ "options": { "memory_params": { "open_render_limit": 8, "open_failure_ttl": "168h" } } }
```

- Unknown keys, non-integral values, and out-of-bounds values are
  hard errors at `app.New` — an arm or user that declares a
  parameter must never silently run different parameters.
- Eval arms set it like any option; it is declared in
  `eval/flags.json` so `ValidateArmFlags` admits it and baseline
  keys rotate on it.
- This is authored intent. Learned values travel the store below —
  the same struct, populated from `learned_params` instead of
  config — never the other direction: nothing learned lands in
  `crushrc`/`crush.json`, which are user-authored and committed.

## The store (deferred, per contract)

Build when the first parameter earns a per-project value:

```sql
learned_params (
    project_key    TEXT NOT NULL,     -- cmdlog partition identity
    name           TEXT NOT NULL,     -- params.Memory JSON key
    value          TEXT NOT NULL,     -- JSON-encoded value
    provenance     TEXT NOT NULL,     -- evidence that established it
    version        INTEGER NOT NULL,  -- per-row update sequence
    last_verified  INTEGER NOT NULL,  -- UnixMilli of last supporting evidence
    evidence_window TEXT NOT NULL,    -- evaluation window that produced it
    PRIMARY KEY (project_key, name)
)
```

Cold projects initialize from user-level priors (#229), which
initialize from `DefaultMemory()`. Project evidence overrides the
prior as it accumulates — a hierarchical shrinkage estimate:
insufficient per-project evidence leaves the posterior at the
prior, not halfway to noise. Evidence never flows back to user
level without #229's screened promotion path.

## Tuning ladder (in order)

1. **Global, offline** — tune via the binding benchmark (#216) plus
   #206 telemetry pooled across projects. Most parameters never
   need per-project values.
2. **Per-project promotion** — a parameter earns a project value
   only when the data shows both (a) it actually differs between
   projects and (b) it receives enough events per evaluation window
   to move under the acceptance rule. The minimum-events threshold
   is set per parameter at promotion time, not tuned per case.
3. A parameter update is itself an evidence decision — updates
   travel the same windowed, noise-adjusted acceptance rule as
   mechanism changes, never single-turn reactions; otherwise tuning
   becomes adaptive evaluation over the corpus, the channel #224's
   sealed pool exists to control.

## Asymmetry

Learned params may **tighten freely** — more abstention, shorter
TTLs, smaller envelopes. **Relaxing** requires the full evolution
loop including the sealed pool (#224) and stays inside
skeleton-defined bounds (`open_failure_ttl` ∈ [24h, 720h] carries
the contract; other bounds arrive with their promotions). Lexicon
learning adds veto words only, never grant words. `Validate()`
enforces the stated bounds on every resolution path.

## Decay

Decay is required, not optional — learned params without decay are
the next stale-memory channel, one layer down. A parameter whose
supporting evidence stops arriving (repo shape changed, decisions
under it keep getting vetoed) demotes back toward the prior. The
store's `last_verified`/`evidence_window` columns exist for this;
the demotion rule lands with the store.

## First tenants (when real parameters exist)

Selector thresholds (#216), TTL/staleness windows, referent
promotion counts (#165), artifact-recency windows (L2), file-heat
weights — each promoted per the ladder, not installed
speculatively.

## Explicit non-goals (this PR)

- No `learned_params` table — no tenant exists to read it.
- No promotion/decay machinery — no evidence source in-harness yet.
- No per-parameter skeleton bounds beyond the TTL — bounds arrive
  with each promotion contract.
