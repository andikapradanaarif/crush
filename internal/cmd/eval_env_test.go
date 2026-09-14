package cmd

import (
	"testing"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/eval"
	"github.com/stretchr/testify/require"
)

// The CRUSH_EVAL_* contract is duplicated across packages — eval
// (driver), app (telemetry emission), agent (step cap). A drift
// silently breaks extraction or misclassifies which budget fired.
func TestEvalEnvVarConstantsAgree(t *testing.T) {
	require.Equal(t, app.EvalTelemetryEnvVar, eval.EvalTelemetryEnvVar)
	require.Equal(t, app.EvalFlagsEnvVar, eval.EvalFlagsEnvVar)
	require.Equal(t, agent.EvalMaxStepsEnvVar, eval.EvalMaxStepsEnvVar)
}
