package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// EvalTelemetryEnvVar names the file the agent subprocess writes its
// per-run telemetry to when set. It is the eval extraction path for
// numbers that live in-process: steps, usage, stub stats, recalls.
const EvalTelemetryEnvVar = "CRUSH_EVAL_TELEMETRY"

// EvalMaxStepsEnvVar caps a single `crush run`'s steps — the run-side
// enforcement of the trajectory-wide max_steps budget. The driver
// passes the remaining budget (plus one) each turn.
const EvalMaxStepsEnvVar = "CRUSH_EVAL_MAX_STEPS"

// EvalFlagsEnvVar carries the manifest flag names the subprocess
// should report resolved values for.
const EvalFlagsEnvVar = "CRUSH_EVAL_FLAGS"

// EvalRequestVectorEnvVar hands the previous turn process's final
// request fingerprint to the restarted turn's process — the diff
// baseline that keeps a turn-first step's attribution honest
// instead of cold (#115). Kept in sync with
// agent.EvalRequestVectorEnvVar; the constant is duplicated so eval
// doesn't import agent.
const EvalRequestVectorEnvVar = "CRUSH_EVAL_REQUEST_VECTOR"

// EvalTurnsFileEnvVar names the JSON prompt list the persistent-
// process driver hands a single `crush run` subprocess (#117) — the
// child loops the turns on one session instead of restarting per
// turn. Kept in sync with app.EvalTurnsFileEnvVar; the constant is
// duplicated so eval doesn't import app.
const EvalTurnsFileEnvVar = "CRUSH_EVAL_TURNS_FILE"

// RunResult is what one trajectory run (all turns) produced.
type RunResult struct {
	Steps       int
	Tokens      TokenUsage
	StubStats   StubStats
	PriorTurns  PriorTurns
	Recalls     Recalls
	Checkpoints Checkpoints
	Digests     Checkpoints
	Hydration   Hydration
	// PromptTokensPerTurn is the growth curve: each turn's last
	// request's normalized prompt tokens (input + cache write +
	// cache read). Flat across turns means the context machinery
	// holds the rendered request down — the benefit claim.
	PromptTokensPerTurn []int64
	// Request carries the trajectory-final request's composition
	// (system/notebook/history/tool bytes) and the run's peak
	// prompt size. Informational, never gating.
	Request RequestStats
	// EdgeFirings is the trajectory-wide edge/outcome firing split —
	// the summed per-turn deltas (each `crush run` process's counters
	// are in-memory and reset on spawn).
	EdgeFirings map[string]map[string]int
	SessionID   string
	// ParamVersion is the child's resolved memory-parameter snapshot
	// identity (#228) — identical across the run's turn processes;
	// empty on children predating the substrate.
	ParamVersion  string
	ModelResolved string
	ModelSmall    string
	ModelSummary  string
	// ResolvedOptions is the child's report of what each manifest
	// flag resolved to — the truth the baseline key hashes.
	ResolvedOptions map[string]any
	// RequestVector is the trajectory-final process's request
	// fingerprint — forwarded verbatim to the next turn's process
	// (#115). Raw because the driver never inspects the contents.
	RequestVector json.RawMessage
	// Drains is each turn's detached-work join outcome — the
	// lifecycle evidence that a restart boundary settled cleanly,
	// Turn stamped at fold.
	Drains []DrainReport
	// StepRecords is the trajectory-wide per-step table — every
	// turn's steps with usage and prefix attribution, Turn stamped
	// at fold time.
	StepRecords []StepRecord
	// Tail is the trajectory-wide per-turn tail audit — each turn's
	// rendered context envelopes with sizes, digest, and text, Turn
	// stamped at fold time. Absent on turns where no tail rendered.
	Tail []TurnTail
	// TailRuns is the trajectory-wide per-Run tail audit history —
	// every tail each turn's process rendered, Turn and Run
	// attributed, so a turn's repair-chain renders stay
	// individually inspectable (#249).
	TailRuns []TurnTail
	// Pressure carries the pressure gate's trajectory-wide state —
	// summed engage transitions and the last turn's latch and
	// estimate.
	Pressure Pressure
	// GeneratorTokens is the summed sidecar generation spend across
	// the trajectory's turns.
	GeneratorTokens GeneratorTokens
	// ErrorClass is the erroring turn's typed classification —
	// what the breaker reads.
	ErrorClass string
	// TimedOut is set when the run hit the trajectory's
	// run_timeout_seconds or max_steps budget.
	TimedOut bool
	// Err is set when the run failed in transport/agent machinery —
	// the model didn't produce the outcome.
	Err error
}

