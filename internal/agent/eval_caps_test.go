package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEvalStepCaps_NoneWithoutEnv(t *testing.T) {
	require.Empty(t, evalStepCaps(func() int64 { return 0 }))
}

func TestEvalStepCaps_Steps(t *testing.T) {
	t.Setenv(EvalMaxStepsEnvVar, "3")
	caps := evalStepCaps(func() int64 { return 0 })
	require.Len(t, caps, 1)
}

func TestEvalStepCaps_Tokens(t *testing.T) {
	t.Setenv(EvalMaxTokensEnvVar, "1000")
	var total int64 = 999
	caps := evalStepCaps(func() int64 { return total })
	require.Len(t, caps, 1)
	require.False(t, caps[0](nil))
	total = 1001
	require.True(t, caps[0](nil), "cap fires at/above the token budget")
}

func TestEvalStepCaps_TokensNeedMeter(t *testing.T) {
	t.Setenv(EvalMaxTokensEnvVar, "1000")
	require.Empty(t, evalStepCaps(nil), "no token cap without a token meter")
}
