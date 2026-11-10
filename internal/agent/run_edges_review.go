package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
)

const (
	// freshReviewCallTimeout bounds the reviewer's model call — the
	// review reads a full diff, so it out-ranks the advisor's 10s
	// note budget but a completion boundary still cannot hang on it.
	freshReviewCallTimeout = 45 * time.Second
	// freshReviewMaxTokens floors the verdict's output budget —
	// findings are terse by contract, but reasoning models spend
	// output on reasoning_content first (advisor documents the
	// truncation), so the solver's own default budget wins when it
	// exceeds the floor.
	freshReviewMaxTokens = 2048
	// freshReviewDiffCap bounds the diff text the reviewer sees —
	// the review judges a delta, not a repository.
	freshReviewDiffCap = 12000
	// freshReviewUntrackedCap bounds per-file content for new files
	// a `git diff HEAD` cannot show.
	freshReviewUntrackedCap = 2048
)

// freshReviewPrefix leads the fresh-review repair section — a
// fingerprint literal distinct from every other edge's (the eval
// analyzer attributes the repair turn by it).
const freshReviewPrefix = "A fresh-context reviewer flagged problems with the completed work"

// freshReviewSystemPrompt frames the reviewer's job (#442): judge
// {task, changed files, diff, check output} — never the worker's
// transcript or its claim of done. The freshness is the arm: the
// same-context self-verification the run already did catches ~61%
// of wrong completions; withholding the worker's own report is what
// buys the rest.
const freshReviewSystemPrompt = "You are a fresh-context reviewer judging whether a coding task's " +
	"work is actually complete. You see only the task, the file list, the workspace diff, and the " +
	"check output the run observed — never the worker's transcript or its report of done. Report " +
	"only real defects: wrong behavior, missing requirements, failing checks, incomplete work. No " +
	"style nits and no speculative improvements. Reply with VERDICT: PASS or VERDICT: ISSUES on the " +
	"first line. For ISSUES follow with one finding per line: file — what is wrong — why it fails " +
	"the task."

// freshVerdict is the deterministic parse of the reviewer's reply.
type freshVerdict struct {
	// issues is the rendered findings text — non-empty fires the edge.
	issues string
	// passed marks a clean VERDICT: PASS.
	passed bool
	// degraded marks every path where the review could not produce a
	// verdict — model error, timeout, truncation, unparseable text.
	// A degraded review must never block a completion claim.
	degraded bool
	// detail joins the firing row's trigger_detail.
	detail string
}

// scanFreshReviewEdge fires when the run makes a completion claim
// over real writes — cleanStop's genuine stop plus a non-empty
// mutation set. Read-only runs carry no completion risk worth a
// review, and StopWhen-ended runs (stalled, verifyLoop) belong to
// their own edges. Opt-in via options.fresh_review — the gated row
// records the would-have-fired verdict for the paired A/B read.
// The one exit that must not be silent: a repair turn that
// "disproves" the findings without writing has no mutating call to
// scan — re-reviewing an unchanged diff could only repeat the
// verdict, so the loop ends on a degraded row plus a terminal
// "findings stand" line the A/B read can count separately.
func (a *sessionAgent) scanFreshReviewEdge(_ context.Context, call SessionAgentCall, in edgeInput) *edgeTrigger {
	if !cleanStop(in) || in.stalled || in.verifyLoop || a.isSubAgent {
		return nil
	}
	if !runHasMutatingCall(in.result.Steps) {
		// A write-less stop on a fresh-review repair turn exits the
		// review loop on the model's own disproof — the exact claim
		// shape the edge distrusts. Re-reviewing an unchanged diff
		// can only repeat the verdict, so the escape stands — but
		// it must not be silent: mark it degraded so the A/B read
		// counts disproof-exits separately from real passes, and
		// resolve appends the terminal "findings stand" line.
		if a.freshReview && strings.Contains(call.Prompt, freshReviewPrefix) {
			return &edgeTrigger{
				steps:     in.result.Steps,
				assistant: in.currentAssistant,
				detail:    "writes=0 disproof=unreviewed",
				hint:      edgeOutcomeDegraded,
			}
		}
		return nil
	}
	writeSet := stepWriteSet(in.result.Steps)
	t := &edgeTrigger{
		steps:    in.result.Steps,
		writeSet: writeSet,
		detail:   fmt.Sprintf("writes=%d", len(writeSet)),
	}
	if !a.freshReview {
		t.hint = edgeOutcomeGated
		return t
	}
	// Fire tentatively — resolve clears on PASS or degraded; the
	// firing row's outcome is the review's verdict.
	t.fire = true
	return t
}

