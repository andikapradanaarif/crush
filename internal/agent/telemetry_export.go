package agent

import (
	"os"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/notebook"
)

// SessionTelemetry is the per-session counter snapshot the eval
// harness records into run records — the stub-track and
// notebook-recall splits that a flat count can't express.
type SessionTelemetry struct {
	StubInvalidations int   `json:"invalidations"`
	StubResults       int   `json:"results"`
	StubSavedBytes    int64 `json:"saved_bytes"`
	BoundaryAdvances  int   `json:"boundary_advances"`
	// StubKinds splits StubResults by stub kind, keyed by the kind's
	// telemetry label — the empty-string superseded kind surfaces as
	// "superseded", never "".
	StubKinds     map[string]int `json:"kinds,omitempty"`
	ResultRecalls int            `json:"result_recalls"`
	EntryRecalls  int            `json:"entry_recalls"`
	EmptyRecalls  int            `json:"empty_recalls"`
	CrossRecalls  int            `json:"cross_recalls"`
	// PriorTurnResultRecalls counts result: recalls that resolved to
	// a call in a prior turn — the recall-into-collapsed approximation.
	PriorTurnResultRecalls int `json:"prior_turn_result_recalls"`
	// Checkpoint telemetry: written counts committed checkpoint
	// entries; rendered counts prefix renders that included one.
	// Boundary/session granularity only — turn digests split off
	// into the digests counters.
	CheckpointsWritten int `json:"checkpoints_written"`
	CheckpointRenders  int `json:"checkpoint_renders"`
	// Digest telemetry: written counts committed granularity:turn
	// digests; rendered counts prefix renders that included one —
	// the "digest present at render" signal that measures the async
	// generation race.
	DigestsWritten int `json:"digests_written"`
	DigestRenders  int `json:"digest_renders"`
	// Hydration telemetry: seeds counts committed mem0-sourced seed
	// entries, plan_seeds the locally sourced plan seed; renders
	// counts prefix renders that included a hydrated-tagged entry —
	// the cold-start arm's "did the mechanism fire" coverage signal.
	HydrationSeeds     int `json:"hydration_seeds"`
	HydrationPlanSeeds int `json:"hydration_plan_seeds"`
	HydrationRenders   int `json:"hydration_renders"`
	// GeneratorCalls and Generator*Tokens account the sidecar LLM
	// calls that produced notebook entries — spend invisible in the
	// run's token totals.
	GeneratorCalls            int   `json:"generator_calls"`
	GeneratorInputTokens      int64 `json:"generator_input_tokens"`
	GeneratorOutputTokens     int64 `json:"generator_output_tokens"`
	GeneratorCacheReadTokens  int64 `json:"generator_cache_read_tokens"`
	GeneratorCacheWriteTokens int64 `json:"generator_cache_write_tokens"`
	// Prior-turn collapse telemetry: distinct turns collapsed and the
	// call/result pairs inside them — deduped against the persisted
	// collapsed_turns rows.
	TurnsCollapsed  int `json:"turns_collapsed"`
	EventsCollapsed int `json:"events_collapsed"`
	// SummaryFetchFailed marks a summarize-mode run whose entry fetch
	// failed — the run rendered verbatim, a control-shaped prompt
	// inside the treatment arm.
	SummaryFetchFailed bool `json:"summary_fetch_failed,omitempty"`
	// Request telemetry: the prompt growth curve (last/peak
	// normalized prompt tokens across the run's steps) and the last
	// rendered request's content-byte composition. This is the
	// flat-vs-growing signal the benefit measurement reads —
	// informational only, never gating.
	PromptRequests   int64 `json:"prompt_requests"`
	PromptTokensLast int64 `json:"prompt_tokens_last"`
	PromptTokensPeak int64 `json:"prompt_tokens_peak"`
	// Steps is the per-request usage + prefix-attribution table —
	// the cache-miss forensics the run aggregate can't carry.
	Steps              []StepRecord `json:"steps,omitempty"`
	ReqSystemBytes     int64        `json:"req_system_bytes"`
	ReqNotebookBytes   int64        `json:"req_notebook_bytes"`
	ReqHistoryBytes    int64        `json:"req_history_bytes"`
	ReqToolCallBytes   int64        `json:"req_tool_call_bytes"`
	ReqToolResultBytes int64        `json:"req_tool_result_bytes"`
	// EdgeFirings splits run-boundary edge firing counts by edge and
	// outcome — the in-memory mirror of the edge_firings rows this
	// process wrote. Cumulative for the process; the eval harness
	// emits EdgeFiringDelta instead so multi-emission processes
	// can't double-count.
	EdgeFirings map[string]map[string]int `json:"edge_firings,omitempty"`
}

