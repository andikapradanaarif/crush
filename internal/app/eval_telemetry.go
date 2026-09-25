package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
)

// EvalTelemetryEnvVar names the file a non-interactive run writes its
// per-run telemetry to when set — the eval harness's extraction path
// for numbers that only exist in-process (steps, usage, stub stats,
// recalls). Kept in sync with internal/eval.EvalTelemetryEnvVar; the
// constant is duplicated so app doesn't import eval.
const EvalTelemetryEnvVar = "CRUSH_EVAL_TELEMETRY"

// EvalFlagsEnvVar lists the manifest flag names the run reports
// resolved values for — the harness's no-op detection. Kept in sync
// with internal/eval.EvalFlagsEnvVar.
const EvalFlagsEnvVar = "CRUSH_EVAL_FLAGS"

// emitEvalTelemetry writes the run's telemetry JSON when
// CRUSH_EVAL_TELEMETRY is set. Best-effort: a write failure must never
// fail the run itself.
func (app *App) emitEvalTelemetry(sessionID string, result *fantasy.AgentResult, runErr error, approxSteps int) {
	path := os.Getenv(EvalTelemetryEnvVar)
	if path == "" || app.AgentCoordinator == nil {
		return
	}
	doc := map[string]any{
		"session_id": sessionID,
	}
	// The child reports what each manifest flag actually resolved to —
	// arm intent can silently no-op on a renamed or shadowed option.
	if keys := os.Getenv(EvalFlagsEnvVar); keys != "" {
		doc["resolved_options"] = config.OptionsProjection(
			*app.config.Config().Options, strings.Split(keys, ","))
	}
	if result != nil {
		doc["steps"] = len(result.Steps)
		doc["tokens"] = map[string]int64{
			"input":       result.TotalUsage.InputTokens,
			"output":      result.TotalUsage.OutputTokens,
			"cache_read":  result.TotalUsage.CacheReadTokens,
			"cache_write": result.TotalUsage.CacheCreationTokens,
		}
	} else if approxSteps > 0 {
		// Killed mid-run — approximate from distinct assistant
		// messages observed so the record isn't 0/0 for a run that
		// burned real budget.
		doc["steps"] = approxSteps
	}
	// SessionTelemetry is not on the Coordinator interface — assert so
	// test stubs and alternate coordinators needn't implement it.
	if c, ok := app.AgentCoordinator.(interface {
		SessionTelemetry(string) agent.SessionTelemetry
	}); ok {
		tel := c.SessionTelemetry(sessionID)
		kinds := tel.StubKinds
		if kinds == nil {
			// Emit an object, not null, so the doc's shape is stable
			// for runs that never promoted a stub.
			kinds = map[string]int{}
		}
		doc["stub_stats"] = map[string]any{
			"invalidations":     tel.StubInvalidations,
			"results":           tel.StubResults,
			"saved_bytes":       tel.StubSavedBytes,
			"boundary_advances": tel.BoundaryAdvances,
			"kinds":             kinds,
		}
		doc["prior_turns"] = map[string]any{
			"turns_collapsed":  tel.TurnsCollapsed,
			"events_collapsed": tel.EventsCollapsed,
		}
		doc["recalls"] = map[string]any{
			"result":            tel.ResultRecalls,
			"entry":             tel.EntryRecalls,
			"empty":             tel.EmptyRecalls,
			"cross":             tel.CrossRecalls,
			"prior_turn_result": tel.PriorTurnResultRecalls,
		}
		doc["checkpoints"] = map[string]any{
			"written":  tel.CheckpointsWritten,
			"rendered": tel.CheckpointRenders,
		}
		doc["digests"] = map[string]any{
			"written":  tel.DigestsWritten,
			"rendered": tel.DigestRenders,
		}
		doc["hydration"] = map[string]any{
			"seeds":      tel.HydrationSeeds,
			"plan_seeds": tel.HydrationPlanSeeds,
			"rendered":   tel.HydrationRenders,
		}
		// Sidecar generation spend — the notebook-vs-off comparison
		// can't price the notebook without it.
		doc["generator_tokens"] = map[string]any{
			"calls":       tel.GeneratorCalls,
			"input":       tel.GeneratorInputTokens,
			"output":      tel.GeneratorOutputTokens,
			"cache_read":  tel.GeneratorCacheReadTokens,
			"cache_write": tel.GeneratorCacheWriteTokens,
		}
		// Request telemetry: the prompt growth curve + last rendered
		// request's composition — the flat-vs-growing signal the
		// benefit measurement reads. Informational, never gating.
		doc["request"] = map[string]any{
			"prompt_requests":    tel.PromptRequests,
			"prompt_tokens_last": tel.PromptTokensLast,
			"prompt_tokens_peak": tel.PromptTokensPeak,
			"system_bytes":       tel.ReqSystemBytes,
			"notebook_bytes":     tel.ReqNotebookBytes,
			"history_bytes":      tel.ReqHistoryBytes,
			"tool_call_bytes":    tel.ReqToolCallBytes,
			"tool_result_bytes":  tel.ReqToolResultBytes,
			// Per-step rows: usage plus prefix attribution — the
			// named cause behind every cache miss.
			"steps": tel.Steps,
		}
		// Pressure-gate state: the "did the gate fire" predicate the
		// comfortable-regime experiment asserts as silent.
		doc["pressure"] = map[string]any{
			"activations": tel.PressureActivations,
			"engaged":     tel.PressureEngaged,
			"estimate":    tel.PressureEstimate,
		}
	}
	// Edge firings emit as a DELTA, not the cumulative snapshot — the
	// driver sums per-turn telemetry files, so a process emitting
	// twice for one session must not double-count.
	if c, ok := app.AgentCoordinator.(interface {
		EdgeFiringDelta(string) map[string]map[string]int
	}); ok {
		edgeFirings := c.EdgeFiringDelta(sessionID)
		if edgeFirings == nil {
			edgeFirings = map[string]map[string]int{}
		}
		doc["edge_firings"] = edgeFirings
	}
	if m, ok := app.config.Config().Models[config.SelectedModelTypeLarge]; ok {
		doc["model"] = m.Provider + "/" + m.Model
	}
	// The pin covers only the large slot — small/summary resolve from
	// ambient config. Record what they resolved to so a compaction-
	// flag experiment can audit which summarizer actually ran.
	for _, slot := range []config.SelectedModelType{
		config.SelectedModelTypeSmall,
		config.SelectedModelTypeSummary,
	} {
		if m, ok := app.config.Config().Models[slot]; ok {
			doc["model_"+string(slot)] = m.Provider + "/" + m.Model
		}
	}
	if runErr != nil {
		doc["error"] = runErr.Error()
		if class := classifyRunError(runErr); class != "" {
			doc["error_class"] = class
		}
	}
	if data, err := json.Marshal(doc); err == nil {
		_ = os.WriteFile(path, data, 0o644)
	}
}

