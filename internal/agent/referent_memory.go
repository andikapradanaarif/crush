package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"unicode"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/params"
)

// Referent memory (#165) learns phrase→target mappings from accepted
// outcomes: a vague prompt ("fix the config") that the agent resolved
// to an edit of internal/config/config.go, followed by a next user
// turn carrying no revision signal, is one clean observation. Enough
// clean observations promote the mapping so a later vague prompt can
// render it as a candidate — "usually means X", never a fact.
//
// The three mechanisms the issue's amendment prescribes land as:
//  1. Episodic storage — referent_episodes rows, written at the next
//     user turn (the judgment point), keyed on the vague message ID
//     so repair-chain re-runs dedupe.
//  2. Promotion — accepted episodes count distinct sessions; the
//     threshold is params.Memory.ReferentPromoteHits.
//  3. Contamination screen — an edit landing on a file the rendered
//     referent section itself named is marked suggested and excluded
//     from evidence (the self-reinforcing-heat guard).

// referentVerdictRe matches revision cues in the user turn that
// follows a vague prompt's edits — the negative half of the
// acceptance signal. Cue tokens only; mentioning the target's name
// is not itself a revision ("also update config.go" extends, not
// corrects). Bare verdicts count too: "wrong.", "nope", "try again"
// are all revision signals a session-grain read should catch.
var referentVerdictRe = regexp.MustCompile(`(?i)\b(revert|undo|rollback|roll\s+back|` +
	`wrong|incorrect|nope|try\s+again|redo|start\s+over|not\s+that|` +
	`that'?s\s+not|i\s+meant|shouldn'?t\s+have|should\s+not\s+have|` +
	`mistake|oops)\b`)

// referentMutationTools are the calls whose file_path marks the
// referent the agent committed to — edits, not reads: a view of the
// file is exploration, an edit is the answer the user judges.
var referentMutationTools = map[string]bool{
	tools.EditToolName:      true,
	tools.WriteToolName:     true,
	tools.MultiEditToolName: true,
}

// referentJunkNouns can follow "the" but never name a target —
// "the same thing" must not learn a mapping for "same".
var referentJunkNouns = map[string]bool{
	"same": true, "thing": true, "things": true, "stuff": true,
	"one": true, "other": true, "another": true, "rest": true,
}

// extractReferentPhrase reduces a vague prompt to the learned
// phrase key — currently the definite-article noun ("fix the
// config" → "config"). Prompts carrying several distinct "the X"
// referents record nothing: which edit maps to which noun is
// unknowable from the trace, and a wrong attribution is worse than
// a missed one.
func extractReferentPhrase(prompt string) string {
	matches := theNounRe.FindAllStringSubmatch(prompt, -1)
	seen := map[string]struct{}{}
	var nouns []string
	for _, m := range matches {
		n := strings.ToLower(m[1])
		if referentJunkNouns[n] {
			continue
		}
		if _, ok := seen[n]; !ok {
			seen[n] = struct{}{}
			nouns = append(nouns, n)
		}
	}
	if len(nouns) != 1 {
		return ""
	}
	return nouns[0]
}

// referentLabelMatureLimit bounds the per-turn artifact-label
// maturity pass (#294) — the oldest pending episodes re-check each
// turn, so the backlog amortizes across turns instead of a
// scheduler owning it.
const referentLabelMatureLimit = 50

// referentJudgedVerdict classifies the current user prompt's verdict
// on the previous turn's edits — a correction cue marks the episode
// revised. A follow-up the English cue regex cannot read is unknown,
// not accepted: unparseable input must not mint promotion evidence
// (learned labels fail closed, same asymmetry rule the substrate
// applies elsewhere). ASCII-script text the regex reads cleanly
// counts as acceptance — weak feedback per the issue's amendment:
// the user may have revised silently, which is why the promotion
// floor counts distinct sessions and the render stays a candidate.
// Known residual: a Latin-script language the cues don't cover
// (e.g. Indonesian "bukan itu") still slips — the artifact-based
// acceptance signals are the real fix for that class.
func referentJudgedVerdict(currentPrompt string) string {
	if referentVerdictRe.MatchString(currentPrompt) {
		return cmdlog.ReferentRevised
	}
	hasLetter := false
	for _, r := range currentPrompt {
		if unicode.IsLetter(r) {
			hasLetter = true
			if r > unicode.MaxASCII {
				// A letter the English cue vocabulary can't
				// read — the verdict may be a revision the
				// regex doesn't know.
				return cmdlog.ReferentUnknown
			}
		}
	}
	if !hasLetter {
		// No lexical content at all (emoji, punctuation) — there
		// is no cue to evaluate either way.
		return cmdlog.ReferentUnknown
	}
	return cmdlog.ReferentAccepted
}

