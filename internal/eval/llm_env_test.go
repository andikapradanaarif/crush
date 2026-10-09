package eval

import (
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

func llmExperiment() *Experiment {
	return &Experiment{
		Name:  "llm-env",
		Model: "llm/deepseek-v4.1-flash",
		Providers: map[string]any{
			"llm": map[string]any{
				"name":     "Eval LLM",
				"type":     "openai-compat",
				"api_key":  "$LLM_API_KEY",
				"base_url": "$LLM_BASE_URL",
				"models": []any{
					map[string]any{
						"id":                 "deepseek-v4.1-flash",
						"name":               "DeepSeek V4.1 Flash",
						"context_window":     1000000,
						"default_max_tokens": 384000,
					},
				},
			},
		},
		Arms: map[string]Arm{ArmControl: {}, ArmTreatment: {}},
	}
}

// stubCatalog replaces the catalog lookup for the test duration.
func stubCatalog(t *testing.T, models map[string]catwalk.Model) {
	t.Helper()
	old := catalogModelLookup
	t.Cleanup(func() { catalogModelLookup = old })
	catalogModelLookup = func(id string) (*catwalk.Model, bool) {
		if m, ok := models[id]; ok {
			return &m, true
		}
		return nil, false
	}
}

func modelsOf(t *testing.T, e *Experiment, provider string) []any {
	t.Helper()
	prov, ok := e.Providers[provider].(map[string]any)
	require.True(t, ok)
	models, ok := prov["models"].([]any)
	require.True(t, ok)
	return models
}

func TestEnvModelOverride_Unset(t *testing.T) {
	e := llmExperiment()
	require.NoError(t, applyEnvModelOverride(e))
	require.Equal(t, "llm/deepseek-v4.1-flash", e.Model)
	require.Len(t, modelsOf(t, e, "llm"), 1)
}

func TestEnvModelOverride_BareModelSwapsID(t *testing.T) {
	stubCatalog(t, map[string]catwalk.Model{
		"muse-spark-1.3-contributor": {
			ID: "muse-spark-1.3-contributor", Name: "Muse Spark 1.3 Contributor",
			ContextWindow: 1048576, DefaultMaxTokens: 131072,
		},
	})
	t.Setenv(LLMModelEnvVar, "muse-spark-1.3-contributor")
	e := llmExperiment()
	require.NoError(t, applyEnvModelOverride(e))
	require.Equal(t, "llm/muse-spark-1.3-contributor", e.Model)

	models := modelsOf(t, e, "llm")
	require.Len(t, models, 2)
	synth, ok := models[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "muse-spark-1.3-contributor", synth["id"])
	require.Equal(t, float64(1048576), synth["context_window"])
	require.Equal(t, float64(131072), synth["default_max_tokens"])
}

func TestEnvModelOverride_SlashedFormReplacesProvider(t *testing.T) {
	stubCatalog(t, nil)
	t.Setenv(LLMModelEnvVar, "opencode-go/muse-spark-1.3-contributor")
	e := llmExperiment()
	require.NoError(t, applyEnvModelOverride(e))
	require.Equal(t, "opencode-go/muse-spark-1.3-contributor", e.Model)
	// Builtin provider — nothing to synthesize into the block.
	require.Len(t, modelsOf(t, e, "llm"), 1)
}

func TestEnvModelOverride_SlashedFormKeepsDeclaredProvider(t *testing.T) {
	stubCatalog(t, map[string]catwalk.Model{
		"other-model": {ID: "other-model", Name: "Other", ContextWindow: 128000, DefaultMaxTokens: 8192},
	})
	t.Setenv(LLMModelEnvVar, "llm/other-model")
	e := llmExperiment()
	require.NoError(t, applyEnvModelOverride(e))
	require.Equal(t, "llm/other-model", e.Model)
	require.Len(t, modelsOf(t, e, "llm"), 2)
}

func TestEnvModelOverride_DeclaredModelNotDuplicated(t *testing.T) {
	t.Setenv(LLMModelEnvVar, "deepseek-v4.1-flash")
	e := llmExperiment()
	require.NoError(t, applyEnvModelOverride(e))
	require.Equal(t, "llm/deepseek-v4.1-flash", e.Model)
	require.Len(t, modelsOf(t, e, "llm"), 1)
}

func TestEnvModelOverride_CatalogMissRequiresContextWindow(t *testing.T) {
	stubCatalog(t, nil)
	t.Setenv(LLMModelEnvVar, "totally-unknown-model")
	e := llmExperiment()
	err := applyEnvModelOverride(e)
	require.Error(t, err)
	require.Contains(t, err.Error(), LLMContextWindowEnvVar)
}

func TestEnvModelOverride_CatalogMissSynthesizesFromEnv(t *testing.T) {
	stubCatalog(t, nil)
	t.Setenv(LLMModelEnvVar, "local-llm")
	t.Setenv(LLMContextWindowEnvVar, "65536")
	e := llmExperiment()
	require.NoError(t, applyEnvModelOverride(e))
	require.Equal(t, "llm/local-llm", e.Model)

	models := modelsOf(t, e, "llm")
	require.Len(t, models, 2)
	synth := models[1].(map[string]any)
	require.Equal(t, "local-llm", synth["id"])
	require.Equal(t, int64(65536), synth["context_window"])
	require.Equal(t, defaultSynthMaxTokens, synth["default_max_tokens"])
}

func TestEnvModelOverride_EnvBeatsCatalogMetadata(t *testing.T) {
	stubCatalog(t, map[string]catwalk.Model{
		"muse-spark-1.3-contributor": {
			ID: "muse-spark-1.3-contributor", ContextWindow: 1048576, DefaultMaxTokens: 131072,
		},
	})
	t.Setenv(LLMModelEnvVar, "muse-spark-1.3-contributor")
	t.Setenv(LLMContextWindowEnvVar, "262144")
	t.Setenv(LLMMaxTokensEnvVar, "32768")
	e := llmExperiment()
	require.NoError(t, applyEnvModelOverride(e))

	synth := modelsOf(t, e, "llm")[1].(map[string]any)
	require.Equal(t, int64(262144), synth["context_window"])
	require.Equal(t, int64(32768), synth["default_max_tokens"])
}

func TestEnvModelOverride_BareModelNeedsProvider(t *testing.T) {
	t.Setenv(LLMModelEnvVar, "some-model")
	e := llmExperiment()
	e.Model = "no-provider"
	err := applyEnvModelOverride(e)
	require.Error(t, err)
	require.Contains(t, err.Error(), "names no provider")
}

func TestEnvModelOverride_MalformedSlashRejected(t *testing.T) {
	t.Setenv(LLMModelEnvVar, "a/b/c")
	err := applyEnvModelOverride(llmExperiment())
	require.Error(t, err)
	require.Contains(t, err.Error(), "provider/model")
}

func TestEnvModelOverride_BadMetadataEnvRejected(t *testing.T) {
	stubCatalog(t, nil)
	t.Setenv(LLMModelEnvVar, "local-llm")
	t.Setenv(LLMContextWindowEnvVar, "notanumber")
	err := applyEnvModelOverride(llmExperiment())
	require.Error(t, err)
	require.Contains(t, err.Error(), LLMContextWindowEnvVar)
}

// TestLoadExperiment_EnvModelOverride exercises the integration: the
// env applies inside LoadExperiment, before validation.
func TestLoadExperiment_EnvModelOverride(t *testing.T) {
	stubCatalog(t, map[string]catwalk.Model{
		"muse-spark-1.3-contributor": {
			ID: "muse-spark-1.3-contributor", ContextWindow: 1048576, DefaultMaxTokens: 131072,
		},
	})
	t.Setenv(LLMModelEnvVar, "muse-spark-1.3-contributor")

	manifest := `{
		"name": "env-model",
		"model": "llm/deepseek-v4.1-flash",
		"temperature": 0,
		"corpus": ["warm-clean-seed"],
		"arms": {"control": {}, "treatment": {}},
		"providers": {
			"llm": {
				"name": "Eval LLM",
				"type": "openai-compat",
				"api_key": "$LLM_API_KEY",
				"base_url": "$LLM_BASE_URL",
				"models": [{"id": "deepseek-v4.1-flash", "name": "DeepSeek V4.1 Flash", "context_window": 1000000, "default_max_tokens": 384000}]
			}
		}
	}`
	path := filepath.Join(t.TempDir(), "exp.json")
	require.NoError(t, os.WriteFile(path, []byte(manifest), 0o644))

	e, err := LoadExperiment(path)
	require.NoError(t, err)
	require.Equal(t, "llm/muse-spark-1.3-contributor", e.Model)
	require.Len(t, modelsOf(t, e, "llm"), 2)
}
