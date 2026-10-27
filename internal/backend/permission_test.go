package backend

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// ptr returns a *bool for the tri-state session override API.
func ptr(v bool) *bool { return &v }

// awaitPermissionsSkipChanged drains events until a
// PermissionsSkipChanged is received for the given workspace ID, or
// fails the test on timeout. Other event types are ignored.
func awaitPermissionsSkipChanged(t *testing.T, evc <-chan pubsub.Event[tea.Msg], workspaceID, sessionID string) proto.PermissionsSkipChanged {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-evc:
			if !ok {
				t.Fatal("event channel closed before PermissionsSkipChanged arrived")
			}
			psc, ok := ev.Payload.(pubsub.Event[proto.PermissionsSkipChanged])
			if !ok {
				continue
			}
			require.Equal(t, workspaceID, psc.Payload.WorkspaceID)
			require.Equal(t, sessionID, psc.Payload.SessionID)
			return psc.Payload
		case <-deadline:
			t.Fatal("timed out waiting for PermissionsSkipChanged event")
		}
	}
}

// TestSetPermissionsSkip_PublishesEvent covers the workspace-global
// flag: the change must reach SSE subscribers, and the event carries
// an empty SessionID so it broadcasts workspace-wide.
func TestSetPermissionsSkip_PublishesEvent(t *testing.T) {
	b, ws, evc := newPublishingWorkspace(t)

	require.NoError(t, b.SetPermissionsSkip(ws.ID, true))
	payload := awaitPermissionsSkipChanged(t, evc, ws.ID, "")
	require.True(t, payload.Skip)

	skip, err := b.GetPermissionsSkip(ws.ID)
	require.NoError(t, err)
	require.True(t, skip)
}

// TestSetSessionPermissionsSkip_PublishesEvent covers the per-session
// flag: set/unset round-trip, event scoping via SessionID, and
// orthogonality with the workspace-global flag.
func TestSetSessionPermissionsSkip_PublishesEvent(t *testing.T) {
	b, ws, evc := newPublishingWorkspace(t)
	ctx := t.Context()

	sess, err := ws.Sessions.Create(ctx, "yolo session")
	require.NoError(t, err)

	// Arm the session flag: event carries the session ID and the
	// readback reports it armed.
	require.NoError(t, b.SetSessionPermissionsSkip(ctx, ws.ID, sess.ID, ptr(true)))
	payload := awaitPermissionsSkipChanged(t, evc, ws.ID, sess.ID)
	require.True(t, payload.Skip)

	skip, err := b.GetSessionPermissionsSkip(ctx, ws.ID, sess.ID)
	require.NoError(t, err)
	require.True(t, skip)

	// Disarm: the flag flips back and the event follows.
	require.NoError(t, b.SetSessionPermissionsSkip(ctx, ws.ID, sess.ID, ptr(false)))
	payload = awaitPermissionsSkipChanged(t, evc, ws.ID, sess.ID)
	require.False(t, payload.Skip)

	skip, err = b.GetSessionPermissionsSkip(ctx, ws.ID, sess.ID)
	require.NoError(t, err)
	require.False(t, skip)

	// Clear: a nil override returns the session to inherit mode —
	// the persisted row must read NULL again. The clear publishes
	// the effective state like any other transition.
	require.NoError(t, b.SetSessionPermissionsSkip(ctx, ws.ID, sess.ID, nil))
	payload = awaitPermissionsSkipChanged(t, evc, ws.ID, sess.ID)
	require.False(t, payload.Skip, "cleared override inherits the unset workspace flag")
	reloaded, err := ws.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Nil(t, reloaded.PermissionSkip, "cleared override must persist as NULL")

	// The workspace-global flag is orthogonal to the persisted
	// override: with no override the readback reports the effective
	// state (true under workspace skip), and disarming the workspace
	// flag shows nothing was persisted per-session.
	require.NoError(t, b.SetPermissionsSkip(ws.ID, true))
	payload = awaitPermissionsSkipChanged(t, evc, ws.ID, "")
	require.True(t, payload.Skip)

	skip, err = b.GetSessionPermissionsSkip(ctx, ws.ID, sess.ID)
	require.NoError(t, err)
	require.True(t, skip, "unflagged session inherits the workspace flag")

	require.NoError(t, b.SetPermissionsSkip(ws.ID, false))
	payload = awaitPermissionsSkipChanged(t, evc, ws.ID, "")
	require.False(t, payload.Skip)
	skip, err = b.GetSessionPermissionsSkip(ctx, ws.ID, sess.ID)
	require.NoError(t, err)
	require.False(t, skip, "workspace skip must not leave the session flag armed")
	reloaded, err = ws.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Nil(t, reloaded.PermissionSkip, "workspace skip must not persist a session override")
}

