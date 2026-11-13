package params

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Edges is the resolved parameter set for the run-edge layer
// (#408): the thresholds and per-edge enable flags the boundary
// machinery consults instead of compiled constants. Field names
// are the overlay keys in options.edge_params — stable contract
// surface, same as Memory's.
//
// Enable flags gate acting, not measuring: a disabled edge still
// scans and records edge_firings rows, but a trigger it would
// have fired resolves to "gated" instead of producing a repair
// turn — the same verdict the feature flags already record.
type Edges struct {
	// resolved marks a set produced by ResolveEdges. The zero
	// Edges{} is ambiguous — all-false flags are a legitimate
	// "every edge off" declaration — so OrDefault checks the
	// marker rather than comparing against the zero value.
	resolved bool

	// Enable flags, one per runEdgeSet edge. All default on —
	// today's behavior.
	VerificationEnabled bool `json:"verification_enabled"`
	TodosEnabled        bool `json:"todos_enabled"`
	StallEnabled        bool `json:"stall_enabled"`
	VerifyLoopEnabled   bool `json:"verify_loop_enabled"`
	BurnWatchEnabled    bool `json:"burn_watch_enabled"`
	ReconcileEnabled    bool `json:"reconcile_enabled"`
	FreshReviewEnabled  bool `json:"fresh_review_enabled"`

	// MaxRepairAttempts bounds the shared repair-turn budget every
	// firing edge draws from — zero disables repair turns outright.
	MaxRepairAttempts int `json:"max_repair_attempts"`

	// Burn-watch tripwires: the step-count arm fires on steps
	// alone; the token arm fires when a run has spent at least
	// MinSteps steps AND more than InputTokens input tokens with
	// no mutating call.
	BurnWatchStepsThreshold int   `json:"burn_watch_steps_threshold"`
	BurnWatchMinSteps       int   `json:"burn_watch_min_steps"`
	BurnWatchInputTokens    int64 `json:"burn_watch_input_tokens"`

	// VerifyLoopMinSteps is the post-write streak that trips the
	// verify-loop detector — that many consecutive trailing steps
	// after the last mutating call without another write.
	VerifyLoopMinSteps int `json:"verify_loop_min_steps"`

	// Loop-detector window: a tool signature repeated MaxRepeats
	// times within the last WindowSize steps trips the identical-
	// call detector.
	LoopDetectionWindowSize int `json:"loop_detection_window_size"`
	LoopDetectionMaxRepeats int `json:"loop_detection_max_repeats"`

	// Scope-gate boundaries: the explore→execute confirm arms at
	// MinExploration explorations; the two bounce budgets bound
	// rejected plan declarations and missing-evidence writes
	// before the loop escalates to the scope question.
	ScopeGateMinExploration       int `json:"scope_gate_min_exploration"`
	ScopeGatePlanBounceBudget     int `json:"scope_gate_plan_bounce_budget"`
	ScopeGateEvidenceBounceBudget int `json:"scope_gate_evidence_bounce_budget"`
}

// Skeleton bounds, same asymmetry contract as Memory: tightening
// is free (a tripwire that fires early nags, not corrupts),
// relaxing is bounded so a learned or overridden value can't turn
// a boundary into dead code.
const (
	// The repair bound can't exceed the synthetic-ordinal stride
	// (×8) in recordEdgeFiring — attempts past it would collide
	// with the next stamp's boundaries.
	maxRepairAttemptsMax = 7

	burnWatchStepsMin  = 5
	burnWatchStepsMax  = 500
	burnWatchTokensMin = 1_000
	burnWatchTokensMax = 100_000_000

	verifyLoopMinStepsMin = 2
	verifyLoopMinStepsMax = 100

	loopWindowMin  = 3
	loopWindowMax  = 100
	loopRepeatsMin = 2

	scopeExploreMin = 1
	scopeExploreMax = 200
	scopeBounceMax  = 20
)

// DefaultEdges is the shipped edge set — the values that were
// compiled constants before #408 moved them under the substrate.
func DefaultEdges() Edges {
	return Edges{
		resolved:                      true,
		VerificationEnabled:           true,
		TodosEnabled:                  true,
		StallEnabled:                  true,
		VerifyLoopEnabled:             true,
		BurnWatchEnabled:              true,
		ReconcileEnabled:              true,
		FreshReviewEnabled:            true,
		MaxRepairAttempts:             2,
		BurnWatchStepsThreshold:       30,
		BurnWatchMinSteps:             10,
		BurnWatchInputTokens:          200_000,
		VerifyLoopMinSteps:            8,
		LoopDetectionWindowSize:       10,
		LoopDetectionMaxRepeats:       5,
		ScopeGateMinExploration:       8,
		ScopeGatePlanBounceBudget:     3,
		ScopeGateEvidenceBounceBudget: 3,
	}
}

// OrDefault returns DefaultEdges for an unresolved value, so
// constructors can accept an optional override. The marker check
// — not a zero compare — is what lets an authored all-false flag
// set survive.
func (e Edges) OrDefault() Edges {
	if !e.resolved {
		return DefaultEdges()
	}
	return e
}