// resolveFreshReviewEdge runs the review: assemble {goal, files,
// diff, check output}, call the solver model in a fresh context, and
// parse the verdict. PASS clears the trigger; ISSUES keeps it firing
// with the findings as the report; anything else degrades — a broken
// reviewer never blocks completion.
func (a *sessionAgent) resolveFreshReviewEdge(ctx context.Context, call SessionAgentCall, t *edgeTrigger) {
	if !runHasMutatingCall(t.steps) {
		// The write-less disproof exit — no model call, just the
		// terminal mark that the flagged diff stands unanswered.
		a.markDisproofExit(ctx, call, t)
		return
	}
	// The handoff pull fills goal, the run's observed check, and
	// workingDir-relative write paths — the same evidence the
	// escalation prompts relay.
	a.pullEscalationHandoff(ctx, call, t)
	v := a.reviewFreshEyes(ctx, call, t)
	t.detail = strings.TrimSpace(t.detail + " " + v.detail)
	switch {
	case v.passed:
		t.fire = false
	case v.degraded:
		t.fire = false
		t.hint = edgeOutcomeDegraded
	default:
		t.report = v.issues
	}
}

// markDisproofExit appends the terminal line for a fresh-review
// repair turn that ended write-less: the findings it was asked to
// fix or disprove stand unanswered — the disproof claim lives in
// the transcript the reviewer never sees, so the loop closes on the
// visible mark, not on a re-review that could only repeat.
func (a *sessionAgent) markDisproofExit(ctx context.Context, _ SessionAgentCall, t *edgeTrigger) {
	if t.assistant == nil || a.messages == nil {
		return
	}
	wctx, cancel := detachedWriteCtx(ctx)
	defer cancel()
	t.assistant.AppendContent("\n\nFresh-review: the findings stand — the repair turn answered " +
		"without writes and the flagged diff is unchanged.")
	if err := a.messages.Update(wctx, *t.assistant); err != nil {
		slog.Error("Failed to record fresh-review disproof exit", "error", err)
	} else if err := a.messages.FlushAll(wctx); err != nil {
		slog.Error("Failed to flush fresh-review disproof exit", "error", err)
	}
}

// reviewFreshEyes calls the session's own solver model with the
// review bundle and returns the parsed verdict. The solver — not a
// configured stronger model — is deliberate: the A/B arm isolates
// context freshness as the variable, not model strength.
func (a *sessionAgent) reviewFreshEyes(ctx context.Context, call SessionAgentCall, t *edgeTrigger) freshVerdict {
	if a.models == nil {
		return freshVerdict{degraded: true, detail: "verdict=degraded reason=no-models"}
	}
	actx, cancel := context.WithTimeout(ctx, freshReviewCallTimeout)
	defer cancel()
	model := a.models(call.SessionID).Large
	if model.Model == nil {
		return freshVerdict{degraded: true, detail: "verdict=degraded reason=no-model"}
	}
	bundle := a.freshReviewBundle(actx, call, t)

	tok := int64(freshReviewMaxTokens)
	if model.CatwalkCfg.CanReason() {
		tok = max(tok, model.CatwalkCfg.DefaultMaxTokens)
	}
	ag := fantasy.NewAgent(
		model.Model,
		fantasy.WithSystemPrompt(freshReviewSystemPrompt),
		fantasy.WithMaxOutputTokens(tok),
		fantasy.WithUserAgent(userAgent),
	)
	resp, err := ag.Stream(actx, fantasy.AgentStreamCall{
		Prompt:  bundle,
		Headers: sessionHeaders(call.SessionID),
	})
	a.recordUsage(call.SessionID, resp)
	if err != nil {
		slog.Warn("Fresh-context review call failed; completing unreviewed",
			"error", err, "session_id", call.SessionID)
		return freshVerdict{degraded: true, detail: "verdict=degraded reason=call-error"}
	}
	if resp == nil || resp.Response.FinishReason == fantasy.FinishReasonLength {
		return freshVerdict{degraded: true, detail: "verdict=degraded reason=truncated"}
	}
	return parseFreshVerdict(resp.Response.Content.Text())
}

