package proto

import (
	"encoding/json"
	"testing"

	"charm.land/catwalk/v3/pkg/catwalk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProviderModelDualShape pins #532: the wire model must carry both
// the v3 nested fields and the legacy v2 flat fields, populated
// consistently, so neither generation of client reads missing fields.
func TestProviderModelDualShape(t *testing.T) {
	t.Parallel()

	m := ProviderModelFromCatwalk(catwalk.Model{
		ID:   "deepseek-v4.1-flash",
		Type: catwalk.TypeCompletions,
		Name: "DeepSeek V4.1 Flash",
		Pricing: catwalk.Pricing{
			Input:       0.07,
			Output:      1.10,
			CacheCreate: 0.14,
			CacheHit:    0.017,
		},
		ContextWindow:    262144,
		DefaultMaxTokens: 8192,
		Reasoning: catwalk.Reasoning{
			Thinking: catwalk.ThinkingToggleable,
			EffortLevels: []catwalk.EffortLevel{
				{Value: "low", Display: "Low"},
				{Value: "high", Display: "High"},
			},
			DefaultEffortLevel: "low",
		},
		Capabilities:   catwalk.Capabilities{Vision: true},
		MaxAttachments: 4,
	})

	// v3 fields pass through.
	assert.Equal(t, catwalk.ThinkingToggleable, m.Reasoning.Thinking)
	assert.True(t, m.Capabilities.Vision)
	assert.Equal(t, 4, m.MaxAttachments)

	// v2 fields derive consistently.
	assert.True(t, m.CanReason)
	assert.Equal(t, []string{"low", "high"}, m.ReasoningLevels)
	assert.Equal(t, "low", m.DefaultReasoningEffort)
	assert.True(t, m.SupportsAttachments)
	assert.InDelta(t, 0.07, m.CostPer1MIn, 1e-9)
	assert.InDelta(t, 1.10, m.CostPer1MOut, 1e-9)
	assert.InDelta(t, 0.14, m.CostPer1MInCached, 1e-9)
	assert.InDelta(t, 0.017, m.CostPer1MOutCached, 1e-9)

	// Both shapes must actually serialize onto the wire.
	var wire map[string]any
	require.NoError(t, json.Unmarshal(mustJSON(t, m), &wire))
	for _, key := range []string{
		"reasoning", "capabilities", "pricing",
		"can_reason", "supports_attachments",
		"cost_per_1m_in", "cost_per_1m_out",
	} {
		assert.Contains(t, wire, key)
	}
	assert.Equal(t, "low", wire["default_reasoning_effort"])
}

// TestProviderModelNonReasoning pins the v2 edge a v2 client depends
// on: a non-reasoning model must emit can_reason=false and no levels,
// and must not be confused with a reasoning one.
func TestProviderModelNonReasoning(t *testing.T) {
	t.Parallel()

	m := ProviderModelFromCatwalk(catwalk.Model{
		ID:   "plain-model",
		Name: "Plain",
		Reasoning: catwalk.Reasoning{
			Thinking: catwalk.ThinkingNever,
		},
	})
	assert.False(t, m.CanReason)
	assert.Empty(t, m.ReasoningLevels)
}

// TestProvidersFromCatwalk covers provider pass-through and nil
// handling.
func TestProvidersFromCatwalk(t *testing.T) {
	t.Parallel()

	assert.Nil(t, ProvidersFromCatwalk(nil))

	out := ProvidersFromCatwalk([]catwalk.Provider{{
		ID:                  catwalk.InferenceProvider("test"),
		Name:                "Test",
		APIEndpoint:         "https://example.invalid",
		DefaultLargeModelID: "large-1",
		Models: []catwalk.Model{{
			ID:   "large-1",
			Name: "Large",
		}},
	}})
	require.Len(t, out, 1)
	assert.Equal(t, "Test", out[0].Name)
	assert.Equal(t, "https://example.invalid", out[0].APIEndpoint)
	require.Len(t, out[0].Models, 1)
	assert.Equal(t, "large-1", out[0].Models[0].ID)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}
