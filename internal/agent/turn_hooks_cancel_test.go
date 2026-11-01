package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/stretchr/testify/require"
)

func endedTurnContext(t *testing.T, expired bool) context.Context {
	t.Helper()
	if expired {
		ctx, cancel := context.WithDeadline(t.Context(), time.Unix(1, 0))
		t.Cleanup(cancel)
		require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
		return ctx
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	return ctx
}

func TestTurnHooks_EndingsOutliveCaller(t *testing.T) {
	t.Parallel()
	for _, event := range []string{hooks.EventTurnEnd, hooks.EventStopFailure, hooks.EventInterrupt} {
		for _, cause := range []struct {
			name    string
			expired bool
		}{{"cancelled", false}, {"expired", true}} {
			t.Run(event+"/"+cause.name, func(t *testing.T) {
				t.Parallel()
				sa := &sessionAgent{}
				read := recordingHooks(t, sa, event)
				sa.fireTurnHook(endedTurnContext(t, cause.expired), SessionAgentCall{
					SessionID: "00000000-0000-4000-8000-000000000002",
				}, hooks.Event{Name: event})
				require.Equal(t, []string{event}, read())
			})
		}
	}
}

func TestTurnHooks_StartRemainsCancellationSensitive(t *testing.T) {
	t.Parallel()
	for _, expired := range []bool{false, true} {
		sa := &sessionAgent{}
		read := recordingHooks(t, sa, hooks.EventTurnStart)
		sa.fireTurnHook(endedTurnContext(t, expired), SessionAgentCall{
			SessionID: "00000000-0000-4000-8000-000000000002",
		}, hooks.Event{Name: hooks.EventTurnStart})
		require.Empty(t, read())
	}
}

func TestTurnHooks_DetachedEndingRetainsHookTimeout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	started, late := filepath.Join(dir, "started"), filepath.Join(dir, "late")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	sa := &sessionAgent{hooks: hooks.NewRunner(hooks.Static(map[string][]hooks.HookConfig{
		hooks.EventTurnEnd: {{
			Command: "printf started > " + quote(started) + "; sleep 30; printf late > " + quote(late),
			Timeout: 1,
		}},
	}), dir, dir)}
	begin := time.Now()
	sa.fireTurnHook(endedTurnContext(t, false), SessionAgentCall{
		SessionID: "00000000-0000-4000-8000-000000000002",
	}, hooks.Event{Name: hooks.EventTurnEnd})
	require.Less(t, time.Since(begin), 4*time.Second)
	body, err := os.ReadFile(started)
	require.NoError(t, err)
	require.Equal(t, "started", string(body))
	require.NoFileExists(t, late)
}