// parseFreshVerdict reads the reviewer's first-line contract:
// VERDICT: PASS clears, VERDICT: ISSUES fires with the following
// lines as findings. Anything else — or ISSUES with no findings —
// is unparseable and degrades.
func parseFreshVerdict(text string) freshVerdict {
	head, rest, _ := strings.Cut(strings.TrimSpace(text), "\n")
	head = strings.ToUpper(strings.TrimSpace(head))
	switch {
	case head == "VERDICT: PASS":
		// Exact match — a prefix read would also take "VERDICT:
		// PASSWORD"-shaped noise as a clear.
		return freshVerdict{passed: true, detail: "verdict=pass"}
	case strings.HasPrefix(head, "VERDICT: ISSUES"):
		issues := strings.TrimSpace(rest)
		if issues == "" {
			return freshVerdict{degraded: true, detail: "verdict=degraded reason=no-findings"}
		}
		return freshVerdict{issues: issues, detail: "verdict=issues"}
	}
	return freshVerdict{degraded: true, detail: "verdict=degraded reason=unparseable"}
}

// freshReviewBundle assembles the reviewer's whole world: the task,
// the changed-file list, the workspace diff, and the check output
// the run observed — exactly the issue's evidence set and nothing
// more. No transcript, no completion claim.
func (a *sessionAgent) freshReviewBundle(ctx context.Context, call SessionAgentCall, t *edgeTrigger) string {
	var b strings.Builder
	b.WriteString("A coding agent reports a task complete. Judge it from the evidence below.\n\n")
	if t.goal != "" {
		fmt.Fprintf(&b, "The task: %s\n\n", t.goal)
	}
	if len(t.writeSet) > 0 {
		shown := t.writeSet
		more := ""
		if len(shown) > 10 {
			more = fmt.Sprintf(" +%d more", len(shown)-10)
			shown = shown[:10]
		}
		fmt.Fprintf(&b, "Files this run wrote: %s%s\n\n", strings.Join(shown, ", "), more)
	}
	dir := ""
	if a.configStore != nil {
		dir = a.configStore.WorkingDir()
	}
	b.WriteString(freshReviewDiff(ctx, dir))
	b.WriteString("\n")
	switch {
	case t.lastCheck != nil:
		state := "passed"
		if t.lastCheck.isError {
			state = "failed"
		}
		fmt.Fprintf(&b, "Last check the run observed: `%s` %s",
			truncateTailText(tailSafeText(t.lastCheck.command), 120), state)
		if tail := tailSafeText(lastRunes(strings.TrimSpace(t.lastCheck.output), 800)); tail != "" {
			b.WriteString("\n  tail: " + tail)
		}
		b.WriteString("\n")
	case t.checkFallback != nil:
		chk := t.checkFallback.check
		fmt.Fprintf(&b, "Last check the run observed: `%s` %s",
			truncateTailText(tailSafeText(chk.Check), 80), chk.State)
		if chk.Detail != "" {
			b.WriteString(" — " + truncateTailText(tailSafeText(chk.Detail), 200))
		}
		b.WriteString("\n")
	default:
		b.WriteString("The run observed no check output.\n")
	}
	return b.String()
}