// AgentRunner executes the agent against a prepared workdir. The
// interface exists so tests drive the harness without a provider.
type AgentRunner interface {
	Run(ctx context.Context, workdir string, turns []string, budget Budget) RunResult
}

// CrushRunner drives runs through the crush binary under test —
// os.Executable(), so both arms provably run the same build. Each turn
// is a `crush run` subprocess in the materialized workdir with a pinned
// HOME/XDG: the global config layers merge into every run and would
// otherwise leak a dev laptop's options into results. Credentials come
// from the eval environment — they pass through.
type CrushRunner struct {
	Bin string // os.Executable() when empty is resolved at Run time
	// Home is the pinned HOME for subprocesses; XDG dirs are derived
	// from it.
	Home string
	// ExtraEnv entries override os.Environ for the subprocess — they
	// precede inherited vars so duplicates resolve to these values.
	// Harness-pinned keys (HOME, telemetry, flags) still win.
	ExtraEnv []string
	// FlagKeys are the manifest flag names the child reports resolved
	// values for, via CRUSH_EVAL_FLAGS.
	FlagKeys []string
}

// runTelemetry is the JSON the agent subprocess drops at
// CRUSH_EVAL_TELEMETRY.
type runTelemetry struct {
	SessionID string `json:"session_id"`
	// ParamVersion is the child's resolved memory-parameter snapshot
	// identity (#228) — the field a cohort split attributes outcomes
	// to. Constant per process; empty on children predating the
	// substrate.
	ParamVersion string `json:"param_version"`
	Steps        int    `json:"steps"`
	Tokens       struct {
		Input      int64 `json:"input"`
		Output     int64 `json:"output"`
		CacheRead  int64 `json:"cache_read"`
		CacheWrite int64 `json:"cache_write"`
	} `json:"tokens"`
	StubStats struct {
		Invalidations    int            `json:"invalidations"`
		Results          int            `json:"results"`
		SavedBytes       int64          `json:"saved_bytes"`
		BoundaryAdvances int            `json:"boundary_advances"`
		Kinds            map[string]int `json:"kinds"`
	} `json:"stub_stats"`
	// PriorTurns carries the collapse telemetry: each turn counts once
	// trajectory-wide because the persisted collapsed_turns row is the
	// dedupe across the per-turn subprocesses.
	PriorTurns struct {
		TurnsCollapsed  int `json:"turns_collapsed"`
		EventsCollapsed int `json:"events_collapsed"`
	} `json:"prior_turns"`
	Recalls struct {
		Result          int `json:"result"`
		Entry           int `json:"entry"`
		Empty           int `json:"empty"`
		Cross           int `json:"cross"`
		PriorTurnResult int `json:"prior_turn_result"`
	} `json:"recalls"`
	// Checkpoints carries the checkpoint telemetry: written counts
	// committed checkpoint entries, rendered counts prefix renders
	// that included one — the "present at render" signal.
	Checkpoints struct {
		Written  int `json:"written"`
		Rendered int `json:"rendered"`
	} `json:"checkpoints"`
	// Digests carries the turn-digest telemetry, same written/
	// rendered split — the "digest present at render" signal that
	// measures the async generation race.
	Digests struct {
		Written  int `json:"written"`
		Rendered int `json:"rendered"`
	} `json:"digests"`
	// Hydration carries the cold-start seed telemetry: seeds counts
	// mem0-sourced entries SeedEntries committed this turn's process
	// (the marker suppresses re-seeding, so only the seeding turn
	// reports a nonzero count), plan_seeds the locally sourced plan
	// seed; rendered counts prefix renders that included a
	// hydrated-tagged entry.
	Hydration struct {
		Seeds     int `json:"seeds"`
		PlanSeeds int `json:"plan_seeds"`
		Rendered  int `json:"rendered"`
	} `json:"hydration"`
	// Request carries the prompt growth-curve sample and the last
	// rendered request's composition — the flat-vs-growing signal
	// the benefit measurement reads. Informational, never gating.
	// Steps is the per-request table: usage plus the prefix
	// attribution naming each cache miss's cause.
	Request struct {
		PromptRequests   int64        `json:"prompt_requests"`
		PromptTokensLast int64        `json:"prompt_tokens_last"`
		PromptTokensPeak int64        `json:"prompt_tokens_peak"`
		SystemBytes      int64        `json:"system_bytes"`
		NotebookBytes    int64        `json:"notebook_bytes"`
		HistoryBytes     int64        `json:"history_bytes"`
		ToolCallBytes    int64        `json:"tool_call_bytes"`
		ToolResultBytes  int64        `json:"tool_result_bytes"`
		Steps            []StepRecord `json:"steps"`
	} `json:"request"`
	// GeneratorTokens is the notebook sidecar's generation spend —
	// segment/checkpoint/digest LLM calls the run totals can't see.
	GeneratorTokens struct {
		Calls      int   `json:"calls"`
		Input      int64 `json:"input"`
		Output     int64 `json:"output"`
		CacheRead  int64 `json:"cache_read"`
		CacheWrite int64 `json:"cache_write"`
	} `json:"generator_tokens"`
	// Pressure carries the gate's state: activations counts engage
	// transitions (the "did it fire" predicate — the latch makes it
	// at most one per process), engaged is the latch at snapshot
	// time, estimate the last next-request estimate for the
	// estimate-vs-reported audit.
	Pressure struct {
		Activations int   `json:"activations"`
		Engaged     bool  `json:"engaged"`
		Estimate    int64 `json:"estimate"`
	} `json:"pressure"`
	// Tail is the turn's ephemeral-tail audit — which context
	// envelopes the model saw. Pointer-gated: nil when no tail
	// rendered, so "no tail" doesn't alias "telemetry missing".
	Tail *TurnTail `json:"tail,omitempty"`
	// TailRuns is the process's per-Run tail audit history — every
	// rendered tail in order, attributed by run_stamp and
	// repair_attempts (#249).
	TailRuns []TurnTail `json:"tail_runs,omitempty"`
	// EdgeFirings splits run-boundary edge firing counts by edge and
	// outcome — the per-turn delta of the session's edge_firings rows
	// this process recorded (repair retries share the process).
	EdgeFirings  map[string]map[string]int `json:"edge_firings"`
	Model        string                    `json:"model"`
	ModelSmall   string                    `json:"model_small"`
	ModelSummary string                    `json:"model_summary"`
	// ResolvedOptions is the child's effective config projected onto
	// the manifest flags — what actually ran, not what the arm asked.
	ResolvedOptions map[string]any `json:"resolved_options"`
	// RequestVector is the process's final request fingerprint —
	// forwarded verbatim to the next turn's process via
	// CRUSH_EVAL_REQUEST_VECTOR so restart attribution diffs
	// against it instead of reporting cold (#115). Raw because the
	// driver never inspects the contents — the agent package owns
	// the schema.
	RequestVector json.RawMessage `json:"request_vector,omitempty"`
	// Drain is this process's detached-work join outcome — nil when
	// the child predates #115 or died before emitting.
	Drain *DrainReport `json:"drain,omitempty"`
	Error string       `json:"error,omitempty"`
	// ErrorClass is the child's typed classification of the terminal
	// error — auth/provider_deterministic/provider_server/
	// rate_limit/context_too_large/window_cap_enforced/
	// provider_unreachable/provider_transient/cancelled/timeout. The
	// circuit breaker reads it instead of string-matching when
	// present.
	ErrorClass string `json:"error_class,omitempty"`
}

