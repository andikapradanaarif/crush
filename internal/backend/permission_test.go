package backend

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

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
	require.NoError(t, b.SetSessionPermissionsSkip(ctx, ws.ID, sess.ID, true))
	payload := awaitPermissionsSkipChanged(t, evc, ws.ID, sess.ID)
	require.True(t, payload.Skip)

	skip, err := b.GetSessionPermissionsSkip(ctx, ws.ID, sess.ID)
	require.NoError(t, err)
	require.True(t, skip)

	// Disarm: the flag flips back and the event follows.
	require.NoError(t, b.SetSessionPermissionsSkip(ctx, ws.ID, sess.ID, false))
	payload = awaitPermissionsSkipChanged(t, evc, ws.ID, sess.ID)
	require.False(t, payload.Skip)

	skip, err = b.GetSessionPermissionsSkip(ctx, ws.ID, sess.ID)
	require.NoError(t, err)
	require.False(t, skip)

	// The workspace-global flag is orthogonal: arming it neither
	// flips nor reports through the per-session readback.
	require.NoError(t, b.SetPermissionsSkip(ws.ID, true))
	payload = awaitPermissionsSkipChanged(t, evc, ws.ID, "")
	require.True(t, payload.Skip)

	skip, err = b.GetSessionPermissionsSkip(ctx, ws.ID, sess.ID)
	require.NoError(t, err)
	require.False(t, skip, "global skip must not leak into the per-session flag")
}

// TestSessionPermissionsSkip_UnknownSession verifies the
// session-scoped endpoints surface the same error as other
// session-scoped backend calls when the session does not exist.
func TestSessionPermissionsSkip_UnknownSession(t *testing.T) {
	b, ws, evc := newPublishingWorkspace(t)
	ctx := t.Context()

	require.Error(t, b.SetSessionPermissionsSkip(ctx, ws.ID, "no-such-session", true))
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
