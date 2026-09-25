// Package eval implements the golden-trajectory harness described in
// docs/design/EVAL_HARNESS.md: a fixed, rerun-able corpus of tasks — each a
// start state, a prompt sequence, and a deterministic end-state check —
// plus the paired-experiment machinery that gates changes on corpus
// outcomes instead of live traffic.
//
// The package owns the corpus format (trajectory.json, experiment.json,
// run records, bands.json), the quarantine procedure, and the runner.
// Agent runs are driven through the crush binary itself so both arms of
// an experiment exercise the same build under test.
package eval

import "time"

// Outcome is the per-run verdict.
//
// Precedence when several apply: error > timeout > fail >
// inconclusive > pass. Derailment (error, timeout) preempts the check;
// a check-fail is a real outcome whether or not coverage fired;
// coverage unmet converts pass to inconclusive only.
type Outcome string

const (
	OutcomePass         Outcome = "pass"
	OutcomeFail         Outcome = "fail"
	OutcomeError        Outcome = "error"
	OutcomeTimeout      Outcome = "timeout"
	OutcomeInconclusive Outcome = "inconclusive"
)

// Conclusive reports whether the outcome counts toward pass-rate
// estimates: excluded classes (error, inconclusive) are non-samples.
func (o Outcome) Conclusive() bool {
	return o == OutcomePass || o == OutcomeFail || o == OutcomeTimeout
}

// Band is the characterization state of a trajectory.
type Band string

const (
	BandStable          Band = "stable"
	BandMid             Band = "mid"
	BandUncharacterized Band = "uncharacterized"
	BandQuarantined     Band = "quarantined"
)

// QuarantineReason enumerates why a trajectory is quarantined.
type QuarantineReason string

const (
	ReasonFlaky         QuarantineReason = "flaky"
	ReasonVacuous       QuarantineReason = "vacuous"
	ReasonMiscalibrated QuarantineReason = "miscalibrated"
	ReasonSuspectCheck  QuarantineReason = "suspect_check"
	ReasonNeverPassed   QuarantineReason = "never_passed"
)

// Arm names are load-bearing across ExecuteRun callers, gate pairing,
// and the condition-key join — const, not literals.
const (
	ArmControl   = "control"
	ArmTreatment = "treatment"
	// ArmBaseline is the characterize/smoke arm name — runs under the
	// empty arm, i.e. the true default condition.
	ArmBaseline = "baseline"
	// ArmAA is the calibration arm --aa injects: a clone of control
	// whose runs measure the harness's own false-effect magnitude.
	// The gate only ever compares control vs treatment, so aa records
	// are excluded from every verdict tier by construction.
	ArmAA = "aa"
)

// CharacterizeExperiment is the reserved experiment name for genesis
// and re-characterization runs — non-comparison samples that flow
// through the same run-record pipeline.
const CharacterizeExperiment = "_characterize"

// Trajectory is the authored spec: what the task is.
type Trajectory struct {
	ID            string     `json:"id"`
	SchemaVersion int        `json:"schema_version"`
	Origin        Origin     `json:"origin"`
	StartState    StartState `json:"start_state"`
	Task          Task       `json:"task"`
	Check         Check      `json:"check"`
	Coverage      Coverage   `json:"coverage,omitempty"`
	Requires      Requires   `json:"requires,omitempty"`
	Budget        Budget     `json:"budget,omitempty"`
}

// Origin records what a trajectory guards.
type Origin struct {
	Kind     string `json:"kind"` // regression | production | synthetic
	Source   string `json:"source,omitempty"`
	Scrubbed *bool  `json:"scrubbed,omitempty"`
}

// StartState describes materialization.
type StartState struct {
	Kind       string   `json:"kind"` // fixture | git
	FixtureDir string   `json:"fixture_dir,omitempty"`
	Repo       string   `json:"repo,omitempty"`
	Ref        string   `json:"ref,omitempty"`
	Setup      []string `json:"setup,omitempty"`
}

// Task is the fixed prompt sequence.
type Task struct {
	Turns []string `json:"turns"`
}

// Check is the scoring-function contract.
type Check struct {
	Script           string `json:"script"`             // relative to trajectory dir, e.g. "check.sh"
	ExpectStartState string `json:"expect_start_state"` // fail | pass
	TimeoutSeconds   int    `json:"timeout_seconds,omitempty"`
}

// Coverage is the closed predicate grammar over run-record fields:
// keys are <op>_<field> where op is min|max and field is a dotted
// run-record path (steps, tokens.output, stub_stats.boundary_advances,
// stub_stats.kinds.deleted, recalls.entry). A run that misses any
// predicate is inconclusive (passes only; fails stand).
type Coverage map[string]float64