// SessionTelemetry returns the coordinator's per-session counters.
// Deliberately not on the Coordinator interface — the eval harness
// type-asserts for it so test stubs needn't implement it. Zero value
// when the agent is absent or the session has no accumulated counters.
func (c *coordinator) SessionTelemetry(sessionID string) SessionTelemetry {
	sa, ok := c.currentAgent().(*sessionAgent)
	if !ok || sa == nil {
		return SessionTelemetry{}
	}
	var t SessionTelemetry
	if s, ok := sa.stubStats.Get(sessionID); ok {
		t.StubInvalidations = s.Invalidations
		t.StubResults = s.Results
		t.StubSavedBytes = s.SavedBytes
		t.BoundaryAdvances = s.BoundaryAdvances
		t.TurnsCollapsed = s.TurnsCollapsed
		t.EventsCollapsed = s.EventsCollapsed
		t.SummaryFetchFailed = s.SummaryFetchFailed
		if len(s.Kinds) > 0 {
			t.StubKinds = make(map[string]int, len(s.Kinds))
			for kind, n := range s.Kinds {
				t.StubKinds[kind.String()] += n
			}
		}
	}
	if n, ok := sa.nbStats.Get(sessionID); ok {
		t.ResultRecalls = n.ResultRecalls
		t.EntryRecalls = n.EntryRecalls
		t.EmptyRecalls = n.EmptyRecalls
		t.CrossRecalls = n.CrossRecalls
		t.CheckpointsWritten = n.CheckpointsWritten
		t.CheckpointRenders = n.CheckpointRenders
		t.DigestsWritten = n.DigestsWritten
		t.DigestRenders = n.DigestRenders
		t.HydrationSeeds = n.HydrationSeeds
		t.HydrationPlanSeeds = n.HydrationPlanSeeds
		t.HydrationRenders = n.HydrationRenders
		t.PriorTurnResultRecalls = n.PriorTurnResultRecalls
		t.GeneratorCalls = n.GeneratorCalls
		t.GeneratorInputTokens = n.GeneratorInputTokens
		t.GeneratorOutputTokens = n.GeneratorOutputTokens
		t.GeneratorCacheReadTokens = n.GeneratorCacheReadTokens
		t.GeneratorCacheWriteTokens = n.GeneratorCacheWriteTokens
	}
	if sa.reqStats != nil {
		if r, ok := sa.reqStats.Get(sessionID); ok {
			t.PromptRequests = r.Requests
			t.PromptTokensLast = r.LastPromptTokens
			t.PromptTokensPeak = r.PeakPromptTokens
			t.ReqSystemBytes = r.SystemBytes
			t.ReqNotebookBytes = r.NotebookBytes
			t.ReqHistoryBytes = r.HistoryBytes
			t.ReqToolCallBytes = r.ToolCallBytes
			t.ReqToolResultBytes = r.ToolResultBytes
			t.Steps = r.Steps
		}
	}
	if sa.edgeStats != nil {
		if m, ok := sa.edgeStats.Get(sessionID); ok && len(m) > 0 {
			t.EdgeFirings = make(map[string]map[string]int, len(m))
			for key, n := range m {
				edge, outcome, _ := strings.Cut(key, ":")
				if t.EdgeFirings[edge] == nil {
					t.EdgeFirings[edge] = map[string]int{}
				}
				t.EdgeFirings[edge][outcome] += n
			}
		}
	}
	return t
}

