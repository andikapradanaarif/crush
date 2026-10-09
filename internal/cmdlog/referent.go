package cmdlog

import (
	"context"
	"time"

	"github.com/charmbracelet/crush/internal/db"
)

// Referent verdicts — the next user turn's judgment of the edit a
// vague prompt produced (#165).
const (
	ReferentAccepted = "accepted"
	ReferentRevised  = "revised"
)

// ReferentEpisode is one judged observation: the user asked with a
// referent-leaning phrase, the agent resolved it to an edit of
// Target, and the following user turn carried no revision signal (or
// did — verdict records which). Provenance fields follow #220's
// per-observation contract: the session and mutating call that
// produced the observation, the repo state it was true of, and the
// contamination flag for edits the memory tail itself suggested.
type ReferentEpisode struct {
	Phrase          string // normalized referent phrase from the vague prompt
	Target          string // workspace-relative path the agent edited
	SessionID       string
	SourceMessageID string // the vague user message — idempotency key
	ToolCallID      string // the mutating call that produced the edit
	RepoState       string
	Suggested       bool   // the rendered tail itself named this target
	Verdict         string // ReferentAccepted or ReferentRevised
}

// Referent is a promoted phrase→target candidate — enough clean
// acceptances to render as a hint ("usually means X"), never a fact.
type Referent struct {
	Phrase       string
	Target       string
	Hits         int64 // clean accepted evidence count
	LastAt       time.Time
	ProjectKey   string
	ParamVersion string
}

// fileSuggestedKey namespaces the suggested map for file targets —
// referent renders mark paths, command renders mark cmd_norms, and
// neither must collide with the other's key space.
func fileSuggestedKey(path string) string { return "file:" + path }

// MarkSuggestedFile records that the session was shown path as a
// rendered referent target this turn — same contamination-screen
// contract as MarkSuggested, for the file channel (#165): an edit
// landing on a file the tail itself named is echo, not evidence.
func (s *service) MarkSuggestedFile(sessionID, path string) {
	if sessionID == "" || path == "" {
		return
	}
	s.MarkSuggested(sessionID, fileSuggestedKey(path))
}

// WasSuggestedFile reports whether this session — or its parent, for
// delegated runs — was shown path in a rendered referent section.
// Same contamination-screen semantics as wasSuggested: errs inclusive.
func (s *service) WasSuggestedFile(ctx context.Context, sessionID, path string) bool {
	if sessionID == "" || path == "" {
		return false
	}
	return s.wasSuggested(ctx, sessionID, fileSuggestedKey(path))
}

// RecordReferentEpisode stores one judged observation and runs the
// promotion check: when an accepted, non-suggested episode brings the
// (phrase, target) mapping's distinct-session acceptance count to
// promoteMin, the candidate earns its referent_memory row — and every
// further clean acceptance keeps scoring it. Episodes insert under
// their source-message idempotency key, so a repair-chain re-emit is
// a no-op and cannot double-count.
func (s *service) RecordReferentEpisode(ctx context.Context, ep ReferentEpisode, promoteMin int) error {
	if ep.Phrase == "" || ep.Target == "" || ep.SessionID == "" || ep.SourceMessageID == "" {
		return nil
	}
	if ep.Verdict != ReferentAccepted && ep.Verdict != ReferentRevised {
		return nil
	}
	suggested := int64(0)
	if ep.Suggested {
		suggested = 1
	}
	now := s.now().UnixMilli()
	if err := s.q.InsertReferentEpisode(ctx, db.InsertReferentEpisodeParams{
		Phrase:          ep.Phrase,
		Target:          ep.Target,
		SessionID:       ep.SessionID,
		SourceMessageID: ep.SourceMessageID,
		ToolCallID:      ep.ToolCallID,
		RepoState:       ep.RepoState,
		MemorySuggested: suggested,
		Verdict:         ep.Verdict,
		ProjectKey:      s.projectKey,
		ParamVersion:    s.paramVersion,
		CreatedAt:       now,
	}); err != nil {
		return err
	}
	if ep.Verdict != ReferentAccepted || ep.Suggested || promoteMin <= 0 {
		return nil
	}
	acceptances, err := s.q.CountCleanReferentAcceptances(ctx, db.CountCleanReferentAcceptancesParams{
		ProjectKey: s.projectKey,
		Phrase:     ep.Phrase,
		Target:     ep.Target,
	})
	if err != nil {
		return err
	}
	if acceptances < int64(promoteMin) {
		return nil
	}
	return s.q.PromoteReferent(ctx, db.PromoteReferentParams{
		Phrase: ep.Phrase,
		Target: ep.Target,
		// The row is born with the full clean-acceptance count that
		// crossed the floor — seeding 1 would render "accepted 1×"
		// while promoteMin sessions of evidence exist.
		Hits:         acceptances,
		LastAt:       now,
		ProjectKey:   s.projectKey,
		ParamVersion: s.paramVersion,
	})
}

// ListReferentCandidates returns promoted mappings for the turn's
// extracted phrases, strongest evidence first, capped at limit —
// the injection pool the turn tail renders.
func (s *service) ListReferentCandidates(ctx context.Context, phrases []string, limit int) ([]Referent, error) {
	if len(phrases) == 0 || limit <= 0 {
		return nil, nil
	}
	rows, err := s.q.ListReferentsForPhrases(ctx, db.ListReferentsForPhrasesParams{
		ProjectKey: s.projectKey,
		Phrases:    phrases,
		Limit:      int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Referent, 0, len(rows))
	for _, r := range rows {
		out = append(out, Referent{
			Phrase:       r.Phrase,
			Target:       r.Target,
			Hits:         r.Hits,
			LastAt:       time.UnixMilli(r.LastAt),
			ProjectKey:   r.ProjectKey,
			ParamVersion: r.ParamVersion,
		})
	}
	return out, nil
}
