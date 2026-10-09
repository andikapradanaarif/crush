package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
)

// Eval model endpoint conventions. Manifests pin a literal
// provider/model because model identity is part of the measured
// condition — LLM_MODEL overrides that pin at load time for
// iteration-cost runs where the model is operator infrastructure,
// not the variable under test. The resolved pin still lands in
// rec.Env.ModelPin, so run records key on what actually ran.
const (
	// LLMModelEnvVar overrides the experiment's model pin. A bare
	// model id keeps the manifest's provider prefix; a
	// provider/model value replaces the whole pin.
	LLMModelEnvVar = "LLM_MODEL"
	// LLMContextWindowEnvVar supplies context_window when the
	// overridden model is neither declared in the provider block
	// nor known to the provider catalog. Also overrides catalog
	// metadata when set.
	LLMContextWindowEnvVar = "LLM_CONTEXT_WINDOW"
	// LLMMaxTokensEnvVar supplies default_max_tokens the same way.
	LLMMaxTokensEnvVar = "LLM_DEFAULT_MAX_TOKENS"
)

// defaultSynthMaxTokens is the default_max_tokens written for a
// catalog-unknown model when LLM_DEFAULT_MAX_TOKENS is unset —
// deliberately conservative; truncation failures are legible.
const defaultSynthMaxTokens int64 = 8192

// catalogModelLookup searches the provider catalog for a model id.
// Declared as a var so tests can stub the catalog fetch — the real
// path consults the same cached catalog preflight resolves against.
var catalogModelLookup = func(modelID string) (*catwalk.Model, bool) {
	known, err := config.Providers(&config.Config{
		Options: &config.Options{DisableProviderAutoUpdate: true},
	})
	if err != nil {
		return nil, false
	}
	for _, p := range known {
		for i := range p.Models {
			if p.Models[i].ID == modelID {
				return &p.Models[i], true
			}
		}
	}
	return nil, false
}

// applyEnvModelOverride rewrites the experiment's model pin from
// LLM_MODEL and ensures the declared provider block carries the
// resolved model. Runs at LoadExperiment so validation, preflight,
// materialization, and run records all see the effective pin.
func applyEnvModelOverride(e *Experiment) error {
	raw := strings.TrimSpace(os.Getenv(LLMModelEnvVar))
	if raw == "" {
		return nil
	}
	if strings.Contains(raw, "/") {
		providerID, modelID, ok := strings.Cut(raw, "/")
		if !ok || providerID == "" || modelID == "" || strings.Contains(modelID, "/") {
			return fmt.Errorf("%s %q: expected provider/model or a bare model id", LLMModelEnvVar, raw)
		}
		e.Model = providerID + "/" + modelID
	} else {
		providerID, _, ok := strings.Cut(e.Model, "/")
		if !ok || providerID == "" {
			return fmt.Errorf("%s %q is a bare model id but the experiment model %q names no provider — pin provider/model in the manifest or set %s to provider/model",
				LLMModelEnvVar, raw, e.Model, LLMModelEnvVar)
		}
		e.Model = providerID + "/" + raw
	}
	providerID, modelID, _ := strings.Cut(e.Model, "/")
	return ensureDeclaredModel(e, providerID, modelID)
}

// ensureDeclaredModel appends a synthesized entry to the provider
// block's models array when the pinned model isn't declared —
// resolution in the child requires a models[] row, not just a name.
// A provider absent from the block is a builtin/catalog provider and
// needs no synthesis; preflight validates the model exists there.
func ensureDeclaredModel(e *Experiment, providerID, modelID string) error {
	rawProv, ok := e.Providers[providerID]
	if !ok {
		return nil
	}
	prov, ok := rawProv.(map[string]any)
	if !ok {
		return fmt.Errorf("providers.%s does not decode to an object", providerID)
	}
	models, _ := prov["models"].([]any)
	for _, m := range models {
		if mm, ok := m.(map[string]any); ok && mm["id"] == modelID {
			return nil
		}
	}
	entry, err := synthModelEntry(modelID)
	if err != nil {
		return err
	}
	prov["models"] = append(models, entry)
	return nil
}

// synthModelEntry builds the models[] row for an env-selected model.
// Metadata comes from the provider catalog when the id is known
// (opencode-go/zen and gateway catalogs share ids); env vars always
// win. A catalog miss requires LLM_CONTEXT_WINDOW — guessing a
// context budget would silently recondition the cell.
func synthModelEntry(modelID string) (map[string]any, error) {
	var entry map[string]any
	if cat, ok := catalogModelLookup(modelID); ok {
		data, err := json.Marshal(cat)
		if err != nil {
			return nil, fmt.Errorf("marshal catalog model %q: %w", modelID, err)
		}
		if err := json.Unmarshal(data, &entry); err != nil {
			return nil, fmt.Errorf("decode catalog model %q: %w", modelID, err)
		}
	} else {
		ctx, err := envInt64(LLMContextWindowEnvVar)
		if err != nil {
			return nil, err
		}
		if ctx == 0 {
			return nil, fmt.Errorf("%s %q is not declared in the provider's models and unknown to the provider catalog — set %s (and optionally %s) or add the model to the manifest",
				LLMModelEnvVar, modelID, LLMContextWindowEnvVar, LLMMaxTokensEnvVar)
		}
		entry = map[string]any{
			"id":                 modelID,
			"name":               modelID,
			"context_window":     ctx,
			"default_max_tokens": defaultSynthMaxTokens,
		}
	}
	if v, err := envInt64(LLMContextWindowEnvVar); err != nil {
		return nil, err
	} else if v != 0 {
		entry["context_window"] = v
	}
	if v, err := envInt64(LLMMaxTokensEnvVar); err != nil {
		return nil, err
	} else if v != 0 {
		entry["default_max_tokens"] = v
	}
	return entry, nil
}

// envInt64 reads a positive-int64 env var; 0 means unset.
func envInt64(name string) (int64, error) {
	s := strings.TrimSpace(os.Getenv(name))
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%s %q: must be a positive integer", name, s)
	}
	return v, nil
}
