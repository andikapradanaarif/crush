package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/notebook"
	"github.com/stretchr/testify/require"
)

// SessionTelemetryDelta is the per-turn emission basis for the
// persistent-process eval regime (#117): first call returns the full
// snapshot, subsequent calls return only what changed — so a process
// folding one file per turn never double-counts cumulative counters.
func TestSessionTelemetryDelta_PerTurnEmission(t *testing.T) {
	t.Parallel()

	sa := &sessionAgent{
		stubStats:   csync.NewMap[string, stubStats](),
		nbStats:     csync.NewMap[string, notebook.Stats](),
		reqStats:    csync.NewMap[string, requestStats](),
		usageLedger: csync.NewMap[string, ledgerUsage](),
		tailAudit:   csync.NewMap[string, TailAudit](),
		tailRuns:    csync.NewMap[string, []TailAudit](),
		edgeStats:   csync.NewMap[string, map[string]int](),
	}
	coord := &coordinator{
		agents:           map[string]SessionAgent{config.AgentCoder: sa},
		telemetryEmitted: csync.NewMap[string, SessionTelemetry](),
	}
	coord.mainAgent = sa
	coord.mainAgentName = config.AgentCoder

	sid := "s1"
	sa.reqStats.Set(sid, requestStats{
		Requests: 2,
		Steps:    []StepRecord{{Step: 0}, {Step: 1}},
	})
	sa.usageLedger.Set(sid, ledgerUsage{
		InputTokens: 100, OutputTokens: 20,
		Steps: 2,
	})
	sa.nbStats.Set(sid, notebook.Stats{ResultRecalls: 3, GeneratorCalls: 1})

	// First emission: full snapshot — identical to SessionTelemetry.
	first := coord.SessionTelemetryDelta(sid)
	require.Equal(t, coord.SessionTelemetry(sid).LedgerSteps, first.LedgerSteps)
	require.Equal(t, 2, first.LedgerSteps)
	require.Equal(t, int64(100), first.LedgerUsage.InputTokens)
	require.Len(t, first.Steps, 2)
	require.Equal(t, 3, first.ResultRecalls)
	require.Equal(t, 1, first.GeneratorCalls)
	require.Equal(t, int64(2), first.PromptRequests)

	// Turn 2 accumulates counters — the delta reports only the
	// increment, slices emit their new tail.
	sa.reqStats.Update(sid, func(r *requestStats) {
		r.Requests++
		r.Steps = append(r.Steps, StepRecord{Step: 2})
	})
	sa.usageLedger.Update(sid, func(u *ledgerUsage) {
		u.InputTokens += 40
		u.Steps++
	})
	sa.nbStats.Update(sid, func(n *notebook.Stats) {
		n.ResultRecalls++
	})

	second := coord.SessionTelemetryDelta(sid)
	require.Equal(t, 1, second.LedgerSteps)
	require.Equal(t, int64(40), second.LedgerUsage.InputTokens)
	require.Zero(t, second.LedgerUsage.OutputTokens, "unchanged counters subtract to zero")
	require.Len(t, second.Steps, 1)
	require.Equal(t, 2, second.Steps[0].Step)
	require.Equal(t, 1, second.ResultRecalls)
	require.Equal(t, int64(1), second.PromptRequests)

	// Nothing new emits a zero delta — the file still writes so the
	// turn's existence is recorded.
	third := coord.SessionTelemetryDelta(sid)
	require.Zero(t, third.LedgerSteps)
	require.Empty(t, third.Steps)
}