// Requires declares environment preconditions. Declared, not enforced:
// an undeclared dependency surfaces as error, which is the detection
// path.
type Requires struct {
	Network *bool    `json:"network,omitempty"` // non-provider egress allowed
	LSP     []string `json:"lsp,omitempty"`
	Tools   []string `json:"tools,omitempty"`
	OS      []string `json:"os,omitempty"`
}

// Budget bounds a derailed run, trajectory-wide.
type Budget struct {
	RunTimeoutSeconds int `json:"run_timeout_seconds,omitempty"`
	MaxSteps          int `json:"max_steps,omitempty"`
}

// Experiment is a paired comparison: arms × corpus slice.
type Experiment struct {
	Name              string         `json:"name"`
	Model             string         `json:"model"` // provider/model, e.g. "hyper/deepseek-v4-pro-0813"
	Temperature       *float64       `json:"temperature,omitempty"`
	Corpus            []string       `json:"corpus"` // globs or "band:<name>"
	RunsPerTrajectory map[Band]int   `json:"runs_per_trajectory"`
	Arms              map[string]Arm `json:"arms"`
	// Primary is the experiment's pre-registered decision metric —
	// the one continuous measurement the run exists to move. The
	// power gate refuses to schedule a run whose total arm sample
	// can't resolve Primary.MDE at the recorded noise level. Absent
	// primary, the experiment runs the binary gate only.
	Primary *Primary `json:"primary,omitempty"`
	// CostWeights prices one billed unit of each discounted token
	// class in uncached-input units — h for cache-read, o for output —
	// pinned per model so weighted_cost is comparable across runs.
	CostWeights *CostWeights `json:"cost_weights,omitempty"`
	// Providers declares custom providers the experiment's model
	// resolves against — written into the generated .crush.json so
	// non-builtin providers (e.g. an OpenAI-compatible endpoint) work
	// under the child's sanitized HOME. Raw JSON matching the config
	// providers schema; api_key should be an env ref ($VAR) — the
	// eval environment's credentials pass through, secrets never
	// enter the repo.
	Providers map[string]any `json:"providers,omitempty"`
	// ExpectedExclusion declares an arm's designed exclusion
	// pattern — the experiment's hypothesis is that the arm cannot
	// complete the corpus (the pressure-regime control dying at the
	// manufactured cap). A declared exclusion is data, not an alarm:
	// error-saturation and the excluded-differential are consumed
	// for trajectories where the declared arm meets the expectation,
	// while a trajectory where it falls short reports the miss —
	// the regime the experiment needs never engaged, so the run is
	// vacuous where it should have been decisive.
	ExpectedExclusion *ExpectedExclusion `json:"expected_exclusion,omitempty"`
}

// ExpectedExclusion pins which arm is supposed to fail and how:
// at least Min error records carrying ErrorClass per trajectory.
// Min should equal the trajectory's designed n when the claim is
// total death (control cannot run this corpus); a smaller Min
// asserts only that the regime engages. Transient-infrastructure
// classes (rate_limit, provider_transient, provider_unreachable)
// riding alongside are excused from the exclusivity check as
// weather — they never count toward Min and stay reported on the
// report's tolerated line.
type ExpectedExclusion struct {
	Arm        string `json:"arm"`         // control | treatment
	ErrorClass string `json:"error_class"` // e.g. window_cap_enforced
	Min        int    `json:"min"`
}

// Primary declares the decision metric, its expected direction, and
// the minimum detectable effect as a relative change (0.15 = 15%).
// Direction selects a one-sided comparison: "decrease" claims the
// treatment lowers the metric, "increase" that it raises it.
type Primary struct {
	Metric    string  `json:"metric"`
	Direction string  `json:"direction"` // increase | decrease
	MDE       float64 `json:"mde"`
}

// PrimaryDirection enumerates the legal direction spellings.
const (
	PrimaryIncrease = "increase"
	PrimaryDecrease = "decrease"
)

// CostWeights converts discounted token classes into uncached-input
// equivalents: weighted_cost = input + h·cache_read + o·output where
// h = CacheRead, o = Output. Weights are relative prices, not dollars —
// e.g. cache-read billed at 10% of input gives CacheRead 0.1.
type CostWeights struct {
	CacheRead float64 `json:"cache_read"`
	Output    float64 `json:"output"`
}

