package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/env"
)

// configErrorSignatures identify subprocess failures that can only mean
// the generated config is broken — they abort the experiment rather than
// sample. Scoped tight on purpose: "session not found" and friends are
// real bugs, not config errors.
var configErrorSignatures = []string{
	"no providers configured",
	"not found in provider config",
	"does not parse",
	"invalid config",
}

// authErrorSignatures cover the deferred config-class failure: a custom
// (openai-compat) provider with an unresolved api_key is *kept* at load
// (warn-only — keyless local providers are legitimate), then fails every
// request with a 401. That arrives as "agent run failed:" with telemetry
// present, so the pre-model triad never sees it. Scoped tight — bare
// "api key"/"401" substrings would match rate-limit text.
var authErrorSignatures = []string{
	"unauthorized",
	"no api-key",
	"no api key",
	"invalid api key",
	"authentication failed",
	"missing credentials",
}

// nonConfigErrorMarkers negate an otherwise-matching signature — a
// rate-limit message containing "api key" is transient, not config.
var nonConfigErrorMarkers = []string{
	"rate limit",
	"429",
	"quota",
	"too many requests",
}

// preflightExperiment dry-resolves the experiment's providers the same
// way a turn subprocess will — the real provider-prep path over the
// child's effective environment — so credential and config-class errors
// abort before a single subprocess burns an attempt. Runs once per arm
// because arm option fragments (e.g. disable_default_providers) can
// change which providers survive. Custom AgentRunner implementations
// own their environment and skip the check.
func (r *Runner) preflightExperiment(ctx context.Context, exp *Experiment) error {
	drv := r.driver()
	cr, ok := drv.(CrushRunner)
	if !ok {
		return nil
	}
	envMap := make(map[string]string)
	for _, kv := range cr.subprocessEnv("", 0) {
		k, v, _ := strings.Cut(kv, "=")
		envMap[k] = v
	}
	e := env.NewFromMap(envMap)
	resolver := config.NewShellVariableResolver(e)

	providerID, modelID, pinnedProvider := strings.Cut(exp.Model, "/")

	// The catwalk catalog is process-global (providerOnce) — fetch once
	// outside the arm loop so map order can't seed it with one arm's
	// options. Auto-update is pinned off to match the child's generated
	// .crush.json (materialize.go always sets it; arms can't override).
	// Per-arm DisableDefaultProviders still applies inside prep.
	catalogCfg := &config.Config{Options: &config.Options{DisableProviderAutoUpdate: true}}
	known, catErr := config.Providers(catalogCfg)

	// Decode the experiment's provider block once — it's arm-independent.
	var declaredProviders map[string]config.ProviderConfig
	if len(exp.Providers) > 0 {
		data, err := json.Marshal(exp.Providers)
		if err != nil {
			return fmt.Errorf("preflight: marshal providers: %w", err)
		}
		if err := json.Unmarshal(data, &declaredProviders); err != nil {
			return fmt.Errorf("preflight: providers block does not decode: %w", err)
		}
	}

	var problems []string
	for armName, arm := range exp.Arms {
		cfg := &config.Config{
			Options:   &config.Options{DisableProviderAutoUpdate: true},
			Providers: csync.NewMap[string, config.ProviderConfig](),
		}
		if raw := arm.Config.Options; len(raw) > 0 {
			data, err := json.Marshal(raw)
			if err != nil {
				problems = append(problems, fmt.Sprintf("arm %s: options marshal: %v", armName, err))
				continue
			}
			if err := json.Unmarshal(data, cfg.Options); err != nil {
				problems = append(problems, fmt.Sprintf("arm %s: options decode: %v", armName, err))
				continue
			}
		}
		for id, pc := range declaredProviders {
			cfg.Providers.Set(id, pc)
		}

		if catErr != nil && len(known) == 0 {
			problems = append(problems, fmt.Sprintf("arm %s: provider catalog: %v", armName, catErr))
			continue
		}
		if err := cfg.PreflightProviders(ctx, e, known); err != nil {
			problems = append(problems, fmt.Sprintf("arm %s: %v", armName, err))
			continue
		}

		// The pinned provider gets the informative branch first — a
		// builtin-provider experiment whose key is unset wants "anthropic
		// dropped", not the generic unconfigured line.
		if pinnedProvider {
			_, exists := cfg.Providers.Get(providerID)
			if !exists {
				problems = append(problems, fmt.Sprintf("arm %s: provider %q dropped during resolution%s",
					armName, providerID, dropHint(exp, providerID, known, resolver)))
				continue
			}
			// Post-prep GetModel reflects discovery results — "not
			// found" is definitive regardless of discover_models.
			// Stricter than the child by design: resolveSelectedModels
			// silently falls back to the default model on a bad pin,
			// landing records under a condition nobody measured.
			if cfg.GetModel(providerID, modelID) == nil {
				problems = append(problems, fmt.Sprintf("arm %s: model %q not found in provider %q",
					armName, modelID, providerID))
			}
		}
		if !cfg.IsConfigured() {
			problems = append(problems, fmt.Sprintf("arm %s: no providers configured after resolution", armName))
			continue
		}
		// Custom providers survive a missing api_key (warn-only — keyless
		// locals are legitimate) but every request 401s. A *declared*
		// credential template that resolves empty is a config error;
		// absent api_key on a loopback endpoint is by design.
		for id, raw := range exp.Providers {
			var declared config.ProviderConfig
			if data, err := json.Marshal(raw); err == nil {
				_ = json.Unmarshal(data, &declared)
			}
			if declared.APIKey == "" {
				continue
			}
			resolved, err := resolver.ResolveValue(declared.APIKey)
			if (err != nil || resolved == "") && !loopbackBaseURL(declared.BaseURL, resolver) {
				problems = append(problems, fmt.Sprintf("arm %s: provider %q api_key %q resolves empty (unset env var?)",
					armName, id, declared.APIKey))
			}
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("preflight: %s", strings.Join(problems, "; "))
	}
	return nil
}

// loopbackBaseURL reports whether the provider's base_url resolves to a
// loopback host — keyless local endpoints (ollama, llama.cpp) are the
// legitimate empty-credential case the credential check must not flag.
func loopbackBaseURL(baseURL string, resolver config.VariableResolver) bool {
	u, err := resolver.ResolveValue(baseURL)
	if err != nil || u == "" {
		return false
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// dropHint re-resolves the provider's credential template for the abort
// message — the common failure is an env ref that resolves empty. The
// template comes from the experiment block when declared, else the
// catwalk catalog entry's api_key env ref.
func dropHint(exp *Experiment, providerID string, known []catwalk.Provider, resolver config.VariableResolver) string {
	keyTemplate := ""
	if raw, ok := exp.Providers[providerID]; ok {
		var declared config.ProviderConfig
		if data, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(data, &declared)
		}
		keyTemplate = declared.APIKey
	} else {
		for _, kp := range known {
			if string(kp.ID) == providerID {
				keyTemplate = kp.APIKey
				break
			}
		}
	}
	switch {
	case keyTemplate == "":
		return " (no credential template found)"
	default:
		if v, err := resolver.ResolveValue(keyTemplate); err != nil || v == "" {
			return fmt.Sprintf(" (api_key %q resolves empty — unset env var?)", keyTemplate)
		}
		return ""
	}
}

// isConfigClassError reports whether a run record is a provably
// config-class failure. When the child reports error_class — typed
// classification of the fantasy.ProviderError, immune to message-text
// drift — it wins outright:
//
//   - auth / provider_unreachable: credentials and the endpoint are
//     experiment-global — every trajectory fails identically — trip.
//   - provider_deterministic / provider_server: scope-split. Before
//     any request completes (Steps==0 && Request==nil) the failure is
//     config-shaped — trip; mid-run it can be payload-specific —
//     fixture-class. (A dead endpoint can never produce a completed
//     request, so it still reads step-0.)
//   - rate_limit / provider_transient / context_too_large /
//     window_cap_enforced / provider_other / cancelled / timeout: keep
//     sampling — resampling or the attempts cap is the right mechanism.
//     (context_too_large is additionally fixture-class: deterministic
//     per trajectory; window_cap_enforced is deliberately not — see
//     isFixtureConfigError.)
//
// Without error_class (older children), two string-matched classes:
//
//   - Pre-model exit: "crush run failed:" with no request stats and no
//     steps, matching a config signature — the subprocess died before
//     the model. (SessionDB is NOT such a signal: the child creates
//     crush.db in setupLocalWorkspace before IsConfigured fails, and
//     preserveSessionDB snapshots it unconditionally — every real
//     "No providers configured" record carries session_db.)
//   - Deferred credential failure: "agent run failed:" matching an auth
//     signature — the provider survived load but every request 401s.
//
// Both are experiment-global (credentials/config don't vary per run);
// anything else — rate limits, mid-run model errors — keeps sampling.
// Signatures match case-insensitively: real run_error strings carry
// TUI-rendered capitalization ("  No providers configured  ").
func isConfigClassError(rec RunRecord) bool {
	if rec.Outcome != OutcomeError {
		return false
	}
	switch rec.ErrorClass {
	case "auth", "provider_unreachable":
		// Credentials and the endpoint itself are experiment-global —
		// a failure at any step still fails every other trajectory.
		return true
	case "provider_deterministic", "provider_server":
		// A provider failure before the first request completes is
		// config-shaped — unresolved model, rejected schema, dead
		// endpoint; every trajectory fails identically at step 0.
		// Mid-run either class can be trajectory-shaped: a
		// pathological tool result the provider rejects (4xx), or a
		// payload that trips a provider bug (5xx — endpoints do
		// return 500/529 on specific inputs). A genuinely dead
		// endpoint still aborts — it can never produce a completed
		// request, so it never reads mid-run.
		return rec.Steps == 0 && rec.Request == nil
	case "rate_limit", "provider_transient", "provider_other", "context_too_large", "window_cap_enforced", "cancelled", "timeout":
		return false
	}
	s, _ := rec.CheckDetail["run_error"].(string)
	lower := strings.ToLower(s)
	if slices.ContainsFunc(nonConfigErrorMarkers, func(m string) bool {
		return strings.Contains(lower, m)
	}) {
		return false
	}
	switch {
	case strings.HasPrefix(s, "crush run failed:"):
		if rec.Request != nil || rec.Steps > 0 {
			return false
		}
		if strings.Contains(lower, "not found") &&
			(strings.Contains(lower, "model") || strings.Contains(lower, "provider")) {
			return true
		}
		return slices.ContainsFunc(configErrorSignatures, func(sig string) bool {
			return strings.Contains(lower, sig)
		})
	case strings.HasPrefix(s, "agent run failed:"):
		return slices.ContainsFunc(authErrorSignatures, func(sig string) bool {
			return strings.Contains(lower, sig)
		})
	}
	return false
}

// isFixtureConfigError reports a trajectory-scoped deterministic
// failure — WriteArmConfig rejected the fixture's config (unparseable
// .crush.json, manifest-flag pinning, forbidden keys) or Materialize
// failed outright (tagged "harness"), or the check script can't run
// (tagged "check_error"). A context_too_large error class joins them:
// the trajectory overflows the model's window every attempt —
// unrunnable, but experiment-global config is fine. The trajectory is
// unrunnable; other trajectories are unaffected. Bare records (no
// CheckDetail) are deliberately NOT fixture-class: the only producers
// of those are ExecuteRun-internal errors — spawn failures, disk,
// timeouts — which are transient, not provably deterministic; they
// burn an attempt and keep sampling.
func isFixtureConfigError(rec RunRecord) bool {
	if rec.Outcome != OutcomeError {
		return false
	}
	if rec.ErrorClass == "context_too_large" {
		return true
	}
	// window_cap_enforced is deliberately not fixture-class even
	// though it is equally deterministic: the manufactured rejection
	// is the experiment's designed condition (the pressure-regime
	// control arm is supposed to die at the declared cap), so the
	// death is data — it keeps sampling, lands as an excluded-class
	// record, and the excluded-differential reports the asymmetry.
	// Fixture-classing it would skip the trajectory at two deaths
	// and void the invocation exactly when the cap works.
	if rec.ErrorClass == "window_cap_enforced" {
		return false
	}
	// A provider rejection or server failure observed mid-trajectory
	// is trajectory-scoped — the offending content belongs to this
	// trajectory's render (a rejected tool result, a payload that
	// trips a provider bug), not the shared config. Step-0 failures
	// stay config-class (see isConfigClassError).
	if rec.ErrorClass == "provider_deterministic" || rec.ErrorClass == "provider_server" {
		return rec.Steps > 0 || rec.Request != nil
	}
	if _, ok := rec.CheckDetail["harness"]; ok {
		return true
	}
	_, ok := rec.CheckDetail["check_error"]
	return ok
}
