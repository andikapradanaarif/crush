package cmdlog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/charmbracelet/crush/internal/db"
)

// Learning layer B (#296): per-project Beta-Binomial rates on the
// referent channel's acceptance evidence. The stored (alpha, beta)
// pair is DECAYED posterior mass — each new evidence-bearing session
// discounts the accumulated pair by gamma before its outcomes fold
// in (Garivier & Moulines, ALT 2011: discounting tracks a rate that
// drifts). The global prior is the pooled outcome evidence across
// every project — empirical-Bayes shrinkage that keeps a sparse
// project's posterior at the global rate automatically. Once #229's
// user partition exists, the same pooling generalizes to the user
// level.

// rateSignalReferent names the referent-acceptance rate in
// project_rates. Other signals (failure usefulness, command
// resolution) can join the table under their own keys.
const rateSignalReferent = "referent"

// rateEventBatch bounds the fold's read per pass; the cursor makes
// leftovers a next-pass job, same drain contract as the label
// maturity pass.
const rateEventBatch = 64

// referentEpisodeOutcome maps one settled episode onto +1 success or
// +1 failure of referent acceptance — the single definition both the
// fold and the prior pool fold over. Artifact reads dominate: a
// landed commit is acceptance regardless of the other signals (the
// project took the work); hash divergence without a commit is the
// failure; a surviving hash is the success; only with no artifact
// baseline does the cue verdict carry the call.
func referentEpisodeOutcome(verdict string, committed, hashChanged sql.NullInt64) (success bool) {
	if committed.Valid && committed.Int64 == 1 {
		return true
	}
	if hashChanged.Valid {
		return hashChanged.Int64 == 0
	}
	return verdict == ReferentAccepted
}

// UpdateProjectRates folds settled referent episodes into the
// project's decayed posterior mass and marks them folded.
// Called from the detached per-turn pass — idempotent on the
// rate_folded mark: a sibling pass racing between this pass's
// upsert and its mark can re-fold one batch at most. gamma is the
// per-session discount; gamma <= 0 or >= 1 is rejected so a config
// slip can't silently freeze (gamma=1) or zero (gamma=0) memory.
func (s *service) UpdateProjectRates(ctx context.Context, gamma float64) error {
	if gamma <= 0 || gamma >= 1 {
		return fmt.Errorf("rate decay gamma out of range (0,1): %v", gamma)
	}
	row, err := s.q.GetProjectRate(ctx, db.GetProjectRateParams{
		ProjectKey: s.projectKey,
		Signal:     rateSignalReferent,
	})
	alpha, beta, cursor, lastSession := 0.0, 0.0, int64(0), ""
	switch {
	case err == nil:
		alpha, beta, cursor, lastSession = row.Alpha, row.Beta, row.LastEventID, row.LastSessionID
	case errors.Is(err, sql.ErrNoRows):
	default:
		return fmt.Errorf("read project rate: %w", err)
	}
	events, err := s.q.ListReferentRateEvents(ctx, db.ListReferentRateEventsParams{
		ProjectKey: s.projectKey,
		Limit:      rateEventBatch,
	})
	if err != nil {
		return fmt.Errorf("list rate events: %w", err)
	}
	if len(events) == 0 {
		return nil
	}
	// Group by session in first-appearance order so decay lands once
	// per distinct session — interleaved rows from concurrent
	// sessions must not coin extra boundaries. A session spanning
	// two fold calls re-matches lastSession and folds undiscounted
	// the second time: it is still one session. (A session
	// re-entering after ANOTHER session interleaved across three
	// batches discounts twice — bounded over-discount the
	// one-slot lastSession accepts rather than tracking a set.)
	sessions := map[string][]db.ListReferentRateEventsRow{}
	var order []string
	for _, ev := range events {
		if _, ok := sessions[ev.SessionID]; !ok {
			order = append(order, ev.SessionID)
		}
		sessions[ev.SessionID] = append(sessions[ev.SessionID], ev)
	}
	var maxID int64
	ids := make([]int64, 0, len(events))
	for _, sid := range order {
		// Discount once per evidence-bearing session boundary:
		// sessions are the evidence unit, not episodes. Sessions
		// with no settled episodes never appear in the stream —
		// they can't decay because the ledger offers no way to
		// enumerate them per project.
		if sid != lastSession {
			alpha *= gamma
			beta *= gamma
			lastSession = sid
		}
		for _, ev := range sessions[sid] {
			// The outcome is read AT fold time — a cue-settled row
			// whose labels later diverge keeps its folded verdict;
			// the mark is one-time, so the evidence mix depends on
			// how far maturity drained before this pass ran.
			if referentEpisodeOutcome(ev.Verdict, ev.LabelCommitted, ev.LabelHashChanged) {
				alpha++
			} else {
				beta++
			}
			ids = append(ids, ev.ID)
			if ev.ID > maxID {
				maxID = ev.ID
			}
		}
	}
	if maxID > cursor {
		cursor = maxID
	}
	if err := s.q.UpsertProjectRate(ctx, db.UpsertProjectRateParams{
		ProjectKey:    s.projectKey,
		Signal:        rateSignalReferent,
		Alpha:         alpha,
		Beta:          beta,
		LastEventID:   cursor,
		LastSessionID: lastSession,
		UpdatedAt:     s.now().Unix(),
	}); err != nil {
		return fmt.Errorf("upsert project rate: %w", err)
	}
	// Membership marks land AFTER the upsert: a crash between them
	// re-folds this batch (a bounded double-count), while marking
	// first would lose the evidence permanently.
	return s.q.MarkReferentEventsFolded(ctx, ids)
}

// ReferentRateLowerBound returns the quantile-q lower bound of this
// project's referent-acceptance posterior: the Beta over the decayed
// project mass shrunk onto the global prior. Zero local evidence
// returns the prior's own bound — the shrinkage is automatic, not a
// special case. An error means the store or the math failed; callers
// decide under uncertainty (the gate fails to incumbent behavior).
func (s *service) ReferentRateLowerBound(ctx context.Context, q float64) (float64, error) {
	a0, b0, err := s.referentPrior(ctx)
	if err != nil {
		return 0, err
	}
	row, err := s.q.GetProjectRate(ctx, db.GetProjectRateParams{
		ProjectKey: s.projectKey,
		Signal:     rateSignalReferent,
	})
	switch {
	case err == nil:
		a0 += row.Alpha
		b0 += row.Beta
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("read project rate: %w", err)
	}
	return BetaQuantile(a0, b0, q), nil
}

// referentPrior fits the global Beta(a0, b0) over pooled settled
// episodes, LEAVE-ONE-OUT: the querying project's own rows are
// excluded — counting them in the prior and again as local mass
// roughly doubles their weight, which is least conservative exactly
// where data is scarcest (the single-project store). A corpus with
// no other-project evidence yields Beta(1, 1) — uniform, maximally
// uncertain — rather than fabricating optimism.
//
// The pool is an unbounded cross-project scan per call; it runs on
// the gated render path only, and a `referent-global` rates row or
// cached prior is the scaling seam if a shared store outgrows it.
func (s *service) referentPrior(ctx context.Context) (a, b float64, err error) {
	rows, err := s.q.ListReferentPriorEvents(ctx, s.projectKey)
	if err != nil {
		return 0, 0, fmt.Errorf("list prior events: %w", err)
	}
	a, b = 1, 1
	for _, r := range rows {
		if referentEpisodeOutcome(r.Verdict, r.LabelCommitted, r.LabelHashChanged) {
			a++
		} else {
			b++
		}
	}
	return a, b, nil
}