// classifyRunError maps the run's terminal error to a stable class for
// the eval harness's circuit breaker. Provider errors arrive typed —
// fantasy.ProviderError survives the RetryError wrap via Unwrap — so
// classify on StatusCode and the typed flags rather than message text:
// the harness then distinguishes deterministic failures (every retry
// fails identically) from transients worth resampling. An empty class
// means "unclassified" — the harness falls back to string signatures.
func classifyRunError(err error) string {
	var pe *fantasy.ProviderError
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(err, agent.ErrRequestCancelled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, agent.ErrContextWindowExceeded):
		// Harness-enforced window cap (options.enforce_context_window)
		// — manufactured by the harness, not observed from the
		// provider, so it gets its own class. A real
		// context_too_large is fixture-class: the trajectory can
		// never fit, so two deaths skip it. A manufactured death is
		// the experiment's designed condition — the pressure-regime
		// control arm is supposed to die — so it must record as an
		// ordinary excluded-class error that keeps sampling and
		// counts in the excluded-differential, not skip the
		// trajectory and void the invocation.
		return "window_cap_enforced"
	case errors.As(err, &pe):
		switch {
		case pe.AuthError || pe.StatusCode == http.StatusUnauthorized || pe.StatusCode == http.StatusForbidden:
			return "auth"
		case pe.IsContextTooLarge():
			// Workload overflow is trajectory-scoped, not config —
			// other trajectories still run.
			return "context_too_large"
		case pe.StatusCode == http.StatusTooManyRequests:
			// Rate limits are transient by definition — resampling
			// is the mechanism, not a breaker trip.
			return "rate_limit"
		case pe.StatusCode >= 400 && pe.StatusCode < 500:
			// Model resolution, malformed requests, schema
			// rejections — deterministic per config. Fantasy marks
			// 408/409 retryable; a persistent one is exhaustion, not
			// a broken config, so keep it out of the breaker path.
			if pe.IsRetryable() {
				return "provider_transient"
			}
			return "provider_deterministic"
		case pe.StatusCode >= 500:
			// Fantasy already retried before surfacing; the harness's
			// strike count is the persistence test.
			return "provider_server"
		default:
			// No HTTP status — the failure happened at transport
			// level. Refused, DNS, and routing-layer unreachable all
			// mean "the endpoint can't be reached at all" —
			// config-class. ETIMEDOUT and x509 errors deliberately
			// stay out: a mid-run timeout can be a blip rather than
			// a dead endpoint, so they resample instead of risking
			// a false abort — the cost is burning the attempts cap
			// on a silently-dropped endpoint.
			var dnsErr *net.DNSError
			switch {
			case errors.Is(pe.Cause, syscall.ECONNREFUSED) ||
				errors.Is(pe.Cause, syscall.EHOSTUNREACH) ||
				errors.Is(pe.Cause, syscall.ENETUNREACH) ||
				errors.As(pe.Cause, &dnsErr):
				return "provider_unreachable"
			case pe.IsRetryable():
				return "provider_transient"
			default:
				return "provider_other"
			}
		}
	default:
		// Transport failures can surface without a ProviderError
		// wrap — RetryError.Unwrap yields the bare last-attempt
		// error, so a *url.Error from http.Client.Do (TLS handshake
		// timeout, reset mid-handshake) reaches here unclassified.
		// Classify on chain shape the same way the ProviderError
		// branch does on Cause above, including its retryability
		// split: a non-retryable transport error (e.g. x509
		// verification) lands on provider_other rather than
		// transient — the tolerated set must not widen on the bare
		// path just because *url.Error is always a net.Error.
		var (
			dnsErr *net.DNSError
			netErr net.Error
		)
		switch {
		case errors.Is(err, syscall.ECONNREFUSED) ||
			errors.Is(err, syscall.EHOSTUNREACH) ||
			errors.Is(err, syscall.ENETUNREACH) ||
			errors.As(err, &dnsErr):
			return "provider_unreachable"
		case fantasy.IsTransportError(err) ||
			errors.Is(err, io.ErrUnexpectedEOF) ||
			(errors.As(err, &netErr) && netErr.Timeout()):
			return "provider_transient"
		case errors.As(err, &netErr):
			return "provider_other"
		}
		return ""
	}
}