// referentTargets extracts the workspace-relative files the agent
// mutated after the judged message — the referent the turn committed
// to. Order is first-edit order; duplicates collapse (the unique key
// dedupes anyway, but the call ID should name the first edit).
// Delegated edits count too: an `agent` tool call's mutations live in
// the child session's messages, reachable through the deterministic
// messageID$$toolCallID session ID.
func (a *sessionAgent) referentTargets(ctx context.Context, msgs []message.Message, after int) []struct {
	path   string
	callID string
} {
	var out []struct {
		path   string
		callID string
	}
	seen := map[string]struct{}{}
	add := func(path, callID string) {
		rel := a.relWorkdir(path)
		if _, ok := seen[rel]; ok {
			return
		}
		seen[rel] = struct{}{}
		out = append(out, struct {
			path   string
			callID string
		}{rel, callID})
	}
	visited := map[string]struct{}{}
	var scan func(ms []message.Message, after int)
	scan = func(ms []message.Message, after int) {
		for _, m := range ms[after:] {
			if m.Role != message.Assistant {
				continue
			}
			for _, tc := range m.ToolCalls() {
				if tc.Name == AgentToolName && a.sessions != nil && a.messages != nil {
					childID := a.sessions.CreateAgentToolSessionID(m.ID, tc.ID)
					if _, ok := visited[childID]; ok {
						continue
					}
					visited[childID] = struct{}{}
					if childMsgs, err := a.messages.List(ctx, childID); err == nil {
						scan(childMsgs, 0)
					}
					continue
				}
				if !referentMutationTools[tc.Name] || tc.Input == "" {
					continue
				}
				var in struct {
					FilePath string `json:"file_path"`
				}
				if json.Unmarshal([]byte(tc.Input), &in) != nil || in.FilePath == "" {
					continue
				}
				add(in.FilePath, tc.ID)
			}
		}
	}
	scan(msgs, after)
	return out
}

