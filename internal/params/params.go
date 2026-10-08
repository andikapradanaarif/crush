// Package params is the learned-parameters substrate (#228): the
// memory subsystem's tunable knobs as one typed, versioned,
// bound-checked value.
//
// The substrate is deliberately thin — a design contract before a
// store (docs/design/LEARNED_PARAMS.md). Parameters resolve as an
// authored overlay (options.memory_params) on compiled defaults;
// the resolved snapshot's Version stamps every memory row and
// telemetry record so eval cohorts can attribute outcomes to the
// parameter set that produced them. A persistent learned_params
// table arrives with the first promoted tenant, not before.
package params

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"time"
)

// Memory is the resolved parameter set for the memory/context
// subsystem — the values the failure-memory selector and the
// turn-context renderer consult instead of compiled constants.
// Field names are the overlay keys in options.memory_params; they
// are stable contract surface, since eval arms and (eventually)
// learned rows key on them.
type Memory struct {
	// WorkingSetLimit bounds the working-set section of the
	// per-turn blob. The read set is cumulative — unbounded it
	// degenerates to "every file ever touched".
	WorkingSetLimit int `json:"working_set_limit"`
	// VaguePromptMaxWords bounds the vagueness pre-filter: prompts
	// longer than this carry enough of their own context that a
	// missing referent is unlikely.
	VaguePromptMaxWords int `json:"vague_prompt_max_words"`
	// IntentMaxBytes bounds the rendered intent record. A statement
	// must never render truncated — a cut constraint reads as a
	// different instruction — so the budget drops whole oldest
	// items instead.
	IntentMaxBytes int `json:"intent_max_bytes"`
	// FileHeatLimit bounds the cross-session heat section — a hint
	// list, not working state, so it runs tighter than the
	// working-set cap.
	FileHeatLimit int `json:"file_heat_limit"`
	// FetchLimit bounds the candidate pool the selector sees —
	// bounded for fetch cost, wide enough that a relevant row past
	// the render cap still earns a decision record instead of
	// vanishing before selection. All three pools fetch under the
	// same bound.
	FetchLimit int `json:"fetch_limit"`
	// OpenRenderLimit bounds the failure-memory tail itself —
	// recent-first, so the cap keeps the freshest bound rows; rows
	// it cuts record render_capped, not silence.
	OpenRenderLimit int `json:"open_render_limit"`
	// ResolvedRenderLimit and CommandRenderLimit bound the
	// knowledge pools — subordinate to open failures: the pressure
	// regime showed injection quality degrades before capacity runs
	// out, so the knowledge envelopes cap tighter than the warning
	// envelope they supplement.
	ResolvedRenderLimit int `json:"resolved_render_limit"`
	CommandRenderLimit  int `json:"command_render_limit"`
	// FailureFileHints bounds file hints rendered per failure row.
	FailureFileHints int `json:"failure_file_hints"`
	// FailureCmdRunes and FailureHeadlineRunes are render-side caps
	// on echoed failure fields — write-side caps already bound
	// them, these keep the tail bounded regardless.
	FailureCmdRunes      int `json:"failure_cmd_runes"`
	FailureHeadlineRunes int `json:"failure_headline_runes"`
	// OpenFailureTTL is the read-side staleness bound for failure
	// memory — the name under-sells the scope: ListResolvedFailures
	// and session reads share the same failuresFromRows filter, so
	// a tightening erases resolved knowledge too, not just open
	// warnings. 0 disables the filter. Skeleton bound for a learned
	// value: [24h, 720h].
	OpenFailureTTL time.Duration `json:"open_failure_ttl"`
}

// Skeleton bounds: the asymmetry contract — tightening is free,
// relaxing is bounded. A learned or overridden value may tighten
// any cap to 0 (suppress the section entirely); relax direction is
// unconstrained only where the skeleton hasn't fixed a bound.
// OpenFailureTTL carries the one explicit skeleton bound so far:
// longer than 30d reads stale memory, shorter than 1d barely
// counts as memory — 0 stays allowed as an authored disable, not
// a learnable value.
const (
	openFailureTTLMin = 24 * time.Hour
	openFailureTTLMax = 30 * 24 * time.Hour
)

// DefaultMemory is the shipped parameter set — the values that
// were compiled constants before the substrate existed.
func DefaultMemory() Memory {
	return Memory{
		WorkingSetLimit:      10,
		VaguePromptMaxWords:  12,
		IntentMaxBytes:       4096,
		FileHeatLimit:        5,
		FetchLimit:           50,
		OpenRenderLimit:      5,
		ResolvedRenderLimit:  3,
		CommandRenderLimit:   3,
		FailureFileHints:     3,
		FailureCmdRunes:      200,
		FailureHeadlineRunes: 140,
		OpenFailureTTL:       30 * 24 * time.Hour,
	}
}