// TestSessionPermissionsSkip_UnknownSession verifies the
// session-scoped endpoints surface the same error as other
// session-scoped backend calls when the session does not exist.
func TestSessionPermissionsSkip_UnknownSession(t *testing.T) {
	b, ws, evc := newPublishingWorkspace(t)
	ctx := t.Context()

	require.Error(t, b.SetSessionPermissionsSkip(ctx, ws.ID, "no-such-session", ptr(true)))
	_, err := b.GetSessionPermissionsSkip(ctx, ws.ID, "no-such-session")
	require.Error(t, err)

	// A rejected set must not publish.
	select {
	case ev := <-evc:
		if psc, ok := ev.Payload.(pubsub.Event[proto.PermissionsSkipChanged]); ok {
			t.Fatalf("failed set must not publish: %+v", psc.Payload)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

// TestRepublishPendingPrompts_ReplaysAll covers the multi-park case:
// two requests parked on session A plus one on session B — reporting
// session A must re-publish both of A's requests and none of B's.
func TestRepublishPendingPrompts_ReplaysAll(t *testing.T) {
	b, ws, evc := newPublishingWorkspace(t)
	ctx := t.Context()

	sessA, err := ws.Sessions.Create(ctx, "session a")
	require.NoError(t, err)
	sessB, err := ws.Sessions.Create(ctx, "session b")
	require.NoError(t, err)

	park := func(sessionID, callID string) {
		go func() {
			_, _ = ws.Permissions.Request(ctx, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				ToolCallID:  callID,
				ToolName:    "bash",
				Action:      "execute",
				Description: "park for replay",
				Path:        "/tmp",
			})
		}()
	}
	park(sessA.ID, "a-1")
	park(sessA.ID, "a-2")
	park(sessB.ID, "b-1")

	require.Eventually(t, func() bool {
		return len(ws.Permissions.PendingRequests()) == 3
	}, 2*time.Second, 5*time.Millisecond, "all three requests must park concurrently")

	// Flush the original publish and session-creation events so the
	// channel only carries what republishPendingPrompts re-emits.
	drainEvents(evc, 200*time.Millisecond)

	b.republishPendingPrompts(ctx, ws, sessA.ID)

	got := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ev, ok := <-evc:
			if !ok {
				t.Fatal("event channel closed before replay events arrived")
			}
			pe, ok := ev.Payload.(pubsub.Event[permission.PermissionRequest])
			if !ok {
				continue
			}
			require.Equal(t, sessA.ID, pe.Payload.SessionID,
				"a parked request for another session must not replay")
			got[pe.Payload.ToolCallID] = true
		case <-deadline:
			t.Fatalf("expected both parked requests to replay, got %v", got)
		}
	}
	require.True(t, got["a-1"], "first parked request must replay")
	require.True(t, got["a-2"], "second parked request must replay")

	// Nothing else should follow: session B's parked request stays out
	// of A's replay.
	select {
	case ev := <-evc:
		if pe, ok := ev.Payload.(pubsub.Event[permission.PermissionRequest]); ok {
			t.Fatalf("unexpected extra replay: %+v", pe.Payload)
		}
	case <-time.After(200 * time.Millisecond):
	}
}

// TestGetSessionPendingPermissions verifies the pending-list backend
// read: parked requests for the session are returned, other sessions'
// requests are excluded, and an unknown session errors.
func TestGetSessionPendingPermissions(t *testing.T) {
	b, ws, _ := newPublishingWorkspace(t)
	ctx := t.Context()

	sessA, err := ws.Sessions.Create(ctx, "session a")
	require.NoError(t, err)
	sessB, err := ws.Sessions.Create(ctx, "session b")
	require.NoError(t, err)

	go func() {
		_, _ = ws.Permissions.Request(ctx, permission.CreatePermissionRequest{
			SessionID:   sessA.ID,
			ToolCallID:  "a-1",
			ToolName:    "bash",
			Action:      "execute",
			Description: "park for listing",
			Path:        "/tmp",
		})
	}()
	go func() {
		_, _ = ws.Permissions.Request(ctx, permission.CreatePermissionRequest{
			SessionID:   sessB.ID,
			ToolCallID:  "b-1",
			ToolName:    "bash",
			Action:      "execute",
			Description: "park for listing",
			Path:        "/tmp",
		})
	}()

	require.Eventually(t, func() bool {
		return len(ws.Permissions.PendingRequests()) == 2
	}, 2*time.Second, 5*time.Millisecond)

	pending, err := b.GetSessionPendingPermissions(ctx, ws.ID, sessA.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "a-1", pending[0].ToolCallID)

	_, err = b.GetSessionPendingPermissions(ctx, ws.ID, "no-such-session")
	require.Error(t, err)
}

// TestSetSessionModel covers the session-scoped model override:
// unknown sessions are rejected, a valid selection persists as JSON
// and a nil selection clears the row back to inherit.
func TestSetSessionModel(t *testing.T) {
	b, ws, _ := newPublishingWorkspace(t)
	ctx := t.Context()

	// The publishing fixture has no agent config, so its coordinator
	// is nil — stub one. Runtime model building is covered by the
	// agent-level override tests; here the persist/clear path and
	// session filtering are under test.
	ws.AgentCoordinator = &busyCoordinator{}

	sel := &config.SelectedModel{Provider: "openai", Model: "gpt-4o"}
	require.Error(t, b.SetSessionModel(ctx, ws.ID, "no-such-session", sel),
		"unknown session must be rejected")

	sess, err := ws.Sessions.Create(ctx, "model session")
	require.NoError(t, err)

	require.NoError(t, b.SetSessionModel(ctx, ws.ID, sess.ID, sel))
	reloaded, err := ws.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{"provider":"openai","model":"gpt-4o"}`,
		string(reloaded.ModelOverride))

	// Clearing restores inherit: the row reads NULL again.
	require.NoError(t, b.SetSessionModel(ctx, ws.ID, sess.ID, nil))
	reloaded, err = ws.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Nil(t, reloaded.ModelOverride)
}
