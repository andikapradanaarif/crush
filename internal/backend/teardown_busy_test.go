package backend

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
)

// busyCoordinator is a minimal agent.Coordinator whose IsBusy reports
// a flag the test can flip — modeling a workspace with a run in
// flight. The flag is atomic because the deferred teardown re-check
// reads it from its own goroutine.
type busyCoordinator struct {
	busy atomic.Bool
}

func (c *busyCoordinator) SetMainAgent(string) error { return nil }
func (c *busyCoordinator) ApprovePlan(context.Context, string) error {
	return nil
}

func (c *busyCoordinator) Run(context.Context, string, string, ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (c *busyCoordinator) RunAccepted(context.Context, *agent.AcceptedRun, string, string, ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, nil
}
func (c *busyCoordinator) BeginAccepted(string) *agent.AcceptedRun { return nil }
func (c *busyCoordinator) Cancel(string)                           {}
func (c *busyCoordinator) CancelAll()                              {}
func (c *busyCoordinator) IsBusy() bool                            { return c.busy.Load() }
func (c *busyCoordinator) IsSessionBusy(string) bool               { return false }
func (c *busyCoordinator) QueuedPrompts(string) int                { return 0 }
func (c *busyCoordinator) QueuedPromptsList(string) []string       { return nil }
func (c *busyCoordinator) ClearQueue(string)                       {}
func (c *busyCoordinator) Summarize(context.Context, string) error {
	return nil
}
func (c *busyCoordinator) Model() agent.Model                            { return agent.Model{} }
func (c *busyCoordinator) UpdateModels(context.Context) error            { return nil }
func (c *busyCoordinator) UpdateSummaryModel(context.Context) error      { return nil }
func (c *busyCoordinator) GenerateTitle(context.Context, string, string) {}
func (c *busyCoordinator) SetSessionModelOverride(context.Context, string, config.SelectedModel) error {
	return nil
}
func (c *busyCoordinator) ClearSessionModelOverride(string) {}
func (c *busyCoordinator) ListSettledMessages(context.Context, string) ([]message.Message, error) {
	return nil, nil
}

func insertBusyWorkspace(t *testing.T, b *Backend, coord agent.Coordinator, shutdowns *atomic.Int32) *Workspace {
	t.Helper()
	ws := &Workspace{
		ID:           newClientID(t),
		Path:         t.TempDir(),
		resolvedPath: t.TempDir(),
		clients:      make(map[string]*clientState),
		shutdownFn:   func() { shutdowns.Add(1) },
	}
	ws.App = &app.App{AgentCoordinator: coord}
	ws.ctx, ws.cancel = context.WithCancel(b.ctx)
	b.mu.Lock()
	b.workspaces.Set(ws.ID, ws)
	b.pathIndex[ws.resolvedPath] = ws.ID
	b.mu.Unlock()
	return ws
}

// TestTeardown_BusyWorkspaceDefersShutdown proves teardown refuses to
// invokeShutdown a workspace whose coordinator has a run in flight —
// killing the workspace context mid-run is what surfaced as the
// spurious "user cancelled assistant tool calling" error. Instead the
// workspace stays registered and a deferred re-check tears it down
// once the run finishes.
func TestTeardown_BusyWorkspaceDefersShutdown(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	var shutdowns atomic.Int32
	coord := &busyCoordinator{}
	coord.busy.Store(true)
	ws := insertBusyWorkspace(t, b, coord, &shutdowns)

	b.teardown(ws)

	require.Equal(t, int32(0), shutdowns.Load(),
		"a busy workspace must not be shut down")
	_, ok := b.workspaces.Get(ws.ID)
	require.True(t, ok, "a busy workspace must stay registered")

	// The deferred re-check keeps polling; once the run ends the
	// workspace is removed and shut down.
	coord.busy.Store(false)
	require.Eventually(t, func() bool {
		_, registered := b.workspaces.Get(ws.ID)
		return !registered && shutdowns.Load() == 1
	}, 3*time.Second, 20*time.Millisecond,
		"the deferred teardown must complete once the workspace goes idle")
}

// TestTeardown_IdleWorkspaceShutsDownImmediately pins the unchanged
// idle path: no run in flight means teardown proceeds on the first
// pass.
func TestTeardown_IdleWorkspaceShutsDownImmediately(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	var shutdowns atomic.Int32
	coord := &busyCoordinator{}
	ws := insertBusyWorkspace(t, b, coord, &shutdowns)

	b.teardown(ws)

	require.Equal(t, int32(1), shutdowns.Load())
	_, ok := b.workspaces.Get(ws.ID)
	require.False(t, ok)
}

// TestTeardown_IdempotentBusyRecheck proves a workspace removed while
// busy-deferred does not get a second shutdown — the membership check
// makes the re-armed pass a no-op.
func TestTeardown_ConcurrentCallsShutDownOnce(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	var shutdowns atomic.Int32
	coord := &busyCoordinator{}
	ws := insertBusyWorkspace(t, b, coord, &shutdowns)

	b.teardown(ws)
	b.teardown(ws)

	require.Equal(t, int32(1), shutdowns.Load(),
		"concurrent teardowns must not double-shutdown")
}