// RecordGeneratorUsage folds one notebook generation call's usage into
// the session's counters — the sidecar spend (segment entries,
// checkpoints, turn digests) the run's own token totals can't see.
// Deliberately not on the Coordinator interface — the notebook
// generator's usage sink type-asserts for it alongside
// SessionTelemetry.
func (c *coordinator) RecordGeneratorUsage(sessionID string, usage fantasy.Usage) {
	if c.nbStats == nil || sessionID == "" {
		return
	}
	c.nbStats.Update(sessionID, func(n *notebook.Stats) {
		n.GeneratorCalls++
		n.GeneratorInputTokens += usage.InputTokens
		n.GeneratorOutputTokens += usage.OutputTokens
		n.GeneratorCacheReadTokens += usage.CacheReadTokens
		n.GeneratorCacheWriteTokens += usage.CacheCreationTokens
	})
}

// WaitForDetachedWork joins the coordinator's detached-work wait
// group — segment, checkpoint, and digest generation, supersession
// flagging, and title generation — so a short-lived `crush run`
// process can commit coverage before exiting instead of killing the
// goroutines mid-flight. Returns false when the timeout fired with
// work still in flight. Deliberately not on the Coordinator
// interface — the caller type-asserts for it alongside
// SessionTelemetry. timeout <= 0 waits unboundedly.
func (c *coordinator) WaitForDetachedWork(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		c.detachedWork.Wait()
		close(done)
	}()
	if timeout <= 0 {
		<-done
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// EdgeFiringDelta returns the session's edge firing counts minus what
// the previous call reported, then snapshots — the eval harness sums
// per-turn telemetry files, so a process emitting more than once must
// not double-count. Deliberately not on the Coordinator interface —
// the eval harness type-asserts for it alongside SessionTelemetry.
func (c *coordinator) EdgeFiringDelta(sessionID string) map[string]map[string]int {
	if c.edgeStats == nil {
		return nil
	}
	// Read the coordinator-owned map directly — not through
	// currentAgent — so an emission landing during an agent rebuild
	// gap still reports the rows.
	cur, _ := c.edgeStats.Get(sessionID)
	prev, _ := c.edgeFiringEmitted.Get(sessionID)
	var delta map[string]map[string]int
	for key, n := range cur {
		if d := n - prev[key]; d > 0 {
			edge, outcome, _ := strings.Cut(key, ":")
			if delta == nil {
				delta = map[string]map[string]int{}
			}
			if delta[edge] == nil {
				delta[edge] = map[string]int{}
			}
			delta[edge][outcome] = d
		}
	}
	snapshot := make(map[string]int, len(cur))
	for k, n := range cur {
		snapshot[k] = n
	}
	c.edgeFiringEmitted.Set(sessionID, snapshot)
	return delta
}

// EvalMaxStepsEnvVar caps a single run's steps when the eval harness
// is driving — the run-side enforcement of the trajectory-wide
// max_steps budget. The driver passes the remaining budget (plus one)
// per turn; kept in sync with internal/eval.EvalMaxStepsEnvVar.
const EvalMaxStepsEnvVar = "CRUSH_EVAL_MAX_STEPS"

// evalStepCaps returns the eval step cap as a StopCondition, or nil
// when the harness isn't driving this process.
func evalStepCaps() []fantasy.StopCondition {
	n, err := strconv.Atoi(os.Getenv(EvalMaxStepsEnvVar))
	if err != nil || n <= 0 {
		return nil
	}
	return []fantasy.StopCondition{fantasy.StepCountIs(n)}
}
