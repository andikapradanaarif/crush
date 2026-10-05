package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/notebook"
	"github.com/charmbracelet/crush/internal/session"
)

const (
	// turnContextWorkingSetLimit bounds the working-set section of the
	// per-turn blob. The read set is cumulative — unbounded it
	// degenerates to "every file ever touched".
	turnContextWorkingSetLimit = 10
	// vaguePromptMaxWords bounds the vagueness pre-filter: prompts
	// longer than this carry enough of their own context that a
	// missing referent is unlikely.
	vaguePromptMaxWords = 12
	// turnContextIntentMaxBytes bounds the rendered intent record. A
	// statement must never render truncated — a cut constraint reads
	// as a different instruction — so the budget drops whole oldest
	// items instead.
	turnContextIntentMaxBytes = 4096
	// turnContextFileHeatLimit bounds the cross-session heat section —
	// a hint list, not working state, so it runs tighter than the
	// working-set cap.
	turnContextFileHeatLimit = 5
	// turnContextOpenFailuresFetchLimit bounds the candidate pool the
	// selector sees — bounded for fetch cost, wide enough that a
	// relevant row past the render cap still earns a decision record
	// instead of vanishing before selection.
	turnContextOpenFailuresFetchLimit = 50
	// turnContextOpenFailuresRenderLimit bounds the failure-memory
	// tail itself — recent-first, so the cap keeps the freshest bound
	// rows; rows it cuts record render_capped, not silence.
	turnContextOpenFailuresRenderLimit = 5
	// turnContextFailureFileHints bounds file hints rendered per
	// failure row.
	turnContextFailureFileHints = 3
	// Render-side caps on echoed failure fields — write-side caps
	// already bound them, these keep the tail bounded regardless.
	turnContextFailureCmdRunes      = 200
	turnContextFailureHeadlineRunes = 140
)

// vagueReferentRe matches prompts that lean on a definite or anaphoric
// referent whose target context must supply — "the bug", "it", "this
// crash". The noun list mirrors referentKindHints so a mapped noun can
// never be gated out upstream — the cost is that "run the tests" now
// reads as referent-shaped even though the whole suite is actionable;
// the directive is advisory, so over-firing is cheap.
var vagueReferentRe = regexp.MustCompile(`(?i)\b(it|its|this|that|them|they)\b|` +
	`\bthe\s+(bugs?|bugfix|crash(?:es)?|errors?|failures?|fail|issues?|problems?|panics?|regressions?|` +
	`leaks?|typo|warnings?|hangs?|deadlocks?|suites?|pipelines?|jobs?|` +
	`config|configuration|tests?|specs?|endpoint|handler|route|feature|changes?|fix|workaround|hack|todo|fixme)\b`)

// theNounRe extracts the definite-article noun for referent shape
// checks — "the test" → "test". The task-binding selector maps it to
// command kinds via referentKindHints.
var theNounRe = regexp.MustCompile(`(?i)\bthe\s+(\w+)\b`)

// isVaguePrompt reports whether the prompt is underspecified in the
// way the pre-filter cares about: short enough to carry no context of
// its own, naming no explicit file paths, and either leaning on a
// referent or carrying no parseable English anchors at all. The
// last clause covers non-English prompts: the referent regex only
// reads English, but a short, path-less prompt is vague in any
// language, and the directive it arms is advisory — the model judges
// underspecification in the user's own words. Fields-style word
// counts stay: an unspaced script is "one word", which is already
// the vague direction.
func isVaguePrompt(prompt string) bool {
	n := len(strings.Fields(prompt))
	if n == 0 || n > vaguePromptMaxWords {
		return false
	}
	if len(extractExplicitFilePaths(prompt)) > 0 {
		return false
	}
	if vagueReferentRe.MatchString(prompt) {
		return true
	}
	// No recognized English function words at all — a prompt this
	// layer cannot read, treated as vague so the model, which can
	// read it, gets the clarify directive. A bare plausible command
	// ("ls", "htop") is anchored on its own — it stays non-vague.
	for _, w := range strings.Fields(prompt) {
		w = strings.ToLower(strings.Trim(w, " \t.,;:!?()[]{}\"'`—–-"))
		if scopeStopWords[w] || commandTokenRe.MatchString(w) {
			return false
		}
	}
	return true
}

