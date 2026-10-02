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
	// turnContextOpenFailuresLimit bounds the failure-memory tail —
	// recent-first, so the cap keeps the freshest unresolved failures.
	turnContextOpenFailuresLimit = 5
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
// crash". The noun list is deliberately referent-shaped and singular:
// "run the tests" acts on the whole suite and stays actionable on its
// own; "the test" names a specific one the context must supply.
var vagueReferentRe = regexp.MustCompile(`(?i)\b(it|its|this|that|them|they)\b|` +
	`\bthe\s+(bug|bugfix|crash|error|errors|failure|fail|issue|problem|panic|regression|leak|typo|warnings?|` +
	`config|configuration|test|spec|endpoint|handler|route|feature|changes?|fix|workaround|hack|todo|fixme)\b`)

// failureReferentNouns are the "the N" referents an open failure can
// be the target of — the failing test, the crash, the regression. A
// vague prompt naming any other noun ("the config", "the endpoint")
// refers to something failure memory cannot supply.
var failureReferentNouns = map[string]bool{
	"bug": true, "bugfix": true, "crash": true, "error": true,
	"errors": true, "failure": true, "fail": true, "panic": true,
	"regression": true, "leak": true, "issue": true, "problem": true,
	"test": true, "spec": true, "warning": true, "warnings": true,
}

var theNounRe = regexp.MustCompile(`(?i)\bthe\s+(\w+)\b`)

// vagueReferentIsFailureShaped reports whether the prompt's referent
// could point at an open failure: an explicit "the <failure-noun>",
// or a bare anaphora ("fix it", "this crashes") — with no noun the
// failing thing is a plausible referent.
func vagueReferentIsFailureShaped(prompt string) bool {
	if m := theNounRe.FindStringSubmatch(prompt); m != nil {
		return failureReferentNouns[strings.ToLower(m[1])]
	}
	return true
}

// isVaguePrompt reports whether the prompt is underspecified in the way
// the pre-filter cares about: short enough to carry no context of its
// own, naming no explicit file paths, and leaning on a referent.
func isVaguePrompt(prompt string) bool {
	n := len(strings.Fields(prompt))
	if n == 0 || n > vaguePromptMaxWords {
		return false
	}
	if len(extractExplicitFilePaths(prompt)) > 0 {
		return false
	}
	return vagueReferentRe.MatchString(prompt)
}

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
	// them and the ambiguity gate reads the same slice.
	var openFailures []cmdlog.Failure
	if a.failureMemory && a.cmdlog != nil {
		if f, err := a.cmdlog.ListOpenFailures(ctx, turnContextOpenFailuresLimit); err == nil {
			openFailures = f
		}
	}
	sections := a.turnContextSections(ctx, call, openFailures)
	if directive := a.ambiguityDirective(ctx, call, msgs, openFailures); directive != "" {
		sections = append(sections, directive)
	}
	if len(sections) == 0 {
		// An armed tail that renders nothing still records an audit:
		// "checked and found nothing" is evidence, distinct from "the
		// tail machinery never ran" — and that difference is what
		// makes an eval's max_tail.sections.* "didn't render"
		// predicate checkable. An unarmed agent still clears a stale
		// audit from an earlier Run in the same process.
		if a.tailArmed() {
			a.recordTailAudit(call.SessionID, nil, "")
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
	a.recordTailAudit(call.SessionID, sections, text)
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
}

var tailSectionNameRe = regexp.MustCompile(`^<(\w+)>`)

// recordTailAudit snapshots the rendered tail for SessionTelemetry.
// Last-write-wins per session: a process's later Run replaces the
// audit, matching the telemetry emission's once-per-process shape.
func (a *sessionAgent) recordTailAudit(sessionID string, sections []string, text string) {
	if a.tailAudit == nil || sessionID == "" {
		return
	}
	sum := sha256.Sum256([]byte(text))
	audit := TailAudit{
		Bytes:  len(text),
		SHA256: hex.EncodeToString(sum[:]),
		Text:   text,
	}
	for _, s := range sections {
		name := "unknown"
		if m := tailSectionNameRe.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
			name = m[1]
		}
		audit.Sections = append(audit.Sections, TailSection{Name: name, Bytes: len(s)})
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
				fmt.Fprintf(&b, ": %s", tailSafeText(truncateTailText(f.Headline, turnContextFailureHeadlineRunes)))
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
	// An open failure is itself the likely referent — but only for a
	// failure-shaped referent or bare anaphora. "Fix the config"
	// names a target the memory cannot supply: suppressing there
	// would disarm the gate for the life of the row.
	if a.failureMemory && len(openFailures) > 0 && vagueReferentIsFailureShaped(call.Prompt) {
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