// OrDefault returns DefaultMemory for the zero value, so
// constructors can accept an optional override without every test
// fixture spelling out the full set.
func (m Memory) OrDefault() Memory {
	if m == (Memory{}) {
		return DefaultMemory()
	}
	return m
}

// Version is the parameter snapshot's identity — stamped on every
// memory row and telemetry record so a cohort of outcomes
// attributes to the values that produced it. "pv0" is the
// pre-substrate marker on rows written before this mechanism
// existed; resolved snapshots are "pv1-<hash>".
func (m Memory) Version() string {
	sum := sha256.Sum256(m.canonical())
	return "pv1-" + hex.EncodeToString(sum[:])[:8]
}

func (m Memory) canonical() []byte {
	// Struct marshaling is field-declaration ordered, so the hash
	// input is stable across processes. Adding a field rotates the
	// version — intended: a new knob means a different snapshot.
	b, _ := json.Marshal(m)
	return b
}

// Validate enforces the skeleton bounds — the asymmetry contract
// in checkable form.
func (m Memory) Validate() error {
	v := reflectValues(m)
	for name, n := range v {
		if n < 0 {
			return fmt.Errorf("memory param %s = %d — bounds do not admit negative values", name, n)
		}
	}
	if m.OpenFailureTTL != 0 &&
		(m.OpenFailureTTL < openFailureTTLMin || m.OpenFailureTTL > openFailureTTLMax) {
		return fmt.Errorf("memory param open_failure_ttl = %s — skeleton bound is 0 (disabled) or [%s, %s]",
			m.OpenFailureTTL, openFailureTTLMin, openFailureTTLMax)
	}
	return nil
}

// reflectValues maps each integer field to its value for the
// nonnegativity sweep — kept explicit (not reflect) so the
// contract names what it checks.
func reflectValues(m Memory) map[string]int64 {
	return map[string]int64{
		"working_set_limit":      int64(m.WorkingSetLimit),
		"vague_prompt_max_words": int64(m.VaguePromptMaxWords),
		"intent_max_bytes":       int64(m.IntentMaxBytes),
		"file_heat_limit":        int64(m.FileHeatLimit),
		"fetch_limit":            int64(m.FetchLimit),
		"open_render_limit":      int64(m.OpenRenderLimit),
		"resolved_render_limit":  int64(m.ResolvedRenderLimit),
		"command_render_limit":   int64(m.CommandRenderLimit),
		"failure_file_hints":     int64(m.FailureFileHints),
		"failure_cmd_runes":      int64(m.FailureCmdRunes),
		"failure_headline_runes": int64(m.FailureHeadlineRunes),
	}
}

// ResolveMemory applies an options.memory_params overlay onto the
// defaults. The overlay is authored intent — an eval arm's config
// or a user's crush.json — so anything it cannot parse is a hard
// error, never a silent default: unknown keys, wrong types, and
// out-of-bounds values all fail rather than quietly running a
// different experiment than declared.
//
// open_failure_ttl accepts a Go duration string ("168h") or a
// number of seconds; every other key takes a JSON integer.
func ResolveMemory(overlay map[string]any) (Memory, error) {
	m := DefaultMemory()
	if len(overlay) == 0 {
		return m, nil
	}
	overlay = maps.Clone(overlay)
	if raw, ok := overlay["open_failure_ttl"]; ok {
		switch v := raw.(type) {
		case string:
			d, err := time.ParseDuration(v)
			if err != nil {
				return m, fmt.Errorf("memory param open_failure_ttl: %q is not a duration (e.g. \"168h\"): %w", v, err)
			}
			overlay["open_failure_ttl"] = int64(d)
		case float64:
			overlay["open_failure_ttl"] = int64(v * float64(time.Second))
		case int:
			overlay["open_failure_ttl"] = int64(v) * int64(time.Second)
		case int64:
			overlay["open_failure_ttl"] = v * int64(time.Second)
		default:
			return m, fmt.Errorf("memory param open_failure_ttl: want a duration string or seconds, got %T", raw)
		}
	}
	data, err := json.Marshal(overlay)
	if err != nil {
		return m, fmt.Errorf("memory params: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("memory params: %w", err)
	}
	if err := m.Validate(); err != nil {
		return m, err
	}
	return m, nil
}