// commandTokenRe matches a bare plausible executable — lowercase
// ASCII like "ls" or "htop" — so a one-word command prompt isn't
// read as vague. Non-ASCII words can't match, which is correct:
// an unreadable token is the vague case, not the anchored one.
var commandTokenRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,15}$`)

// turnTailMessages returns the ephemeral per-turn tail messages — the
// turn-context blob and, when the vagueness pre-filter fires, the
// clarify directive. They are computed once per Run and appended to
// prepared.Messages inside PrepareStep so they survive the notebook
// rebuild and stay byte-stable across the turn's steps: the history
// prefix still cache-reads and the blob rides in the tail that is new
// anyway.
//
// The tail is a user-role message, not a system message: the Anthropic
// and Google providers drop every system block that appears after
// non-system content (finishedSystemBlock in both toPrompt
// converters), which would silently discard the tail on exactly the
// providers where cache stability matters most. A user-role tail
// survives every provider.
func (a *sessionAgent) turnTailMessages(ctx context.Context, call SessionAgentCall, msgs []message.Message) []fantasy.Message {
	// Open failures are fetched once per Run — the tail blob renders
	// the subset the task-binding selector admits and the ambiguity
	// gate reads the same slice. The decision list records every
	// candidate's verdict so "rendered nothing" decomposes into "no
	// candidates" versus "candidates rejected".
	var openFailures []cmdlog.Failure
	var failureDecisions []FailureDecision
	var failureCandidates []cmdlog.Failure
	var fetchErr error
	// The telemetry holdout suppresses injection for the session but
	// not the fetch: the turn record still captures which candidates
	// the suppressed arm would have rendered — the control group's
	// counterfactual, not just a blank. Note the suppression covers
	// every openFailures consumer in this function — including
	// ambiguityDirective — which is the correct counterfactual for
	// "did memory help" (full effect, not render only).
	holdout := false
	telemetryOn := a.memoryTelemetry != nil && !a.isSubAgent
	// armed is effective arming — flag on AND a store to read. A
	// flag-on session with a nil cmdlog records memory_armed:false
	// rather than coining a holdout that could never inject.
	armed := a.failureMemory && a.cmdlog != nil
	if telemetryOn && armed {
		holdout = a.memoryTelemetry.holdoutOff(call.SessionID)
	}
	if a.failureMemory && a.cmdlog != nil && !a.isSubAgent {
		var f []cmdlog.Failure
		f, fetchErr = a.cmdlog.ListOpenFailures(ctx, turnContextOpenFailuresFetchLimit)
		if fetchErr == nil {
			var workDir string
			if a.configStore != nil {
				workDir = a.configStore.WorkingDir()
			}
			var selected []cmdlog.Failure
			selected, failureDecisions = selectOpenFailures(call.Prompt, f, workDir,
				turnContextOpenFailuresRenderLimit)
			failureCandidates = f
			if !holdout {
				openFailures = selected
			}
		} else {
			slog.Debug("Open-failure fetch failed; tail renders without memory",
				"session_id", call.SessionID, "error", fetchErr)
		}
	}
	sections := a.turnContextSections(ctx, call, openFailures)
	if directive := a.ambiguityDirective(ctx, call, msgs, openFailures); directive != "" {
		sections = append(sections, directive)
	}
	if telemetryOn {
		a.memoryTelemetry.recordTurn(call.SessionID, call.Prompt, sections,
			failureCandidates, failureDecisions, a.agentID, armed, holdout, fetchErr)
	}
	if len(sections) == 0 {
		// An armed tail that renders nothing still records an audit:
		// "checked and found nothing" is evidence, distinct from "the
		// tail machinery never ran" — and that difference is what
		// makes an eval's max_tail.sections.* "didn't render"
		// predicate checkable. An unarmed agent still clears a stale
		// audit from an earlier Run in the same process.
		if a.tailArmed() {
			a.recordTailAudit(call.SessionID, nil, "", failureDecisions, fetchErr)
		} else if a.tailAudit != nil {
			a.tailAudit.Del(call.SessionID)
		}
		return nil
	}
	text := strings.Join(sections, "\n\n")
	slog.Debug("Turn tail augmentation",
		"session_id", call.SessionID,
		"sections", len(sections),
		"bytes", len(text),
	)
	a.recordTailAudit(call.SessionID, sections, text, failureDecisions, fetchErr)
	return []fantasy.Message{fantasy.NewUserMessage(text)}
}

// tailArmed reports whether any tail producer is enabled on this
// agent — failure memory, the session-signals tier, or the ambiguity
// gate. Sub-agents are never armed. An armed agent's audit records
// even a zero-section render, so an eval asserting "this arm rendered
// no open_failures" reads that silence as a measurement, not an
// absence.
func (a *sessionAgent) tailArmed() bool {
	return !a.isSubAgent &&
		(a.failureMemory || a.turnContext == "session" || a.ambiguityClarification)
}

// TailSection names one rendered tail envelope and its size — one
// row of the per-turn audit.
type TailSection struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
}

// TailAudit is the ephemeral per-turn tail's observable record:
// which context envelopes rendered, their sizes, the joined text's
// digest, and the verbatim text. The tail never persists to message
// storage — this is the only durable answer to "what did the model
// actually see at this turn", which is exactly what an eval artifact
// needs to audit context-injection arms. SessionTelemetry exports it;
// eval.TurnTail is the wire mirror.
//
// Two reading caveats: the snapshot is taken at render, before the
// request flies — a run that errors before its first request lands
// still records the tail it prepared (the record's error fields
// distinguish delivered from prepared); and Bytes counts the joined
// text including the "\n\n" separators, so it exceeds the sections'
// byte sum whenever more than one envelope renders.
type TailAudit struct {
	Sections []TailSection `json:"sections"`
	Bytes    int           `json:"bytes"`
	SHA256   string        `json:"sha256"`
	Text     string        `json:"text"`
	// Decisions is the task-binding selector's per-candidate verdict
	// for open_failures — every evaluated row, admitted or rejected
	// with its reason. Empty when the selector saw no candidates.
	Decisions []FailureDecision `json:"decisions,omitempty"`
	// FetchError is set when ListOpenFailures itself failed — the
	// empty Decisions then mean "couldn't evaluate", which an eval
	// must not read as "evaluated, none bound".
	FetchError string `json:"fetch_error,omitempty"`
}

var tailSectionNameRe = regexp.MustCompile(`^<(\w+)>`)

// tailSectionName extracts the envelope name from a rendered tail
// blob — the audit and the telemetry log name sections identically.
func tailSectionName(s string) string {
	if m := tailSectionNameRe.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
		return m[1]
	}
	return "unknown"
}

// recordTailAudit snapshots the rendered tail for SessionTelemetry.
// Last-write-wins per session: a process's later Run replaces the
// audit, matching the telemetry emission's once-per-process shape.
func (a *sessionAgent) recordTailAudit(sessionID string, sections []string, text string, decisions []FailureDecision, fetchErr error) {
	if a.tailAudit == nil || sessionID == "" {
		return
	}
	sum := sha256.Sum256([]byte(text))
	audit := TailAudit{
		Bytes:     len(text),
		SHA256:    hex.EncodeToString(sum[:]),
		Text:      text,
		Decisions: decisions,
	}
	if fetchErr != nil {
		audit.FetchError = fetchErr.Error()
	}
	for _, s := range sections {
		audit.Sections = append(audit.Sections, TailSection{Name: tailSectionName(s), Bytes: len(s)})
	}
	a.tailAudit.Set(sessionID, audit)
}

// turnContextBlob renders the tail context sections joined for
// display and tests. The tail and its audit need the per-envelope
// split — see turnContextSections.
func (a *sessionAgent) turnContextBlob(ctx context.Context, call SessionAgentCall, openFailures []cmdlog.Failure) string {
	return strings.Join(a.turnContextSections(ctx, call, openFailures), "\n")
}

// turnContextSections renders the tail context sections — the session
// signals wrapped in <turn_context> when that tier is on, and project
// failure memory under its own <open_failures> envelope. The failure
// section renders outside the tier's wrapper: with
// turn_context=off + failure_memory=on an <open_failures> inside
// <turn_context> would attribute its content to a disabled tier.
// Returns nil for a sub-agent or when no enabled signal has content.
func (a *sessionAgent) turnContextSections(ctx context.Context, call SessionAgentCall, openFailures []cmdlog.Failure) []string {
	if a.isSubAgent {
		return nil
	}
	var sections []string

	if a.turnContext == "session" {
		var b strings.Builder
		a.renderSessionSignals(ctx, call, &b)
		if b.Len() > 0 {
			sections = append(sections, "<turn_context>\n"+b.String()+"</turn_context>")
		}
	}

	if a.failureMemory && len(openFailures) > 0 {
		var b strings.Builder
		b.WriteString("<open_failures>\nCommands that failed in this workspace and have not passed since — the likely referents for \"the failing test\" or \"the build error\"; a clean re-run resolves one:\n")
		for _, f := range openFailures {
			b.WriteString("- ")
			b.WriteString(tailSafeText(truncateTailText(f.Cmd, turnContextFailureCmdRunes)))
			if f.CWD != "" && f.CWD != "." {
				fmt.Fprintf(&b, " (in %s)", tailSafeText(f.CWD))
			}
			if f.Headline != "" {
				// Screened at render too — rows persisted before the
				// write-side screen existed are still covered.
				fmt.Fprintf(&b, ": %s", tailSafeText(truncateTailText(
					cmdlog.ScreenHeadline(f.Headline), turnContextFailureHeadlineRunes)))
			}
			if n := min(len(f.Files), turnContextFailureFileHints); n > 0 {
				hints := make([]string, n)
				for i := range hints {
					hints[i] = tailSafeText(f.Files[i])
				}
				fmt.Fprintf(&b, " [%s]", strings.Join(hints, ", "))
			}
			if age := failureAge(f.LastSeen); age != "" {
				fmt.Fprintf(&b, " — %s", age)
			}
			b.WriteString("\n")
		}
		b.WriteString("</open_failures>\n")
		sections = append(sections, b.String())
	}

	return sections
}

// tailSafeText neutralizes angle brackets in content echoed into the
// tail — a stored headline like "</open_failures>" could otherwise
// spoof a section boundary. Write-side caps bound length; the
// render-side truncate below keeps that bound honest if they loosen.
var tailSafeText = strings.NewReplacer("<", "(", ">", ")").Replace

// failureAge is the staleness hint rendered on an open failure — a
// failure last seen twenty days ago weighs differently than one seen
// this hour. Fresh rows render nothing: no suffix noise on the
// common case.
func failureAge(lastSeen time.Time) string {
	age := time.Since(lastSeen)
	switch {
	case age >= 24*time.Hour:
		return fmt.Sprintf("%dd ago", int(age.Hours()/24))
	case age >= time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	default:
		return ""
	}
}

func truncateTailText(s string, maxRunes int) string {
	if r := []rune(s); len(r) > maxRunes {
		return string(r[:maxRunes])
	}
	return s
}

// renderSessionSignals writes the session-tier sections — intent,
// working set, file heat, open todos — into b.
func (a *sessionAgent) renderSessionSignals(ctx context.Context, call SessionAgentCall, b *strings.Builder) {
	if a.notebook != nil {
		if entries, err := a.notebook.SearchByEventType(ctx, call.SessionID, notebook.EventUserIntent); err == nil && len(entries) > 0 {
			// Budget drops whole oldest items — a constraint rendered
			// partial is worse than absent.
			var items []string
			size := 0
			for i := len(entries) - 1; i >= 0; i-- {
				line := intentLine(entries[i])
				if size+len(line) > turnContextIntentMaxBytes && len(items) > 0 {
					break
				}
				items = append([]string{line}, items...)
				size += len(line)
			}
			if len(items) > 0 {
				b.WriteString("<user_intent>\nUser statements this session, verbatim — constraints and scope in the user's own words, oldest first; a later statement may override an earlier one:\n")
				if dropped := len(entries) - len(items); dropped > 0 {
					fmt.Fprintf(b, "- … %d earlier statement(s) omitted\n", dropped)
				}
				for _, line := range items {
					b.WriteString(line)
				}
				b.WriteString("</user_intent>\n")
			}
		}
	}

	if a.filetracker != nil {
		// Fetch the session's full read set once: the working-set
		// section renders its head, and the whole set dedupes heat —
		// a file this session already touched is not a hint.
		read, _ := a.filetracker.ListRecentReadFiles(ctx, call.SessionID, 0)
		if len(read) > 0 {
			b.WriteString("<working_set>\nRecently read or edited files — the most likely referents for \"the file\", \"the bug\", and similar:\n")
			for _, f := range read[:min(len(read), turnContextWorkingSetLimit)] {
				fmt.Fprintf(b, "- %s\n", a.relWorkdir(f))
			}
			b.WriteString("</working_set>\n")
		}
		// Over-fetch so working-set overlap cannot starve the section.
		if hot, err := a.filetracker.ListHotFiles(ctx, call.SessionID, turnContextFileHeatLimit*4); err == nil {
			wsSet := make(map[string]bool, len(read))
			for _, f := range read {
				wsSet[f] = true
			}
			var lines []string
			for _, h := range hot {
				if wsSet[h.Path] || len(lines) >= turnContextFileHeatLimit {
					continue
				}
				sessions := "sessions"
				if h.Sessions == 1 {
					sessions = "session"
				}
				lines = append(lines, fmt.Sprintf("- %s (%d %s)", a.relWorkdir(h.Path), h.Sessions, sessions))
			}
			if len(lines) > 0 {
				b.WriteString("<file_heat>\nFiles prior sessions in this workspace kept returning to — strong referents for vague mentions:\n")
				for _, l := range lines {
					b.WriteString(l + "\n")
				}
				b.WriteString("</file_heat>\n")
			}
		}
	}

	if a.sessions != nil {
		if sess, err := a.sessions.Get(ctx, call.SessionID); err == nil {
			var open []session.PlanItem
			for _, t := range sess.Todos {
				if t.Status != session.PlanItemCompleted {
					open = append(open, t)
				}
			}
			if len(open) > 0 {
				b.WriteString("<open_todos>\nDeclared work items still open:\n")
				keyByID := session.PlanKeyByID(sess.Todos)
				const maxListedTodos = 10
				for i, t := range open {
					if i >= maxListedTodos {
						fmt.Fprintf(b, "- … and %d more\n", len(open)-maxListedTodos)
						break
					}
					b.WriteString(session.FormatPlanItemLine(t, keyByID) + "\n")
				}
				b.WriteString("</open_todos>\n")
			}
		}
	}
}

// intentLine renders one intent item for the tail: the verbatim
// statement with its embedded heading stripped, labeled by provenance
// — turn number for this session's statements, "prior session" for
// hydrated seeds. Hydrated items carry a "_Seeded from …" preamble
// paragraph that duplicates the label's provenance signal, so it is
// stripped; internal newlines collapse to keep the statement one
// bullet (a newline before "-" would inject a phantom list item).
func intentLine(e notebook.Entry) string {
	text := e.EntryText
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	if rest, ok := strings.CutPrefix(strings.TrimSpace(text), "_Seeded from"); ok {
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			text = rest[i+1:]
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	label := fmt.Sprintf("turn %d", e.TurnNumber)
	if e.TurnNumber == notebook.HydrationTurnNumber {
		label = "prior session"
	}
	return fmt.Sprintf("- %s: %s\n", label, text)
}

// hasTool reports whether the agent's current toolset includes the
// named tool — the live check for pointers and capabilities rendered
// into the prompt (question availability, notebook recall pointers).
func (a *sessionAgent) hasTool(name string) bool {
	return a.tools != nil && slices.ContainsFunc(a.tools.Copy(), func(t fantasy.AgentTool) bool {
		return t.Info().Name == name
	})
}

// relWorkdir renders p relative to the working directory when possible,
// keeping the blob short and prompt-portable.
func (a *sessionAgent) relWorkdir(p string) string {
	if a.configStore == nil {
		return p
	}
	if rel, err := filepath.Rel(a.configStore.WorkingDir(), p); err == nil && rel != "" && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
}

// ambiguityDirective implements the turn-zero vagueness pre-filter: a
// short, referent-leaning prompt with no explicit paths and nothing in
// session context to resolve against takes the forced
// clarify-or-state-assumptions path. The gate fires only when no
// resolvable signal exists — a working set or earlier substantive user
// text means the referent has candidates — and stays opt-in behind
// options.ambiguity_clarification.
func (a *sessionAgent) ambiguityDirective(ctx context.Context, call SessionAgentCall, msgs []message.Message, openFailures []cmdlog.Failure) string {
	// An attached file is almost certainly the referent — "fix it"
	// with a file dropped on the prompt needs no clarification.
	if !a.ambiguityClarification || a.isSubAgent || len(call.Attachments) > 0 || !isVaguePrompt(call.Prompt) {
		return ""
	}
	// Earlier substantive user text can supply the referent — a bare
	// greeting or acknowledgement cannot.
	if hasSubstantiveUserMessage(msgs) {
		return ""
	}
	// A non-empty working set or cross-session heat gives the
	// referent candidates.
	if a.filetracker != nil {
		if files, err := a.filetracker.ListRecentReadFiles(ctx, call.SessionID, 1); err == nil && len(files) > 0 {
			return ""
		}
		if hot, err := a.filetracker.ListHotFiles(ctx, call.SessionID, 1); err == nil && len(hot) > 0 {
			return ""
		}
	}
	// An admitted open failure is itself the likely referent — the
	// selector only admits candidates bound to a failure-shaped
	// referent or an explicit path scope, so presence here already
	// implies shape. An all-rejected set must NOT suppress: those
	// candidates could not be bound, and the gate's declare-scope
	// path is exactly what the prompt needs.
	if a.failureMemory && len(openFailures) > 0 {
		return ""
	}
	if a.interactive && a.hasTool(tools.QuestionToolName) {
		return `<ambiguity_gate>
The user's request appears underspecified: it names no files and this
session has no working set or earlier context to resolve the referent
from. Resolve it before executing:

- If a quick search can identify the referent, do so and state the
  assumption in one line.
- Otherwise call the question tool ONCE with a single focused
  clarifying question — single_choice with per-choice tradeoffs, or
  free_text. If the user cannot answer, proceed with your stated-best
  option.
</ambiguity_gate>`
	}
	// Headless degradation: the clause collapses to "state assumptions,
	// proceed" — an unanswerable question must degrade, never stall,
	// and the directive never names a tool the run doesn't have.
	return `<ambiguity_gate>
The user's request appears underspecified: it names no files and this
session has no working set or earlier context to resolve the referent
from. You cannot ask the user in this run — make the most reasonable
assumption, state it in one line, and proceed.
</ambiguity_gate>`
}
