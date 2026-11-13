package agent

import "github.com/charmbracelet/crush/internal/params"

// Test-local aliases for the thresholds that moved to params.Edges
// (#408) — bound to the shipped defaults rather than literals, so
// the pre-existing edge tests keep pinning today's exact behavior:
// a default that drifts from the compiled constants it replaced
// fails these tests.
var (
	loopDetectionWindowSize       = params.DefaultEdges().LoopDetectionWindowSize
	loopDetectionMaxRepeats       = params.DefaultEdges().LoopDetectionMaxRepeats
	verifyLoopMinSteps            = params.DefaultEdges().VerifyLoopMinSteps
	scopeGateMinExploration       = params.DefaultEdges().ScopeGateMinExploration
	scopeGatePlanBounceBudget     = params.DefaultEdges().ScopeGatePlanBounceBudget
	scopeGateEvidenceBounceBudget = params.DefaultEdges().ScopeGateEvidenceBounceBudget
	maxRepairAttempts             = params.DefaultEdges().MaxRepairAttempts
	burnWatchStepsThreshold       = params.DefaultEdges().BurnWatchStepsThreshold
	burnWatchMinSteps             = params.DefaultEdges().BurnWatchMinSteps
	burnWatchInputTokens          = params.DefaultEdges().BurnWatchInputTokens
)
