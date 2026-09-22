# Edit Result State — Make Writes Visible to the Writer

> **Status:** Proposal. A response-shape change to `edit`/`multiedit`:
> return the post-edit region in the tool result the model sees,
> instead of a blind confirmation. Motivated by the prior-turn collapse
> evaluations (#90, #95), whose audits showed the dominant context waste
> is not old-turn bulk but post-edit re-reads: the model verifies edits
> it cannot see.
>
> **Composes with:** `TURN_DIGEST.md` (this is the alternative to
> in-window collapse — add state per call rather than remove context
> per turn), `CONTEXT_NOTEBOOK.md` (unchanged), `EVAL_HARNESS.md`
> (validation is one paired run against the existing corpus).

## Evidence — all measured, `staged-pipeline-build` artifacts

Across 6 runs of `prior-turns-long-summarize` (3 verbatim + 3
summarize) plus the earlier stub run, session DBs show:

| Metric | Control (verbatim) | Treatment (summarize) |
|---|---|---|
| Steps | 81, 91, 102 → 91.3 | 101, 105, 119 → 108.3 |
| Tool calls | 66–86 | 106–129 |
| File reads | 10–23 | 29–38 |
| **Post-edit reads** | **8–20 (72–86% of reads)** | **23–34 (79–89%)** |
| Edits | 31–35 | 28–34 |

Two facts make the pattern a mechanism, not a correlation:

1. **Edits are blind.** A successful `edit` returns
   `"Content replaced in file: <path>"` (edit.go:449) — a bare
   confirmation. The diff it already computes goes only to
   `EditResponseMetadata` for the permission/UI layer
   (`WithResponseMetadata`, edit.go:425-429); the model never sees it.
   To learn what the file looks like now, the model must call `view`.

2. **The reads are not protocol-forced.** `commitFileChange` records
   the read timestamp after every write (`RecordRead`, edit.go:269),
   so the read-before-write gate (edit.go:292-302) does not re-fire on
   the model's own chained edits. The 80–89% post-edit read rate is
   the model *choosing* to verify a change it was not shown.

The collapse experiments measured the cost of the inverse design:
removing state cost +19% steps (summarize) to +62% (stub). Keeping
state in-band is the cheaper direction.

## Hypothesis

**If `edit`/`multiedit` results carry the post-edit region, most
post-edit reads disappear — the information arrives with the write.**

Falsifiable prediction on `staged-pipeline-build`, verbatim arm:

- Post-edit reads drop from ~80% of reads to < 20%.
- Steps drop 5–15% (each dedicated re-read ≈ 1 step; 8–20 reads/run
  are candidates).
- Output tokens drop 10–20% (fewer verify round-trips).

If post-edit reads do NOT drop, the model verifies for a reason the
region doesn't satisfy (e.g., it wants wider context than the diff) —
a mechanism-level finding, not a tuning bug.

## Spec

- **Response text**: on successful `edit`/`multiedit`, append the
  change rendered as a bounded region — the post-edit lines covering
  the replaced span plus up to ~10 lines of surrounding context,
  capped at ~50 lines total. Shape matches what `view` would have
  shown, so nothing new for the model to learn to read.
- **`write` excluded**: the model authored the full content; the
  merged-result question doesn't arise. (Create path unchanged.)
- **Failures unchanged**: `notFoundError` already embeds
  `currentRegionContext` (edit.go:238) — this change extends that
  same affordance to the success path.
- **Read-before-write gate unchanged**: `modTime.After(lastRead)`
  still detects external modification; the response region is a
  convenience, not a trust substitute.
- **Bounded**: a whole-file rewrite falls back to head+tail or the
  diff stat — the region must never approach `view` of a large file
  in size. Cap ~50 lines (~1–2K tokens worst case) keeps the added
  payload smaller than the read it replaces (~2K+ tokens plus a
  round-trip).

## Targets — pre-registered, judged vs the same trajectory

n=3/arm, verbatim config, `staged-pipeline-build`, same provider and
temperature as the prior-turns runs:

| Metric | Success | Inconclusive | Failure |
|---|---|---|---|
| Post-edit reads | < 20% of reads | 20–50% | ≥ 50% |
| Steps | ≥ 5% below control | ±5% | > control |
| Output tokens | ≥ 10% below control | ±10% | > +10% |
| Wall time | ≥ 5% below control | ±5% | > control |
| Check pass rate | = control | — | < control |
| Provider/render errors | 0 new classes | — | any |

**Decision rule:**

- **Win** (reads + steps in success band) → ship unconditionally;
  re-measure the collapse question later — a cheaper transcript makes
  in-window collapse a different experiment.
- **Fail** → revert is one commit; the response shape returns to
  blind confirmation. No machinery is left behind — this is a string
  in a response, not a subsystem.
- **Reads drop but steps don't** → the reads were informational
  overhead the model parallelized; keep or revert on the token delta
  alone (still cheaper than the reads, but marginal).

## Audit plan

1. **Transcript audit** (2+ runs): edit results contain the region;
   region is bounded; a whole-file edit does not dump the file.
2. **Read audit**: classify every `view` call — post-edit on a path
   the agent wrote vs. exploration of untouched paths. The prediction
   is specifically about the former class.
3. **Protocol audit**: no new provider errors; region content is the
   same bytes `view` would emit (no fabricated state).
4. **Regression audit**: `edit` error paths (not-found,
   multiple-match, permission denied) unchanged.

## Action plan

1. `edit.go` + `multiedit.go`: on success, build the bounded region
   from `result` (already computed) and append to the response text
   (~40 lines).
2. Region builder helper + cap logic (~40 lines) + tests (~80 lines):
   region present, bounded, correct content, large-file fallback.
3. One paired eval run → fill Targets table → audit.
4. Verdict → ship or revert.

## Scope guardrails

Does **not**: change the read-before-write gate, touch edit matching
or whitespace fallback, add an option, or modify `write`. Not a
context-management feature — a tool-response completeness fix that
happens to have context-management effects.

## Known unknowns

- Region size vs. model need: if models verify to see *surrounding*
  logic rather than the change itself, a bounded region may still
  force reads. The audit distinguishes "read to see the change" (fixable)
  from "read to see the file" (not).
- Some post-edit reads are deliberate re-derivation (checking imports,
  unrelated regions). The <20% target assumes most aren't; if the
  floor lands at 30–40%, the residual is the real verification need
  and the win shrinks accordingly.
- Effect size depends on edit density; this corpus is edit-heavy
  (~30 edits/run), so the measured band is near the ceiling of what
  the mechanism can do — a prose-heavy workload would show less.

Refs #90 (economics data), #95 (collapse eval whose audit surfaced
this).