// EdgeEnabled reports whether the named runEdgeSet edge may fire —
// an unknown name reports enabled so an edge added in code before
// its flag exists can't silently dead-code.
func (e Edges) EdgeEnabled(name string) bool {
	switch name {
	case "verification":
		return e.VerificationEnabled
	case "todos":
		return e.TodosEnabled
	case "stall":
		return e.StallEnabled
	case "verify-loop":
		return e.VerifyLoopEnabled
	case "burn-watch":
		return e.BurnWatchEnabled
	case "reconcile":
		return e.ReconcileEnabled
	case "fresh-review":
		return e.FreshReviewEnabled
	}
	return true
}

// Validate enforces the skeleton bounds plus the two cross-field
// invariants — a token arm with a step floor past the step arm
// dead-codes itself, and a repeat cap past the window can never
// trip.
func (e Edges) Validate() error {
	if e.MaxRepairAttempts < 0 || e.MaxRepairAttempts > maxRepairAttemptsMax {
		return fmt.Errorf("edge param max_repair_attempts = %d — bound is [0, %d]", e.MaxRepairAttempts, maxRepairAttemptsMax)
	}
	if e.BurnWatchStepsThreshold < burnWatchStepsMin || e.BurnWatchStepsThreshold > burnWatchStepsMax {
		return fmt.Errorf("edge param burn_watch_steps_threshold = %d — bound is [%d, %d]", e.BurnWatchStepsThreshold, burnWatchStepsMin, burnWatchStepsMax)
	}
	if e.BurnWatchMinSteps < 1 || e.BurnWatchMinSteps > e.BurnWatchStepsThreshold {
		return fmt.Errorf("edge param burn_watch_min_steps = %d — bound is [1, burn_watch_steps_threshold]", e.BurnWatchMinSteps)
	}
	if e.BurnWatchInputTokens < burnWatchTokensMin || e.BurnWatchInputTokens > burnWatchTokensMax {
		return fmt.Errorf("edge param burn_watch_input_tokens = %d — bound is [%d, %d]", e.BurnWatchInputTokens, burnWatchTokensMin, burnWatchTokensMax)
	}
	if e.VerifyLoopMinSteps < verifyLoopMinStepsMin || e.VerifyLoopMinSteps > verifyLoopMinStepsMax {
		return fmt.Errorf("edge param verify_loop_min_steps = %d — bound is [%d, %d]", e.VerifyLoopMinSteps, verifyLoopMinStepsMin, verifyLoopMinStepsMax)
	}
	if e.LoopDetectionWindowSize < loopWindowMin || e.LoopDetectionWindowSize > loopWindowMax {
		return fmt.Errorf("edge param loop_detection_window_size = %d — bound is [%d, %d]", e.LoopDetectionWindowSize, loopWindowMin, loopWindowMax)
	}
	if e.LoopDetectionMaxRepeats < loopRepeatsMin || e.LoopDetectionMaxRepeats > e.LoopDetectionWindowSize {
		return fmt.Errorf("edge param loop_detection_max_repeats = %d — bound is [%d, loop_detection_window_size]", e.LoopDetectionMaxRepeats, loopRepeatsMin)
	}
	if e.ScopeGateMinExploration < scopeExploreMin || e.ScopeGateMinExploration > scopeExploreMax {
		return fmt.Errorf("edge param scope_gate_min_exploration = %d — bound is [%d, %d]", e.ScopeGateMinExploration, scopeExploreMin, scopeExploreMax)
	}
	if e.ScopeGatePlanBounceBudget < 1 || e.ScopeGatePlanBounceBudget > scopeBounceMax {
		return fmt.Errorf("edge param scope_gate_plan_bounce_budget = %d — bound is [1, %d]", e.ScopeGatePlanBounceBudget, scopeBounceMax)
	}
	if e.ScopeGateEvidenceBounceBudget < 1 || e.ScopeGateEvidenceBounceBudget > scopeBounceMax {
		return fmt.Errorf("edge param scope_gate_evidence_bounce_budget = %d — bound is [1, %d]", e.ScopeGateEvidenceBounceBudget, scopeBounceMax)
	}
	return nil
}

func (e Edges) canonical() []byte {
	// Same contract as Memory.canonical — field-declaration order,
	// so a new knob rotates the version.
	b, _ := json.Marshal(e)
	return b
}

// Version composes the resolved parameter snapshot's identity —
// memory plus edges — stamped as param_version on every memory
// row and telemetry record so a change to either set rotates the
// cohort marker (#408).
func Version(m Memory, e Edges) string {
	sum := sha256.Sum256(append(m.canonical(), e.canonical()...))
	return "pv1-" + hex.EncodeToString(sum[:])[:8]
}

// ResolveEdges applies an options.edge_params overlay onto the
// defaults — same contract as ResolveMemory: authored intent, so
// unknown keys, wrong types, and out-of-bounds values are hard
// errors, never silent defaults.
func ResolveEdges(overlay map[string]any) (Edges, error) {
	e := DefaultEdges()
	if len(overlay) == 0 {
		return e, nil
	}
	data, err := json.Marshal(overlay)
	if err != nil {
		return e, fmt.Errorf("edge params: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return e, fmt.Errorf("edge params: %w", err)
	}
	if err := e.Validate(); err != nil {
		return e, err
	}
	return e, nil
}