// Arm is a generated config fragment plus an optional arm-scoped
// coverage block. Config is an options-fragment only: providers, MCPs,
// and LSPs can't vary between arms. Coverage applies only to this
// arm's runs, after the trajectory's shared predicates — it is where
// flag-gated firing assertions live (e.g. a treatment arm that enables
// stubbing can demand min_stub_stats.results so a run where the
// mechanism never fired lands inconclusive instead of passing as
// evidence of nothing), and its grammar reaches the flag-dependent
// call_metrics fields trajectory coverage excludes.
type Arm struct {
	Config   ArmConfig `json:"config"`
	Coverage Coverage  `json:"coverage,omitempty"`
}

// ArmConfig carries the options delta under test.
type ArmConfig struct {
	Options map[string]any `json:"options,omitempty"`
}

// RunRecord is one append-only results/*.jsonl line.
type RunRecord struct {
	Experiment   string `json:"experiment"`
	TrajectoryID string `json:"trajectory_id"`
	Arm          string `json:"arm"`
	// Invocation scopes records to one RunExperiment call — re-running
	// an experiment under a new build must not pool records into the
	// gate's arm samples (the "same build both arms" invariant).
	Invocation  string         `json:"invocation,omitempty"`
	RunIndex    int            `json:"run_index"` // attempt index, sparse under resampling
	Outcome     Outcome        `json:"outcome"`
	CheckDetail map[string]any `json:"check_detail,omitempty"`
	// Bounded tails of check.sh output — the first forensic stop on
	// failure is what the check actually said.
	CheckStdout string      `json:"check_stdout,omitempty"`
	CheckStderr string      `json:"check_stderr,omitempty"`
	StartedAt   time.Time   `json:"started_at"`
	DurationS   float64     `json:"duration_s"`
	Steps       int         `json:"steps"`
	Tokens      TokenUsage  `json:"tokens"`
	StubStats   StubStats   `json:"stub_stats"`
	PriorTurns  PriorTurns  `json:"prior_turns"`
	Recalls     Recalls     `json:"recalls"`
	Checkpoints Checkpoints `json:"checkpoints"`
	// Digests carries the turn-digest telemetry — same written/
	// rendered split as Checkpoints, counting granularity:turn
	// entries.
	Digests Checkpoints `json:"digests"`
	// Hydration carries the session-hydration telemetry — seeds is
	// the firing side (entries committed by SeedEntries), rendered
	// the present-at-render side. The cold-start arm's coverage gate
	// reads min_hydration.seeds.
	Hydration Hydration `json:"hydration"`
	// EdgeFirings is the run's per-edge outcome split — the
	// edge_firings telemetry the flag-flip decisions consume.
	EdgeFirings map[string]map[string]int `json:"edge_firings,omitempty"`
	SessionDB   string                    `json:"session_db,omitempty"`
	// Workdir is the materialized run directory — recorded so a
	// post-hoc `crush eval analyze` on the artifact can anchor relative
	// call paths correctly (the directory itself is deleted).
	Workdir string `json:"workdir,omitempty"`
	// SessionDBIncomplete marks a raw-copy fallback snapshot — the WAL
	// tail may be missing, so call_metrics underreports.
	SessionDBIncomplete bool `json:"session_db_incomplete,omitempty"`
	// CallMetrics is the post-run sequence analysis of SessionDB —
	// populated between preserveSessionDB and record append so
	// min_call_metrics.* predicates can read it during CoverageMet.
	CallMetrics *CallMetrics `json:"call_metrics,omitempty"`
	// CallMetricsError records analyzer failure instead of silently
	// absent metrics — inconclusive-by-absence and analyzer-broke are
	// operationally different and must not conflate.
	CallMetricsError string `json:"call_metrics_error,omitempty"`
	// BaselineKey is the hash of the run's effective config over the
	// flag projection — which baseline condition this run counts
	// toward. Computed at run time so merged experiments' treatment
	// arms self-seed post-flip baselines.
	BaselineKey string `json:"baseline_key,omitempty"`
	// ResolvedOptions is the child's report of what each manifest flag
	// actually resolved to — arm intent can silently no-op on a
	// renamed/shadowed option; resolved state is the truth.
	ResolvedOptions map[string]any `json:"resolved_options,omitempty"`
	// PromptTokensPerTurn is the prompt growth curve: each turn's
	// last request's normalized prompt tokens (input + cache write +
	// cache read). Flat across turns means the context machinery
	// holds the rendered request down — the benefit claim, measured
	// instead of asserted. Informational only; never a predicate.
	PromptTokensPerTurn []int64 `json:"prompt_tokens_per_turn,omitempty"`
	// StepRecords is the per-step table — usage plus prefix
	// attribution for every step across every turn. The cache-miss
	// forensics: which component first differed and what the volatile
	// prefix hashed to. Informational only; never a predicate.
	StepRecords []StepRecord `json:"step_records,omitempty"`
	// GeneratorTokens accounts the sidecar LLM calls that produced
	// notebook entries — generation spend invisible in Tokens. Absent
	// on arms where the notebook never generated.
	GeneratorTokens *GeneratorTokens `json:"generator_tokens,omitempty"`
	// ErrorClass is the child's typed error classification (auth,
	// provider_deterministic, provider_server, rate_limit, ...) —
	// the circuit breaker reads it instead of string-matching when
	// present; absent means an older child, fall back to signatures.
	ErrorClass string `json:"error_class,omitempty"`
	// Request carries the trajectory-final rendered request's byte
	// composition and the run's peak prompt size — the "what fills
	// the prompt" breakdown the 70%-tool-results claim reads.
	Request *RequestStats `json:"request,omitempty"`
	// Pressure carries the pressure gate's trajectory state:
	// activations counts engage transitions (the "did the gate fire"
	// predicate for comfortable-regime experiments), engaged the
	// latch at the last turn's exit, estimate the last next-request
	// estimate for the estimate-vs-reported audit. Pointer-gated like
	// Request: nil when the gate never evaluated (notebook or gate
	// flag off, unknown window), so pressure.* predicates fail closed
	// rather than reading unmeasured silence as zero.
	Pressure *Pressure `json:"pressure,omitempty"`
	Env      Env       `json:"env"`
}