// freshReviewDiff renders the workspace delta the reviewer judges:
// git status for the changed-file roster, git diff HEAD for tracked
// changes (capped), and the heads of untracked files the diff
// cannot show. Outside a git worktree the file list stands alone —
// the review degrades its evidence, never its availability.
func freshReviewDiff(ctx context.Context, dir string) string {
	if dir == "" {
		return "Workspace diff: unavailable — no working directory.\n"
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, serr := freshReviewGit(dctx, dir, "status", "--porcelain")
	diff, derr := freshReviewGit(dctx, dir, "diff", "HEAD", "--")
	if serr != nil && derr != nil {
		return "Workspace diff: unavailable — not a git worktree.\n"
	}
	var b strings.Builder
	b.WriteString("Workspace diff:\n```diff\n")
	b.WriteString(capHeadText(diff, freshReviewDiffCap))
	b.WriteString("\n```\n")
	var newFiles []string
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, "?? ") {
			continue
		}
		p := strings.TrimSpace(line[3:])
		if u, err := strconv.Unquote(p); err == nil {
			p = u
		}
		newFiles = append(newFiles, p)
	}
	for i, p := range newFiles {
		if i >= 3 {
			fmt.Fprintf(&b, "…and %d more new file(s) not shown\n", len(newFiles)-3)
			break
		}
		// Lstat, not Stat — an untracked symlink must not pull its
		// target's content into the reviewer bundle, and a `?? dir/`
		// entry is not a readable file either.
		abs := filepath.Join(dir, filepath.FromSlash(p))
		info, err := os.Lstat(abs)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		content, err := readCapped(abs, freshReviewUntrackedCap)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "--- new file: %s ---\n%s\n", p, content)
	}
	return b.String()
}

// freshReviewGit runs a read-only git command against the workspace.
// Output is capped at capture — a giant worktree diff must not
// buffer unbounded into memory at the boundary; the head is what
// the reviewer reads anyway.
func freshReviewGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var b cappedBuilder
	cmd.Stdout = &b
	err := cmd.Run()
	return b.String(), err
}

// cappedBuilder accepts all writes but keeps only the head — git
// sees a full write so it never blocks on a dead pipe, while the
// captured text stays bounded. Four times the diff cap leaves room
// for the status listing on the same budget.
type cappedBuilder struct {
	b strings.Builder
}

func (w *cappedBuilder) Write(p []byte) (int, error) {
	if rem := freshReviewDiffCap*4 - w.b.Len(); rem > 0 {
		w.b.Write(p[:min(rem, len(p))])
	}
	return len(p), nil
}

func (w *cappedBuilder) String() string { return w.b.String() }

// readCapped reads at most maxRunes worth of a file's head — a
// 500MB untracked artifact must not land in memory for a 2KB slot.
func readCapped(path string, maxRunes int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(maxRunes)*4))
	if err != nil {
		return "", err
	}
	return capHeadText(string(data), maxRunes), nil
}

// capHeadText bounds a block for the review bundle — the head names
// the files the diff touches, so truncation keeps the head and says
// what was cut.
func capHeadText(s string, maxRunes int) string {
	if r := []rune(s); len(r) > maxRunes {
		return string(r[:maxRunes]) + fmt.Sprintf("\n…[%d runes truncated]", len(r)-maxRunes)
	}
	return s
}

// freshReviewPromptSection renders the retry prompt for flagged
// findings: the reviewer's evidence-limited judgment, an instruction
// to fix or disprove each finding concretely. The worker sees the
// findings — the freshness asymmetry is upstream of this prompt.
func freshReviewPromptSection(t *edgeTrigger) string {
	if t == nil || t.report == "" {
		return ""
	}
	return freshReviewPrefix + " — it saw the task, the changed-file list, the diff, and the check " +
		"output, never this transcript or the completion claim:\n\n" + t.report +
		"\n\nFix each finding, or disprove it with concrete evidence (a check that actually passes now)."
}

// freshReviewExhaustNote renders the budget-exhausted terminal line.
func freshReviewExhaustNote(t *edgeTrigger, attempts int) string {
	first := ""
	if t != nil && t.report != "" {
		first, _, _ = strings.Cut(t.report, "\n")
		first = " Last: " + truncateTailText(tailSafeText(first), 120)
	}
	return fmt.Sprintf("Fresh-context review kept flagging problems after %d attempt(s).%s", attempts, first)
}
