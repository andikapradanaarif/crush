package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"

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
// corrects).
var referentVerdictRe = regexp.MustCompile(`(?i)\b(revert|undo|rollback|roll\s+back|` +
	`wrong\s+(file|one|place|thing)|not\s+that|that'?s\s+not|i\s+meant|` +
	`shouldn'?t\s+have|should\s+not\s+have|mistake|oops)\b`)

// referentMutationTools are the calls whose file_path marks the
// referent the agent committed to — edits, not reads: a view of the
// file is exploration, an edit is the answer the user judges.
var referentMutationTools = map[string]bool{
	tools.EditToolName:      true,
	tools.WriteToolName:     true,
	tools.MultiEditToolName: true,
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

// referentJudgedVerdict classifies the current user prompt's verdict
// on the previous turn's edits — a correction cue marks the episode
// revised; anything else is acceptance (weak feedback per the issue's
// amendment: the user may have revised silently, which is why the
// promotion floor counts distinct sessions and the render stays a
// candidate).
func referentJudgedVerdict(currentPrompt string) string {
	if referentVerdictRe.MatchString(currentPrompt) {
		return cmdlog.ReferentRevised
	}
	return cmdlog.ReferentAccepted
}

// referentTargets extracts the workspace-relative files the agent
// mutated after the judged message — the referent the turn committed
// to. Order is first-edit order; duplicates collapse (the unique key
// dedupes anyway, but the call ID should name the first edit).
func (a *sessionAgent) referentTargets(msgs []message.Message, after int) []struct {
	path   string
	callID string
} {
	var out []struct {
		path   string
		callID string
	}
	seen := map[string]struct{}{}
	for _, m := range msgs[after:] {
		if m.Role != message.Assistant {
			continue
		}
		for _, tc := range m.ToolCalls() {
			if !referentMutationTools[tc.Name] || tc.Input == "" {
				continue
			}
			var in struct {
				FilePath string `json:"file_path"`
			}
			if json.Unmarshal([]byte(tc.Input), &in) != nil || in.FilePath == "" {
				continue
			}
			rel := a.relWorkdir(in.FilePath)
			if _, ok := seen[rel]; ok {
				continue
			}
			seen[rel] = struct{}{}
			out = append(out, struct {
				path   string
				callID string
			}{rel, tc.ID})
		}
	}
	return out
}

// recordReferentEpisodes is the observation pass — runs once per Run
// at turn-context build, before the pools select. The last user
// message in msgs is the turn under judgment (msgs predates this
// run's prompt); if it was vague and the turn produced edits, the
// current prompt judges it and the episodes record with verdict and
// contamination flag.
func (a *sessionAgent) recordReferentEpisodes(ctx context.Context, call SessionAgentCall, msgs []message.Message) {
	if a.cmdlog == nil || a.isSubAgent || call.RepairAttempts > 0 || call.Prompt == "" {
		return
	}
	lastUser := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.User {
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
	targets := a.referentTargets(msgs, lastUser+1)
	if len(targets) == 0 {
		return
	}
	verdict := referentJudgedVerdict(call.Prompt)
	for _, t := range targets {
		err := a.cmdlog.RecordReferentEpisode(ctx, cmdlog.ReferentEpisode{
			Phrase:          phrase,
			Target:          t.path,
			SessionID:       call.SessionID,
			SourceMessageID: prev.ID,
			ToolCallID:      t.callID,
			RepoState:       a.referentRepoState(),
			Suggested:       a.cmdlog.WasSuggestedFile(ctx, call.SessionID, t.path),
			Verdict:         verdict,
		}, mp.ReferentPromoteHits)
		if err != nil {
			slog.Warn("Failed to record referent episode", "error", err)
		}
	}
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
