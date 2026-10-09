package agent

import (
	"context"
	"log/slog"
	"regexp"

	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/params"
)

// Session digests (#164) resolve continuation prompts — "continue",
// "what did we do yesterday", "the login thing" — to pointers into
// prior sessions: title, date, touched files. The write path is lazy:
// a vague or continuation-flavored prompt refreshes any digest whose
// session changed since it was materialized (sessions never formally
// end), then runs the FTS5 match — or the recency fallback when the
// prompt carries no content terms.
//
// The render is pointers only: stored fields the user can verify, not
// synthesized narrative. Rendered file pointers mark suggested like
// the referent channel's targets — an edit landing on a file the tail
// itself named is echo, not evidence.

// continuationCueRe matches prompts that reach back to earlier work —
// deictic session references a bare "continue" carries none of.
var continuationCueRe = regexp.MustCompile(`(?i)\b(` +
	`continue|continuing|resume|resuming|pick\s+up|left\s+off|` +
	`where\s+(we|i)\s+(were|left|stopped)|last\s+(session|time|night|week)|` +
	`yesterday|earlier\s+(today|session)|previous\s+(session|work)|` +
	`what\s+(did|were|was)\s+(we|i)|unfinished|still\s+working)\b`)

// sessionReferentRe catches the deictic-object prompt the vagueness
// pre-filter and the continuation cues both miss: "the login thing",
// "that auth session", "the work on the router". isVaguePrompt reads
// "the" as an English anchor and bails; the session channel is exactly
// what a deictic object points at. The object slot stays narrow —
// thing/stuff/work/session — so "the login page" or "the config file"
// stays an anchored prompt, not a recall one.
var sessionReferentRe = regexp.MustCompile(`(?i)\b(` +
	`the\s+[\p{L}\p{N}_.\-/]+\s+(things?|stuff|work|session)\b|` +
	`the\s+(things?|stuff|work)\b|` +
	`that\s+(?:[\p{L}\p{N}_.\-/]+\s+)?(session|work|thing|stuff)\b)\b`)

// digestCandidates fetches session-digest pointers for a vague,
// deictic, or continuation prompt: lazy refresh of stale digests
// first (bounded per turn), then FTS5 on the prompt's content terms,
// falling back to the recency list when the prompt carries no terms
// or the match set is empty under a continuation cue. A deictic
// prompt whose terms match nothing gets no recency guess — it named
// a specific target; a wrong pointer is worse than none.
func (a *sessionAgent) digestCandidates(ctx context.Context, call SessionAgentCall, mp params.Memory) []cmdlog.SessionDigest {
	if !a.sessionMemory || a.cmdlog == nil || a.isSubAgent ||
		call.RepairAttempts > 0 || call.Prompt == "" {
		return nil
	}
	continuation := continuationCueRe.MatchString(call.Prompt)
	if !continuation && !sessionReferentRe.MatchString(call.Prompt) &&
		!isVaguePrompt(call.Prompt, mp.VaguePromptMaxWords) {
		return nil
	}
	// The lazy write path — a session whose updated_at moved since its
	// digest (or that never had one) is materialized now. Cheap per
	// turn: the bound amortizes a long history across turns.
	if err := a.cmdlog.RefreshSessionDigests(ctx, mp.DigestRefreshLimit); err != nil {
		slog.Warn("Session digest refresh failed", "error", err)
	}
	digests, err := a.cmdlog.SearchSessionDigests(ctx, call.Prompt, call.SessionID, mp.DigestRenderLimit)
	if err != nil {
		slog.Warn("Session digest search failed", "error", err)
		return nil
	}
	// A continuation cue promises the user means earlier work — when
	// FTS finds nothing (or the prompt has no terms at all), the
	// freshest other sessions are the honest answer.
	if len(digests) == 0 && continuation {
		digests, err = a.cmdlog.RecentSessionDigests(ctx, call.SessionID, mp.DigestRenderLimit)
		if err != nil {
			slog.Warn("Session digest recency fallback failed", "error", err)
			return nil
		}
	}
	return digests
}
