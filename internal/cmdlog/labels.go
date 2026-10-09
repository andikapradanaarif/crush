// Artifact-based acceptance labels (#294): the episode verdict is
// the next user turn's cue-word judgment — a weak, language-bound
// signal. These columns instead record what the artifacts show, so
// acceptance is derivable from evidence: whether the edited file
// still matches its post-edit hash, whether a commit has since
// touched it, whether the session's test runs ended green, and what
// the turn cost. Every signal keeps honest absence — NULL means
// never observed, never a passing grade.
package cmdlog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/db"
)

// ReferentEpisodeLabelInputs carries the label signals only the
// agent can compute — the judged turn's cost and scope violations
// come from the message trace. The file- and repo-side signals the
// store computes itself.
type ReferentEpisodeLabelInputs struct {
	// Steps is the tool-call count the judged turn issued — the
	// effort leg of "steps/tokens to done".
	Steps int64
	// Tokens is the session's accumulated request tokens through the
	// label pass — process-local, so a restarted session undercounts
	// rather than guesses.
	Tokens int64
	// WrongTarget is the mutating edits the judged turn produced
	// outside the admitted referent — the wrong-file signal. Always
	// zero today: the episode writer abstains on multi-target turns,
	// and the column is recorded so the signal exists the day that
	// rule relaxes.
	WrongTarget int64
}

// LabelReferentEpisode stamps the artifact signals computable at
// episode-record time: the target's post-edit hash baseline (the
// anchor the survival check compares against), the judged turn's
// cost, and the judged session's last-run test verdicts. The update
// keys on the episode's (session, source message, target) unique
// constraint — the same idempotency point the insert uses, so a
// repair-chain re-derivation re-stamps rather than duplicates. A
// file already unreadable at record time writes an empty baseline,
// which keeps the episode out of the maturity scan: no baseline, no
// survival check — unknown, not failed.
func (s *service) LabelReferentEpisode(ctx context.Context, ep ReferentEpisode, in ReferentEpisodeLabelInputs) error {
	return s.q.LabelReferentEpisode(ctx, db.LabelReferentEpisodeParams{
		LabelTargetHash:  s.fileHash(ep.Target),
		LabelWrongTarget: in.WrongTarget,
		LabelSteps:       in.Steps,
		LabelTokens:      in.Tokens,
		LabelTestsGreen:  s.sessionTestsGreen(ctx, ep.SessionID),
		LabeledAt:        s.now().UnixMilli(),
		SessionID:        ep.SessionID,
		SourceMessageID:  ep.SourceMessageID,
		Target:           ep.Target,
	})
}

// MatureReferentLabels re-checks the signals that only resolve over
// time — whether the episode's file still matches its post-edit
// baseline (survival) and whether a commit newer than the episode
// has touched it. The pending set is "not yet committed OR file
// still matches" — a committed-then-diverged row is terminal and
// drops out of the scan. hash_changed re-derives every pass, so a
// file restored to its baseline reads surviving again; committed is
// monotone — once observed it stays observed, since a landed commit
// can't un-happen. Bounded per call: every turn's pass drains the
// oldest-pending rows, so the backlog amortizes across turns without
// a scheduler. Outside a repository the commit check stays NULL —
// honest absence rather than a fabricated 0.
func (s *service) MatureReferentLabels(ctx context.Context, limit int) error {
	if limit <= 0 {
		// LIMIT <= 0 in SQLite is unlimited — clamp, never unbound.
		limit = defaultListLimit
	}
	rows, err := s.q.ListPendingReferentLabels(ctx, db.ListPendingReferentLabelsParams{
		ProjectKey: s.projectKey,
		Limit:      int64(limit),
	})
	if err != nil || len(rows) == 0 {
		return err
	}
	var commits map[string][]int64
	if s.hasRepo {
		// One log over the window covering the oldest pending
		// episode — a single git call serves the whole batch.
		since := time.UnixMilli(rows[0].CreatedAt).UTC().Format(time.RFC3339)
		commits = s.commitsTouchingPaths(ctx, since)
	}
	now := s.now().UnixMilli()
	for _, r := range rows {
		changed := int64(0)
		if cur := s.fileHash(r.Target); cur == "" || cur != r.LabelTargetHash {
			changed = 1
		}
		committed := r.LabelCommitted
		if s.hasRepo && !(committed.Valid && committed.Int64 == 1) {
			v := int64(0)
			for _, ct := range commits[r.Target] {
				// Git timestamps are seconds; the episode's
				// created_at is millis. A commit we cannot prove
				// landed after the episode is not evidence —
				// fail-closed on the ambiguous same-second edge.
				if ct*1000 >= r.CreatedAt {
					v = 1
					break
				}
			}
			committed = sql.NullInt64{Int64: v, Valid: true}
		}
		if err := s.q.UpdateReferentEpisodeLabel(ctx, db.UpdateReferentEpisodeLabelParams{
			LabelCommitted:   committed,
			LabelHashChanged: sql.NullInt64{Int64: changed, Valid: true},
			LabeledAt:        now,
			ID:               r.ID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// fileHash is the survival baseline: SHA-256 of the target's current
// content, workspace-relative. Empty when the file is unreadable —
// including already-deleted, which the maturity check reads as
// diverged (the edit did not survive) only when a baseline exists.
// Paths escaping the workspace refuse — label targets come from
// tool-call paths, and a stored "../x" must not read outside.
func (s *service) fileHash(target string) string {
	if target == "" {
		return ""
	}
	clean := filepath.Clean(filepath.FromSlash(target))
	if filepath.IsAbs(clean) || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(s.workingDir, clean))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// sessionTestsGreen folds the judged session's last-run test
// verdicts into the tri-state signal: every observed kind='test'
// command ending clean is 1, any failure is 0, and a session that
// ran nothing the ledger calls a test leaves NULL — unobserved, not
// green. The ledger's last-exit semantics mean a test the session
// ran early still counts toward session-end state.
func (s *service) sessionTestsGreen(ctx context.Context, sessionID string) sql.NullInt64 {
	rows, err := s.q.ListSessionTestVerdicts(ctx, db.ListSessionTestVerdictsParams{
		ProjectKey:      s.projectKey,
		ID:              sessionID,
		ParentSessionID: sql.NullString{String: sessionID, Valid: true},
	})
	if err != nil || len(rows) == 0 {
		return sql.NullInt64{}
	}
	for _, r := range rows {
		if r.LastExit != 0 {
			return sql.NullInt64{Int64: 0, Valid: true}
		}
	}
	return sql.NullInt64{Int64: 1, Valid: true}
}

// commitsTouchingPaths maps each path a commit in the window touched
// to that commit's unix-second timestamps — one git call for the
// whole pending batch. The "commit <sec>" header lines carry the
// timestamp so a path that happens to be all digits can't be
// misread as a commit boundary.
func (s *service) commitsTouchingPaths(ctx context.Context, sinceRFC3339 string) map[string][]int64 {
	out := map[string][]int64{}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := gitOut(ctx, s.workingDir, "log",
		"--since="+sinceRFC3339, "--format=commit %ct", "--name-only")
	if err != nil {
		return out
	}
	var ct int64 = -1
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "commit "):
			if n, perr := strconv.ParseInt(strings.TrimPrefix(line, "commit "), 10, 64); perr == nil {
				ct = n
			} else {
				ct = -1
			}
		case line != "" && ct >= 0:
			out[line] = append(out[line], ct)
		}
	}
	return out
}