// RequestStats is the run's request-size snapshot: the last rendered
// request's content bytes by component plus the peak normalized
// prompt tokens observed across the trajectory's steps.
type RequestStats struct {
	PromptRequests   int64 `json:"prompt_requests"`
	PromptTokensPeak int64 `json:"prompt_tokens_peak"`
	SystemBytes      int64 `json:"system_bytes"`
	NotebookBytes    int64 `json:"notebook_bytes"`
	HistoryBytes     int64 `json:"history_bytes"`
	ToolCallBytes    int64 `json:"tool_call_bytes"`
	ToolResultBytes  int64 `json:"tool_result_bytes"`
}

// TokenUsage mirrors fantasy.Usage for the record.
type TokenUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// Pressure mirrors the agent's pressure-gate telemetry — the
// notebook-mode overflow guard's activation state.
type Pressure struct {
	Activations int   `json:"activations"`
	Engaged     bool  `json:"engaged"`
	Estimate    int64 `json:"estimate"`
}

// StepRecord is one agent step's usage plus prefix attribution — the
// per-step row the aggregate TokenUsage can't carry. One row per step,
// not per wire request: fantasy's internal retries resend the same
// prompt and fold into a single OnStepFinish. A terminal mid-step
// failure still produces a row (Failed, zero usage) so the request
// that broke the run keeps its attribution. Turn/Step locate it in
// the trajectory; FirstChangedCause names the component that diverged
// from the previous request (cold | append | shrink | system-prompt |
// notebook-prefix | history, or empty when the render is byte-
// identical), and PrefixHash fingerprints the leading system-message
// run the provider's prompt cache keys on. FirstChanged is -1 when
// nothing changed.
type StepRecord struct {
	Turn              int    `json:"turn"`
	Step              int    `json:"step"`
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CacheReadTokens   int64  `json:"cache_read_tokens"`
	CacheWriteTokens  int64  `json:"cache_write_tokens"`
	Estimated         bool   `json:"estimated,omitempty"`
	Failed            bool   `json:"failed,omitempty"`
	PrefixHash        string `json:"prefix_hash,omitempty"`
	FirstChanged      int    `json:"first_changed_index"`
	FirstChangedCause string `json:"first_changed_cause,omitempty"`
	// PressureEstimate/PressureEngaged carry the gate's per-step
	// state — the estimate-vs-reported audit pair.
	PressureEstimate int64 `json:"pressure_estimate,omitempty"`
	PressureEngaged  bool  `json:"pressure_engaged,omitempty"`
}