// isRepairPrompt reports whether a stored user message is
// harness-authored — a repair retry persists its prompt as a real
// user row, so the backward scan for the judged turn must skip it.
func isRepairPrompt(text string) bool {
	for _, p := range RepairPromptPrefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

// recordReferentEpisodes is the observation pass — runs once per Run
// at turn-context build, before the pools select. The last real user
// message in msgs is the turn under judgment (msgs predates this
// run's prompt); repair prompts are harness rows interposed between
// the prompt and this turn, so the scan skips them — the work they
// drove still attributes to the vague turn they were repairing. If it
// was vague and the turn produced edits, the current prompt judges it
// and the episodes record with verdict and contamination flag.
func (a *sessionAgent) recordReferentEpisodes(ctx context.Context, call SessionAgentCall, msgs []message.Message) {
	if a.cmdlog == nil || a.isSubAgent || call.RepairAttempts > 0 || call.Prompt == "" {
		return
	}
	lastUser := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.User && !isRepairPrompt(msgs[i].JoinedText()) {
			lastUser = i
			break
		}
	}
	if lastUser < 0 {
		return
	}
	prev := msgs[lastUser]
	mp := a.memoryParams()
	prevPrompt := prev.JoinedText()
	if !isVaguePrompt(prevPrompt, mp.VaguePromptMaxWords) {
		return
	}
	phrase := extractReferentPhrase(prevPrompt)
	if phrase == "" {
		return
	}
	targets := a.referentTargets(ctx, msgs, lastUser+1)
	if len(targets) != 1 {
		// Zero targets is nothing to judge; several is the
		// multi-referent case the channel abstains from — which edit
		// maps to the phrase is unknowable from the trace, and a
		// collateral edit minting phrase→target evidence is the
		// wrong-attribution failure this channel exists to avoid.
		return
	}
	verdict := referentJudgedVerdict(call.Prompt)
	steps, tokens := a.judgedTurnCost(msgs, lastUser+1, call.SessionID)
	for _, t := range targets {
		ep := cmdlog.ReferentEpisode{
			Phrase:          phrase,
			Target:          t.path,
			SessionID:       call.SessionID,
			SourceMessageID: prev.ID,
			ToolCallID:      t.callID,
			RepoState:       a.referentRepoState(),
			Suggested:       a.cmdlog.WasSuggestedFile(ctx, call.SessionID, t.path),
			Verdict:         verdict,
		}
		err := a.cmdlog.RecordReferentEpisode(ctx, ep, mp.ReferentPromoteHits)
		if err != nil {
			slog.Warn("Failed to record referent episode", "error", err)
			continue
		}
		// Artifact labels (#294): the signals that decide acceptance
		// from evidence rather than cue words. WrongTarget is
		// definitionally zero — the single-target abstention above
		// guarantees every recorded episode had exactly one
		// mutation — and len(targets)-1 keeps that semantics visible
		// if the abstention ever relaxes.
		lerr := a.cmdlog.LabelReferentEpisode(ctx, ep, cmdlog.ReferentEpisodeLabelInputs{
			Steps:       steps,
			Tokens:      tokens,
			WrongTarget: int64(len(targets) - 1),
		})
		if lerr != nil {
			slog.Warn("Failed to label referent episode", "error", lerr)
		}
	}
}

// judgedTurnCost is the artifact-label cost pair for the turn that
// produced the episode (#294): every tool call the judged slice
// issued counts as a step — an agent-tool call counts once, the same
// unit the user sees — and tokens is the session's accumulated
// request usage through this pass from reqStats. Session-cumulative,
// not per-turn: the honest bound "effort spent through the judged
// turn plus the margin to judge it", and process-local, so a
// restarted session undercounts rather than guesses.
func (a *sessionAgent) judgedTurnCost(msgs []message.Message, after int, sessionID string) (steps, tokens int64) {
	for _, m := range msgs[after:] {
		if m.Role == message.Assistant {
			steps += int64(len(m.ToolCalls()))
		}
	}
	if a.reqStats != nil {
		if rs, ok := a.reqStats.Get(sessionID); ok {
			for _, s := range rs.Steps {
				tokens += s.InputTokens + s.OutputTokens
			}
		}
	}
	return steps, tokens
}

// referentRepoState stamps the episode with the repo state the
// observation was true of — HEAD SHA inside a repository, empty
// outside (the cmdlog helper tolerates the same shape).
func (a *sessionAgent) referentRepoState() string {
	if a.configStore == nil {
		return ""
	}
	return headSHA(a.configStore.WorkingDir())
}

// referentCandidates fetches promoted mappings for the turn's phrase
// — the injection pool. A vague prompt is the only shape that carries
// a referent phrase worth rendering, so the fetch gates on the same
// vagueness test the learner used.
func (a *sessionAgent) referentCandidates(ctx context.Context, call SessionAgentCall, mp params.Memory) []cmdlog.Referent {
	if !a.referentMemory || a.cmdlog == nil || a.isSubAgent ||
		call.RepairAttempts > 0 || call.Prompt == "" ||
		!isVaguePrompt(call.Prompt, mp.VaguePromptMaxWords) {
		return nil
	}
	phrase := extractReferentPhrase(call.Prompt)
	if phrase == "" {
		return nil
	}
	refs, err := a.cmdlog.ListReferentCandidates(ctx, []string{phrase}, mp.ReferentRenderLimit)
	if err != nil {
		slog.Warn("Referent candidate fetch failed", "error", err)
		return nil
	}
	return refs
}
