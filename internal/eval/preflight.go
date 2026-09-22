package eval

import (
	"context"
	"encoding/json"
	"fmt"
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
// present, so the pre-model triad never sees it.
var authErrorSignatures = []string{
	"unauthorized",
	"no api-key",
	"api key",
	"authentication",
	"401",
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

	var problems []string
	for armName, arm := range exp.Arms {
		cfg := &config.Config{
			Options:   &config.Options{},
			Providers: csync.NewMap[string, config.ProviderConfig](),
		}
		if raw := arm.Config.Options; len(raw) > 0 {
			if data, err := json.Marshal(raw); err == nil {
				_ = json.Unmarshal(data, cfg.Options)
			}
		}
		if len(exp.Providers) > 0 {
			data, err := json.Marshal(exp.Providers)
			if err != nil {
				return fmt.Errorf("preflight: marshal providers: %w", err)
			}
			var declared map[string]config.ProviderConfig
			if err := json.Unmarshal(data, &declared); err != nil {
				return fmt.Errorf("preflight: providers block does not decode: %w", err)
			}
			for id, pc := range declared {
				cfg.Providers.Set(id, pc)
			}
		}

		// The catwalk catalog the child merges against — same call, so a
		// catalog fetch failure fails here exactly as it would there.
		known, err := config.Providers(cfg)
		if err != nil && len(known) == 0 {
			problems = append(problems, fmt.Sprintf("arm %s: provider catalog: %v", armName, err))
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
			pc, exists := cfg.Providers.Get(providerID)
			if !exists {
				problems = append(problems, fmt.Sprintf("arm %s: provider %q dropped during resolution%s",
					armName, providerID, dropHint(exp, providerID, known, resolver)))
				continue
			}
			if cfg.GetModel(providerID, modelID) == nil &&
				pc.AutoDiscoverModels != nil && !*pc.AutoDiscoverModels {
				// Discovery fetches the model list at runtime — "not
				// declared" is only a failure when it's off.
				problems = append(problems, fmt.Sprintf("arm %s: model %q not in provider %q's declared models (discover_models off)",
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
	host := u
	if i := strings.Index(u, "://"); i >= 0 {
		host = u[i+3:]
	}
	if i := strings.IndexAny(host, "/:"); i >= 0 {
		host = host[:i]
	}
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]"
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
// config-class failure. Two classes:
//
//   - Pre-model exit: "crush run failed:" with no session and no steps,
//     matching a config signature — the subprocess died before the model.
//   - Deferred credential failure: "agent run failed:" matching an auth
//     signature — the provider survived load but every request 401s.
//
// Both are experiment-global (credentials/config don't vary per run);
// anything else — rate limits, mid-run model errors — keeps sampling.
func isConfigClassError(rec RunRecord) bool {
	if rec.Outcome != OutcomeError {
		return false
	}
	s, _ := rec.CheckDetail["run_error"].(string)
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(s, "crush run failed:"):
		if rec.SessionDB != "" || rec.Steps > 0 {
			return false
		}
		if strings.Contains(s, "not found") && (strings.Contains(s, "model") || strings.Contains(s, "provider")) {
			return true
		}
		return slices.ContainsFunc(configErrorSignatures, func(sig string) bool {
			return strings.Contains(s, sig)
		})
	case strings.HasPrefix(s, "agent run failed:"):
		return slices.ContainsFunc(authErrorSignatures, func(sig string) bool {
			return strings.Contains(lower, sig)
		})
	}
	return false
}

// isFixtureConfigError reports a trajectory-scoped config failure —
// WriteArmConfig rejected the fixture's config (unparseable
// .crush.json, manifest-flag pinning). The trajectory is unrunnable;
// other trajectories are unaffected.
func isFixtureConfigError(rec RunRecord) bool {
	if rec.Outcome != OutcomeError {
		return false
	}
	_, ok := rec.CheckDetail["harness"]
	return ok
}