// GeneratorTokens accounts the notebook sidecar's generation spend —
// segment-entry, checkpoint, and turn-digest LLM calls that never
// touch the run's token totals. The notebook-vs-off baseline can't
// price the notebook without it.
type GeneratorTokens struct {
	Calls      int   `json:"calls"`
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// StubStats mirrors the agent's per-session stubbing telemetry.
type StubStats struct {
	Invalidations    int   `json:"invalidations"`
	Results          int   `json:"results"`
	SavedBytes       int64 `json:"saved_bytes"`
	BoundaryAdvances int   `json:"boundary_advances"`
	// Kinds splits Results by stub kind, keyed by the kind's
	// telemetry label — the superseded kind (the empty string on the
	// mark itself) is "superseded" here. It backs the
	// stub_stats.kinds.<kind> coverage predicates.
	Kinds map[string]int `json:"kinds,omitempty"`
}

// PriorTurns mirrors the agent's prior-turn collapse telemetry:
// distinct turns rendered collapsed and the call/result pairs inside
// them — the flag-flip evidence for notebook_prior_turns.
type PriorTurns struct {
	TurnsCollapsed  int `json:"turns_collapsed"`
	EventsCollapsed int `json:"events_collapsed"`
}

// Recalls mirrors notebook.Stats' recall split.
type Recalls struct {
	Result int `json:"result"`
	Entry  int `json:"entry"`
	Empty  int `json:"empty"`
	Cross  int `json:"cross"`
	// PriorTurnResult counts result: recalls into prior turns — the
	// approximation of recall-into-collapsed.
	PriorTurnResult int `json:"prior_turn_result"`
}

// Checkpoints mirrors the checkpoint telemetry split: written counts
// committed checkpoint entries, rendered counts prefix renders that
// included one — the "checkpoint present at render" predicate field.
// RunRecord reuses it for digests (granularity:turn entries).
type Checkpoints struct {
	Written  int `json:"written"`
	Rendered int `json:"rendered"`
}

// Hydration mirrors the agent's session-hydration telemetry:
// seeds counts committed mem0-sourced seed entries, plan_seeds the
// locally sourced plan seed, rendered counts prefix renders that
// included a hydrated-tagged entry.
type Hydration struct {
	Seeds     int `json:"seeds"`
	PlanSeeds int `json:"plan_seeds"`
	Rendered  int `json:"rendered"`
}

// Env is the forensic record: when a trajectory rots, the diff between
// last-green and first-red env blocks is the first place to look.
type Env struct {
	CrushSHA string `json:"crush_sha"`
	// ModelPin is the experiment's model string as spelled in the
	// pin; ModelResolved is what the run actually resolved to. They
	// can differ (aliases, normalization) — the pin defines the
	// condition, the resolved form is the baseline storage key.
	ModelPin      string `json:"model_pin,omitempty"`
	ModelResolved string `json:"model_resolved"`
	// Small/summary resolve from ambient config — recorded so a
	// compaction-flag experiment can audit which summarizer ran.
	ModelSmall   string `json:"model_small,omitempty"`
	ModelSummary string `json:"model_summary,omitempty"`
	// Temperature is part of the run's condition — a characterize at
	// temp 0 vs an experiment at model-default are different
	// conditions the baseline key must not merge. "default" marks
	// an unpinned temperature.
	Temperature string `json:"temperature,omitempty"`
	Go          string `json:"go"`
	OS          string `json:"os"`
	ContentHash string `json:"content_hash"`
}

// BaselineCounts are the integer cells Fisher's exact needs; p̂ is
// derived.
type BaselineCounts struct {
	Passes  int    `json:"passes"`
	N       int    `json:"n"`
	LastRun string `json:"last_run,omitempty"`
}

// PHat derives the baseline pass rate.
func (b BaselineCounts) PHat() float64 {
	if b.N == 0 {
		return 0
	}
	return float64(b.Passes) / float64(b.N)
}

// BandEntry is per-trajectory characterization state. Baselines are
// keyed, not singleton: baselines[model][baselineConfigHash] → counts,
// because control-equivalent is experiment-relative.
type BandEntry struct {
	Band              Band             `json:"band"`
	QuarantineReason  QuarantineReason `json:"quarantine_reason,omitempty"`
	ContentHash       string           `json:"content_hash"`
	LastCharacterized string           `json:"last_characterized,omitempty"`
	// PromotionStreak implements the asymmetric hysteresis: a
	// non-stable band promotes after PromotionStreakRequired
	// consecutive good characterizations.
	PromotionStreak int                                  `json:"promotion_streak,omitempty"`
	Baselines       map[string]map[string]BaselineCounts `json:"baselines,omitempty"` // model -> config hash -> counts
}

// Bands is bands.json: generated characterization state.
type Bands struct {
	SchemaVersion int                  `json:"_schema_version"`
	Entries       map[string]BandEntry `json:"-"` // flattened at (un)marshal time
}
