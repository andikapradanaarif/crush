package model

import (
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// newTestUIForPermissions builds a UI with a chat, dialog overlay, and
// common context sufficient to exercise handlePermissionNotification.
// The workspace stub backs Config(), which openPermissionsDialog reads
// for the diff-mode option.
func newTestUIForPermissions() *UI {
	u := newTestUI()
	u.com.Workspace = &testWorkspace{cfg: &config.Config{
		Options: &config.Options{TUI: &config.TUIOptions{}},
	}}
	u.dialog = dialog.NewOverlay()
	return u
}

func TestHandlePermissionNotification_RemoteGrantClosesDialog(t *testing.T) {
	t.Parallel()

	u := newTestUIForPermissions()
	perm := permission.PermissionRequest{
		ID:         "perm-1",
		ToolCallID: "tool-call-X",
		ToolName:   "bash",
	}
	u.dialog.OpenDialogWithGrace(dialog.NewPermissions(u.com, perm))
	require.True(t, u.dialog.ContainsDialog(dialog.PermissionsID))

	u.handlePermissionNotification(permission.PermissionNotification{
		ToolCallID: "tool-call-X",
		Granted:    true,
	})

	require.False(t, u.dialog.ContainsDialog(dialog.PermissionsID),
		"granted notification should close matching permissions dialog")
}

func TestHandlePermissionNotification_RemoteDenyClosesDialog(t *testing.T) {
	t.Parallel()

	u := newTestUIForPermissions()
	perm := permission.PermissionRequest{
		ID:         "perm-2",
		ToolCallID: "tool-call-Y",
	}
	u.dialog.OpenDialogWithGrace(dialog.NewPermissions(u.com, perm))

	u.handlePermissionNotification(permission.PermissionNotification{
		ToolCallID: "tool-call-Y",
		Denied:     true,
	})

	require.False(t, u.dialog.ContainsDialog(dialog.PermissionsID),
		"denied notification should close matching permissions dialog")
}

func TestHandlePermissionNotification_InitialPendingDoesNotClose(t *testing.T) {
	t.Parallel()

	u := newTestUIForPermissions()
	perm := permission.PermissionRequest{
		ID:         "perm-3",
		ToolCallID: "tool-call-Z",
	}
	u.dialog.OpenDialogWithGrace(dialog.NewPermissions(u.com, perm))

	// The initial notification published by permission.Request is
	// neither granted nor denied; it must not dismiss the dialog.
	u.handlePermissionNotification(permission.PermissionNotification{
		ToolCallID: "tool-call-Z",
	})

	require.True(t, u.dialog.ContainsDialog(dialog.PermissionsID),
		"initial pending notification must not close the dialog")
}

func TestHandlePermissionNotification_DifferentToolCallIDDoesNotClose(t *testing.T) {
	t.Parallel()

	u := newTestUIForPermissions()
	perm := permission.PermissionRequest{
		ID:         "perm-4",
		ToolCallID: "tool-call-A",
	}
	u.dialog.OpenDialogWithGrace(dialog.NewPermissions(u.com, perm))

	u.handlePermissionNotification(permission.PermissionNotification{
		ToolCallID: "tool-call-B",
		Granted:    true,
	})

	require.True(t, u.dialog.ContainsDialog(dialog.PermissionsID),
		"notification for unrelated tool call must not close the dialog")
}

// TestOpenPermissionsDialog_QueuesSecondRequest pins the multi-park
// case: with several requests parked concurrently, a second request
// must queue behind the displayed dialog rather than replacing it —
// replacing strands the first request's tool call until the run is
// cancelled.
func TestOpenPermissionsDialog_QueuesSecondRequest(t *testing.T) {
	t.Parallel()

	u := newTestUIForPermissions()
	first := permission.PermissionRequest{ID: "perm-1", ToolCallID: "call-1", ToolName: "bash"}
	second := permission.PermissionRequest{ID: "perm-2", ToolCallID: "call-2", ToolName: "bash"}

	u.openPermissionsDialog(first)
	u.openPermissionsDialog(second)

	d := u.dialog.Dialog(dialog.PermissionsID)
	require.NotNil(t, d)
	pd, ok := d.(*dialog.Permissions)
	require.True(t, ok)
	require.Equal(t, "call-1", pd.ToolCallID(),
		"the first request must stay displayed, not be replaced")
	require.Len(t, u.pendingPermissions, 1)
	require.Equal(t, "perm-2", u.pendingPermissions[0].ID)

	// Replays are idempotent: the displayed request's re-publish is a
	// no-op, and a re-published queued request does not enqueue twice.
	u.openPermissionsDialog(first)
	u.openPermissionsDialog(second)
	require.Len(t, u.pendingPermissions, 1)

	// Resolving the displayed request surfaces the queued one.
	u.handlePermissionNotification(permission.PermissionNotification{
		ToolCallID: "call-1",
		Granted:    true,
	})
	d = u.dialog.Dialog(dialog.PermissionsID)
	require.NotNil(t, d, "the queued request must surface after the first resolves")
	pd, ok = d.(*dialog.Permissions)
	require.True(t, ok)
	require.Equal(t, "call-2", pd.ToolCallID())
	require.Empty(t, u.pendingPermissions)
}

// TestHandlePermissionNotification_RemoteResolveDropsQueue covers a
// queued request resolved by another client: it must be dropped from
// the queue so it never surfaces, and it must not pop the next entry.
func TestHandlePermissionNotification_RemoteResolveDropsQueue(t *testing.T) {
	t.Parallel()

	u := newTestUIForPermissions()
	first := permission.PermissionRequest{ID: "perm-1", ToolCallID: "call-1", ToolName: "bash"}
	second := permission.PermissionRequest{ID: "perm-2", ToolCallID: "call-2", ToolName: "bash"}
	third := permission.PermissionRequest{ID: "perm-3", ToolCallID: "call-3", ToolName: "bash"}

	u.openPermissionsDialog(first)
	u.openPermissionsDialog(second)
	u.openPermissionsDialog(third)
	require.Len(t, u.pendingPermissions, 2)

	// The queued request resolves remotely: drop it, keep the rest
	// queued, and leave the displayed dialog alone.
	u.handlePermissionNotification(permission.PermissionNotification{
		ToolCallID: "call-2",
		Granted:    true,
	})
	require.Len(t, u.pendingPermissions, 1)
	require.Equal(t, "perm-3", u.pendingPermissions[0].ID)
	d := u.dialog.Dialog(dialog.PermissionsID)
	require.NotNil(t, d)
	pd, ok := d.(*dialog.Permissions)
	require.True(t, ok)
	require.Equal(t, "call-1", pd.ToolCallID())
}