// TurnRunner is the counterfactual-replay contract (#108): execute a
// single trajectory turn against the session state already in
// workdir's crush.db. An empty sessionID starts a fresh session —
// turn 0 of a recording pass or an unseeded fork. prevVector is the
// previous turn process's request fingerprint (#115) — the diff
// baseline the restarted process needs; nil means cold. The returned
// string is the session the turn ran under, for the caller's
// continuation bookkeeping; RunResult carries just this turn's
// telemetry.
type TurnRunner interface {
	RunTurn(ctx context.Context, workdir, sessionID, prompt string, turnIdx, maxSteps int, prevVector json.RawMessage) (string, RunResult)
}

// turnOutcome is one `crush run` subprocess's classified result — the
// error/timeout split Run and replayed turns both apply.
type turnOutcome struct {
	tel      runTelemetry
	err      error
	errClass string
	timedOut bool
}

// Run executes the trajectory's turns sequentially — each turn a
// `crush run` subprocess continuing the same session. Turn i+1 starts
// only after turn i's process exits, which is after its terminal
// RunComplete; gate retries share the run's correlation and suppress
// the premature completion event, so the subprocess boundary satisfies
// the "follow-ups settle" sequencing rule.
func (c CrushRunner) Run(ctx context.Context, workdir string, turns []string, budget Budget) RunResult {
	var res RunResult
	deadline := time.Now().Add(time.Duration(budget.RunTimeoutSeconds) * time.Second)
	if budget.RunTimeoutSeconds <= 0 {
		deadline = time.Now().Add(15 * time.Minute)
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var sessionID string
	for i, turn := range turns {
		// A multi-turn trajectory must continue the SAME session —
		// launching turn i+1 without --session silently degrades to a
		// fresh session and the check may still pass.
		if i > 0 && sessionID == "" {
			res.Err = fmt.Errorf("turn %d: session continuation lost — turn %d's telemetry had no session_id", i+1, i)
			return res
		}
		if budget.MaxSteps > 0 && res.Steps >= budget.MaxSteps {
			// Trajectory-wide budget already consumed — don't launch
			// the next turn at all.
			res.TimedOut = true
			return res
		}
		to := c.runTurnOnce(ctx, workdir, sessionID, turn, i, remainingSteps(budget, res.Steps), res.RequestVector)
		res.addTurnTelemetry(to.tel, i)
		noteTurnFields(&res, to.tel)
		if res.SessionID != "" {
			sessionID = res.SessionID
		}
		if to.err != nil {
			res.Err = to.err
			res.ErrorClass = to.errClass
			return res
		}
		if to.timedOut {
			res.TimedOut = true
			return res
		}
	}
	return res
}

// RunTurn executes one trajectory turn — the counterfactual-replay
// contract (#108). stepCap follows remainingSteps' convention: the
// subprocess may consume the budget's remainder plus one; reaching
// the cap classifies timeout. turnIdx stamps StepRecords/TailRows and
// names the telemetry file — replay passes the trajectory-space index
// so per-step rows attribute to the replayed turn. prevVector seeds
// the child's restart attribution — a fork passes the recorded
// boundary's vector so the replayed turn diffs against the prefix it
// actually continues (#115).
func (c CrushRunner) RunTurn(ctx context.Context, workdir, sessionID, prompt string, turnIdx, stepCap int, prevVector json.RawMessage) (string, RunResult) {
	var res RunResult
	to := c.runTurnOnce(ctx, workdir, sessionID, prompt, turnIdx, stepCap, prevVector)
	res.addTurnTelemetry(to.tel, turnIdx)
	noteTurnFields(&res, to.tel)
	res.Err = to.err
	res.ErrorClass = to.errClass
	res.TimedOut = to.timedOut
	if res.SessionID != "" {
		return res.SessionID, res
	}
	return sessionID, res
}

// noteTurnFields carries a turn's identity fields — session, resolved
// models, resolved flag values — onto the run result. Shared by the
// trajectory loop and single-turn replay.
func noteTurnFields(res *RunResult, tel runTelemetry) {
	if tel.SessionID != "" {
		res.SessionID = tel.SessionID
	}
	if tel.Model != "" {
		res.ModelResolved = tel.Model
	}
	if tel.ModelSmall != "" {
		res.ModelSmall = tel.ModelSmall
	}
	if tel.ModelSummary != "" {
		res.ModelSummary = tel.ModelSummary
	}
	if len(tel.ResolvedOptions) > 0 {
		res.ResolvedOptions = tel.ResolvedOptions
	}
}

// runTurnOnce spawns one `crush run` subprocess for a single prompt
// and classifies the result. Telemetry lives beside the workdir, not
// inside it: check.sh must see the tree exactly as the agent left it
// — untracked harness litter could flip a globbing check. The
// session/model bookkeeping fields land on res through
// addTurnTelemetry and noteTurnFields.
func (c CrushRunner) runTurnOnce(ctx context.Context, workdir, sessionID, prompt string, turnIdx, stepCap int, prevVector json.RawMessage) turnOutcome {
	tfile := filepath.Join(filepath.Dir(workdir), fmt.Sprintf(".eval-telemetry-%s-%d.json", filepath.Base(workdir), turnIdx))
	args := []string{"run", "--quiet"}
	if sessionID != "" {
		args = append(args, "--session", sessionID)
	}
	args = append(args, "--", prompt)

	bin := c.Bin
	if bin == "" {
		bin, _ = os.Executable()
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// SIGINT (not Kill) on deadline/cancel: `crush run` translates
	// it into ctx cancellation, giving the child a beat to write
	// telemetry for the timeout/error carve-out. WaitDelay bounds
	// the grace before the hard kill.
	cmd.Cancel = func() error {
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 10 * time.Second
	cmd.Dir = workdir
	cmd.Env = c.subprocessEnv(tfile, stepCap, prevVector)
	out, err := cmd.CombinedOutput()

	tel, telErr := readTelemetry(tfile)
	_ = os.Remove(tfile)
	var to turnOutcome
	to.tel = tel

	// Timeout/error carve-out: a run that hit the deadline while
	// its API calls were already erroring classifies as `error`,
	// not `timeout` — the child's graceful-cancel telemetry says
	// which. A clean cancellation (our own signal) is a timeout.
	switch {
	case tel.Error != "" && !isCancellation(tel.Error):
		to.err = fmt.Errorf("agent run failed: %s", tel.Error)
		to.errClass = tel.ErrorClass
	case ctx.Err() == context.DeadlineExceeded || (stepCap > 0 && tel.Steps >= stepCap):
		// The step cap is the trajectory remainder plus one — a
		// turn that consumed it was stopped mid-flight.
		to.timedOut = true
	case ctx.Err() != nil:
		to.err = ctx.Err()
	case err != nil:
		to.err = fmt.Errorf("crush run failed: %w: %s", err, tail(out, 4096))
	case telErr != nil:
		// The child exited clean but the telemetry contract broke
		// — zeroed stats would read as coverage-starved
		// inconclusive instead of the error this is.
		to.err = fmt.Errorf("telemetry unreadable after clean run: %w", telErr)
	case tel.Error != "":
		to.err = fmt.Errorf("agent run failed: %s", tel.Error)
		to.errClass = tel.ErrorClass
	}
	return to
}

// addTurnTelemetry folds one turn's telemetry counters into the run
// totals. Per-turn counters reset with each fresh `crush run` process
// (the stats maps are in-memory per session, not rehydrated on
// --session resume), so the trajectory totals are the SUM of per-turn
// deltas, not the last turn's value — per-kind counts included.
// turn is the trajectory turn index — stamped onto the per-step rows
// so the table is ordered across process boundaries.
func (res *RunResult) addTurnTelemetry(tel runTelemetry, turn int) {
	res.Steps += tel.Steps
	// Process-constant: the first turn to report it wins; later
	// turns carry the same value.
	if res.ParamVersion == "" {
		res.ParamVersion = tel.ParamVersion
	}
	res.Tokens.Input += tel.Tokens.Input
	res.Tokens.Output += tel.Tokens.Output
	res.Tokens.CacheRead += tel.Tokens.CacheRead
	res.Tokens.CacheWrite += tel.Tokens.CacheWrite
	res.StubStats.Invalidations += tel.StubStats.Invalidations
	res.StubStats.Results += tel.StubStats.Results
	res.StubStats.SavedBytes += tel.StubStats.SavedBytes
	res.StubStats.BoundaryAdvances += tel.StubStats.BoundaryAdvances
	if len(tel.StubStats.Kinds) > 0 {
		if res.StubStats.Kinds == nil {
			res.StubStats.Kinds = make(map[string]int, len(tel.StubStats.Kinds))
		}
		for kind, n := range tel.StubStats.Kinds {
			res.StubStats.Kinds[kind] += n
		}
	}
	res.PriorTurns.TurnsCollapsed += tel.PriorTurns.TurnsCollapsed
	res.PriorTurns.EventsCollapsed += tel.PriorTurns.EventsCollapsed
	res.Recalls.Result += tel.Recalls.Result
	res.Recalls.Entry += tel.Recalls.Entry
	res.Recalls.Empty += tel.Recalls.Empty
	res.Recalls.Cross += tel.Recalls.Cross
	res.Recalls.PriorTurnResult += tel.Recalls.PriorTurnResult
	res.Checkpoints.Written += tel.Checkpoints.Written
	// Prompt-curve and composition are per-turn snapshots, not
	// sums: append each turn's last-request prompt size (the curve)
	// and keep the latest turn's composition plus the max peak.
	if tel.Request.PromptTokensLast > 0 {
		res.PromptTokensPerTurn = append(res.PromptTokensPerTurn, tel.Request.PromptTokensLast)
	}
	res.Request.PromptRequests += tel.Request.PromptRequests
	if tel.Request.PromptTokensPeak > res.Request.PromptTokensPeak {
		res.Request.PromptTokensPeak = tel.Request.PromptTokensPeak
	}
	res.Request.SystemBytes = tel.Request.SystemBytes
	res.Request.NotebookBytes = tel.Request.NotebookBytes
	res.Request.HistoryBytes = tel.Request.HistoryBytes
	res.Request.ToolCallBytes = tel.Request.ToolCallBytes
	res.Request.ToolResultBytes = tel.Request.ToolResultBytes
	for _, s := range tel.Request.Steps {
		s.Turn = turn
		res.StepRecords = append(res.StepRecords, s)
	}
	// The process's final fingerprint rides forward — the next
	// turn's restarted process diffs its first request against it
	// (#115).
	if len(tel.RequestVector) > 0 {
		res.RequestVector = tel.RequestVector
	}
	if tel.Drain != nil {
		d := *tel.Drain
		d.Turn = turn
		res.Drains = append(res.Drains, d)
	}
	if tel.Tail != nil {
		t := *tel.Tail
		t.Turn = turn
		res.Tail = append(res.Tail, t)
	}
	for _, tr := range tel.TailRuns {
		tr.Turn = turn
		res.TailRuns = append(res.TailRuns, tr)
	}
	res.GeneratorTokens.Calls += tel.GeneratorTokens.Calls
	res.GeneratorTokens.Input += tel.GeneratorTokens.Input
	res.GeneratorTokens.Output += tel.GeneratorTokens.Output
	res.GeneratorTokens.CacheRead += tel.GeneratorTokens.CacheRead
	res.GeneratorTokens.CacheWrite += tel.GeneratorTokens.CacheWrite
	// Pressure state is latch-like across the trajectory's
	// subprocesses: activations sum, engaged ORs (an engaged turn
	// re-engages on the next process's cold-start estimate), and
	// estimate keeps the latest non-zero for the audit.
	res.Pressure.Activations += tel.Pressure.Activations
	res.Pressure.Engaged = res.Pressure.Engaged || tel.Pressure.Engaged
	if tel.Pressure.Estimate > 0 {
		res.Pressure.Estimate = tel.Pressure.Estimate
	}
	res.Checkpoints.Rendered += tel.Checkpoints.Rendered
	res.Digests.Written += tel.Digests.Written
	res.Digests.Rendered += tel.Digests.Rendered
	res.Hydration.Seeds += tel.Hydration.Seeds
	res.Hydration.PlanSeeds += tel.Hydration.PlanSeeds
	res.Hydration.Rendered += tel.Hydration.Rendered
	for edge, outcomes := range tel.EdgeFirings {
		if res.EdgeFirings == nil {
			res.EdgeFirings = map[string]map[string]int{}
		}
		if res.EdgeFirings[edge] == nil {
			res.EdgeFirings[edge] = map[string]int{}
		}
		for outcome, n := range outcomes {
			res.EdgeFirings[edge][outcome] += n
		}
	}
}

// remainingSteps converts the trajectory-wide max_steps budget into
// the child process's per-run cap: the run may consume the remaining
// budget plus one step — hitting the cap means it was stopped
// mid-flight (timeout), while finishing within it is a normal run.
func remainingSteps(budget Budget, used int) int {
	if budget.MaxSteps <= 0 {
		return 0
	}
	return budget.MaxSteps - used + 1
}

// isCancellation identifies the telemetry error the child writes when
// it was stopped by our SIGINT rather than by a failure of its own.
func isCancellation(e string) bool {
	return strings.Contains(e, "context canceled") ||
		strings.Contains(e, "context deadline") ||
		strings.Contains(e, "request canceled by user")
}

// subprocessEnv builds the run's environment: the eval environment's
// credentials pass through; HOME and the XDG dirs are pinned so the
// global config layers merge nothing in. The parent's own CRUSH_* vars
// are stripped — a CRUSH_CLIENT_SERVER=1 left over in the operator's
// env would take the client/server path where the telemetry hook
// doesn't fire, silently breaking session continuation.
func (c CrushRunner) subprocessEnv(telemetryFile string, maxSteps int, prevVector json.RawMessage) []string {
	pinned := map[string]string{
		"HOME":              c.Home,
		"XDG_CONFIG_HOME":   filepath.Join(c.Home, ".config"),
		"XDG_DATA_HOME":     filepath.Join(c.Home, ".local", "share"),
		"XDG_STATE_HOME":    filepath.Join(c.Home, ".local", "state"),
		EvalTelemetryEnvVar: telemetryFile,
	}
	if maxSteps > 0 {
		pinned[EvalMaxStepsEnvVar] = strconv.Itoa(maxSteps)
	}
	if len(prevVector) > 0 {
		pinned[EvalRequestVectorEnvVar] = string(prevVector)
	}
	if len(c.FlagKeys) > 0 {
		pinned[EvalFlagsEnvVar] = strings.Join(c.FlagKeys, ",")
	}
	// Go caches share the eval-wide cache, not the per-run home — see
	// goCachePins. ExtraEnv-reserved keys keep caller intent.
	reserved := map[string]bool{}
	for _, kv := range c.ExtraEnv {
		k, _, _ := strings.Cut(kv, "=")
		reserved[k] = true
	}
	for k, v := range goCachePins(reserved) {
		pinned[k] = v
	}
	// Replace rather than append: duplicated keys in environ are
	// resolved first-match by getenv, so a second HOME wouldn't pin.
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		// Harness-prefixed vars never inherit — an exported
		// EVAL_WORKDIR or CRUSH_MODE would shadow/derail the run.
		if strings.HasPrefix(k, "CRUSH_") || strings.HasPrefix(k, "EVAL_") {
			continue
		}
		if _, overridden := pinned[k]; !overridden {
			env = append(env, kv)
		}
	}
	// exec.Cmd.Env dedupes LAST-wins: the filtered parent env first,
	// then ExtraEnv (caller intent overrides inherited vars), then
	// pinned (harness invariants override everything).
	out := make([]string, 0, len(env)+len(pinned)+len(c.ExtraEnv))
	out = append(out, env...)
	out = append(out, c.ExtraEnv...)
	for k, v := range pinned {
		out = append(out, k+"="+v)
	}
	return out
}

// PersistentRunner is the persistent-process regime (#117): the whole
// trajectory runs inside ONE `crush run` subprocess — production's
// process model — so the in-memory state restart mode rebuilds each
// turn (prefix cache, prev-request vector, notebook high-water)
// survives the boundary, and detached work lives past its turn — the
// measurement point, not a bug. The child reads the prompt list from
// CRUSH_EVAL_TURNS_FILE and drops per-turn telemetry DELTAS at
// <base>-<i> — same fold as restart mode on the driver side. The
// child decrements CRUSH_EVAL_MAX_STEPS between turns, mirroring the
// restart driver's remaining-budget cap; the post-hoc overrun check
// below is the backstop for steps a dying turn never reported.
type PersistentRunner struct{ CrushRunner }

// Run executes all turns in one subprocess. Failure classification
// mirrors runTurnOnce: a non-cancellation telemetry error fails the
// run; a deadline or budget overrun marks TimedOut.
func (p PersistentRunner) Run(ctx context.Context, workdir string, turns []string, budget Budget) RunResult {
	var res RunResult
	deadline := time.Now().Add(time.Duration(budget.RunTimeoutSeconds) * time.Second)
	if budget.RunTimeoutSeconds <= 0 {
		deadline = time.Now().Add(15 * time.Minute)
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	dir := filepath.Dir(workdir)
	base := filepath.Base(workdir)
	turnsFile := filepath.Join(dir, fmt.Sprintf(".eval-turns-%s.json", base))
	data, err := json.Marshal(turns)
	if err != nil {
		res.Err = fmt.Errorf("marshal turns file: %w", err)
		return res
	}
	if err := os.WriteFile(turnsFile, data, 0o600); err != nil {
		res.Err = fmt.Errorf("write turns file: %w", err)
		return res
	}
	defer os.Remove(turnsFile)

	// The child appends -<i> per turn; the base keeps the restart
	// mode's file-naming convention.
	telBase := filepath.Join(dir, fmt.Sprintf(".eval-telemetry-%s", base))
	bin := p.Bin
	if bin == "" {
		bin, _ = os.Executable()
	}
	cmd := exec.CommandContext(ctx, bin, "run", "--quiet")
	cmd.Cancel = func() error {
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 10 * time.Second
	cmd.Dir = workdir
	// Appended after pinned: the turns file is a harness invariant —
	// an ExtraEnv entry must not shadow it.
	cmd.Env = append(p.subprocessEnv(telBase, budget.MaxSteps, nil),
		EvalTurnsFileEnvVar+"="+turnsFile)
	out, runErr := cmd.CombinedOutput()

	emitted := 0
	for i := 0; ; i++ {
		tfile := fmt.Sprintf("%s-%d", telBase, i)
		tel, telErr := readTelemetry(tfile)
		if telErr != nil {
			if errors.Is(telErr, os.ErrNotExist) {
				break
			}
			res.Err = fmt.Errorf("telemetry unreadable after turn %d: %w", i, telErr)
			return res
		}
		_ = os.Remove(tfile)
		emitted++
		res.addTurnTelemetry(tel, i)
		noteTurnFields(&res, tel)
		if tel.Error != "" && !isCancellation(tel.Error) {
			res.Err = fmt.Errorf("agent run failed: %s", tel.Error)
			res.ErrorClass = tel.ErrorClass
			return res
		}
	}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.TimedOut = true
	case ctx.Err() != nil:
		res.Err = ctx.Err()
	case runErr != nil:
		res.Err = fmt.Errorf("crush run failed: %w: %s", runErr, tail(out, 4096))
	case emitted < len(turns):
		// Clean exit short of the turn list means the child stopped
		// mid-trajectory without reporting — classify as a broken
		// run, not a short trajectory.
		res.Err = fmt.Errorf("persistent run emitted %d of %d turns without error", emitted, len(turns))
	case budget.MaxSteps > 0 && res.Steps > budget.MaxSteps:
		// The child's step cap is per-turn; the trajectory-wide
		// budget enforces here.
		res.TimedOut = true
	}
	return res
}

func readTelemetry(path string) (runTelemetry, error) {
	var t runTelemetry
	data, err := os.ReadFile(path)
	if err != nil {
		return t, err
	}
	return t, json.Unmarshal(data, &t)
}

// tail keeps the last n bytes of output — the diagnostically useful
// end without the bulk.
func tail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}
