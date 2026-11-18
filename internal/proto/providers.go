package proto

import "charm.land/catwalk/v3/pkg/catwalk"

// Provider is the stable wire shape for
// GET /v1/workspaces/{id}/providers. The endpoint serializes this type
// instead of catwalk.Provider directly, so catwalk library bumps cannot
// silently change the wire contract again (see #532).
type Provider struct {
	Name                  string                    `json:"name"`
	ID                    catwalk.InferenceProvider `json:"id"`
	APIKey                string                    `json:"api_key,omitempty"`
	APIEndpoint           string                    `json:"api_endpoint,omitempty"`
	Type                  catwalk.Type              `json:"type,omitempty"`
	DefaultLargeModelID   string                    `json:"default_large_model_id,omitempty"`
	DefaultSmallModelID   string                    `json:"default_small_model_id,omitempty"`
	Models                []ProviderModel           `json:"models,omitempty"`
	DefaultHeaders        map[string]string         `json:"default_headers,omitempty"`
	SessionAffinityHeader string                    `json:"session_affinity_header,omitempty"`
}

// ProviderModel is the stable wire shape for a model inside Provider.
// It carries the catwalk v3 nested fields and the legacy v2 flat fields
// populated from them, so clients written against either schema keep
// working. New consumers should read the v3 fields; the flat v2 fields
// are frozen for compatibility and will not be extended.
type ProviderModel struct {
	ID               string               `json:"id"`
	Type             catwalk.Type         `json:"type,omitempty"`
	Name             string               `json:"name"`
	Pricing          catwalk.Pricing      `json:"pricing"`
	ContextWindow    int64                `json:"context_window"`
	DefaultMaxTokens int64                `json:"default_max_tokens"`
	Reasoning        catwalk.Reasoning    `json:"reasoning"`
	Capabilities     catwalk.Capabilities `json:"capabilities"`
	MaxAttachments   int                  `json:"max_attachments,omitempty"`
	Options          catwalk.ModelOptions `json:"options,omitzero"`

	// Legacy v2 fields, derived from the nested fields above.
	CostPer1MIn            float64  `json:"cost_per_1m_in"`
	CostPer1MOut           float64  `json:"cost_per_1m_out"`
	CostPer1MInCached      float64  `json:"cost_per_1m_in_cached"`
	CostPer1MOutCached     float64  `json:"cost_per_1m_out_cached"`
	CanReason              bool     `json:"can_reason"`
	ReasoningLevels        []string `json:"reasoning_levels,omitempty"`
	DefaultReasoningEffort string   `json:"default_reasoning_effort,omitempty"`
	SupportsAttachments    bool     `json:"supports_attachments"`
}

// ProviderModelFromCatwalk converts a catwalk v3 model to the wire
// shape, filling the legacy v2 fields from their v3 counterparts — the
// same mapping catwalk's own v2compat uses.
func ProviderModelFromCatwalk(m catwalk.Model) ProviderModel {
	out := ProviderModel{
		ID:                     m.ID,
		Type:                   m.Type,
		Name:                   m.Name,
		Pricing:                m.Pricing,
		ContextWindow:          m.ContextWindow,
		DefaultMaxTokens:       m.DefaultMaxTokens,
		Reasoning:              m.Reasoning,
		Capabilities:           m.Capabilities,
		MaxAttachments:         m.MaxAttachments,
		Options:                m.Options,
		CostPer1MIn:            m.Pricing.Input,
		CostPer1MOut:           m.Pricing.Output,
		CostPer1MInCached:      m.Pricing.CacheCreate,
		CostPer1MOutCached:     m.Pricing.CacheHit,
		CanReason:              m.Reasoning.Thinking != catwalk.ThinkingNever,
		DefaultReasoningEffort: m.Reasoning.DefaultEffortLevel,
		SupportsAttachments:    m.Capabilities.Vision,
	}
	if out.CanReason && len(m.Reasoning.EffortLevels) > 0 {
		out.ReasoningLevels = make([]string, 0, len(m.Reasoning.EffortLevels))
		for _, level := range m.Reasoning.EffortLevels {
			out.ReasoningLevels = append(out.ReasoningLevels, level.Value)
		}
	}
	return out
}

// ProviderFromCatwalk converts a catwalk v3 provider to the wire shape.
func ProviderFromCatwalk(p catwalk.Provider) Provider {
	out := Provider{
		Name:                  p.Name,
		ID:                    p.ID,
		APIKey:                p.APIKey,
		APIEndpoint:           p.APIEndpoint,
		Type:                  p.Type,
		DefaultLargeModelID:   p.DefaultLargeModelID,
		DefaultSmallModelID:   p.DefaultSmallModelID,
		DefaultHeaders:        p.DefaultHeaders,
		SessionAffinityHeader: p.SessionAffinityHeader,
	}
	if len(p.Models) > 0 {
		out.Models = make([]ProviderModel, 0, len(p.Models))
		for _, m := range p.Models {
			out.Models = append(out.Models, ProviderModelFromCatwalk(m))
		}
	}
	return out
}

// ProvidersFromCatwalk converts a catwalk v3 provider slice to the wire
// shape.
func ProvidersFromCatwalk(providers []catwalk.Provider) []Provider {
	if providers == nil {
		return nil
	}
	out := make([]Provider, 0, len(providers))
	for _, p := range providers {
		out = append(out, ProviderFromCatwalk(p))
	}
	return out
}
