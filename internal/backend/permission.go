package backend

import (
	"context"

	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/pubsub"
)

// GrantPermission grants, denies, or persistently grants a permission
// request. The returned bool reports whether this call resolved the
// pending request (true) or found it already resolved by a previous
// caller (false). A false return is not an error.
func (b *Backend) GrantPermission(workspaceID string, req proto.PermissionGrant) (bool, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return false, err
	}

	perm := permission.PermissionRequest{
		ID:          req.Permission.ID,
		SessionID:   req.Permission.SessionID,
		ToolCallID:  req.Permission.ToolCallID,
		ToolName:    req.Permission.ToolName,
		Description: req.Permission.Description,
		Action:      req.Permission.Action,
		Params:      req.Permission.Params,
		Path:        req.Permission.Path,
	}

	switch req.Action {
	case proto.PermissionAllow:
		return ws.Permissions.Grant(perm), nil
	case proto.PermissionAllowForSession:
		return ws.Permissions.GrantPersistent(perm), nil
	case proto.PermissionDeny:
		return ws.Permissions.Deny(perm), nil
	default:
		return false, ErrInvalidPermissionAction
	}
}

// publishPermissionsSkipChanged broadcasts a permission-skip state
// change on the workspace's event broker so connected clients keep
// their cached flags in sync. An empty sessionID marks the
// workspace-global flag; a non-empty sessionID scopes the event to
// that session's viewers via the server's session-scoped event
// filter.
func publishPermissionsSkipChanged(ws *Workspace, sessionID string, skip bool) {
	if ws == nil || ws.App == nil {
		return
	}

	ws.SendEvent(pubsub.Event[proto.PermissionsSkipChanged]{
		Type: pubsub.UpdatedEvent,
		Payload: proto.PermissionsSkipChanged{
			WorkspaceID: ws.ID,
			SessionID:   sessionID,
			Skip:        skip,
		},
	})
}

// SetPermissionsSkip sets whether permission prompts are skipped.
func (b *Backend) SetPermissionsSkip(workspaceID string, skip bool) error {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return err
	}

	ws.Permissions.SetSkipRequests(skip)
	publishPermissionsSkipChanged(ws, "", skip)
	return nil
}

// GetPermissionsSkip returns whether permission prompts are skipped.
func (b *Backend) GetPermissionsSkip(workspaceID string) (bool, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return false, err
	}

	return ws.Permissions.SkipRequests(), nil
}

// SetSessionPermissionsSkip sets whether permission prompts are
// skipped for a single session, independently of the workspace-global
// flag. One session can run auto-approved while others still prompt.
// A nil skip clears the session's override so it inherits the
// workspace flag again. The setting persists on the session row, so
// it survives workspace process restarts; the in-memory override is
// updated for hot reads.
func (b *Backend) SetSessionPermissionsSkip(ctx context.Context, workspaceID, sessionID string, skip *bool) error {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return err
	}
	// Persist before the in-memory update. No ordering wins both
	// directions: persist-first leaves a sub-millisecond window where
	// a disarm still approves against the stale map value — bounded,
	// self-correcting, and the row stays the source of truth. The
	// reverse order risks the map saying cleared while the row says
	// armed if the persist fails — a divergence that survives until
	// the next write.
	// Serialized: persist + map update must stay ordered against a
	// concurrent call or the map and row diverge until the next write.
	ws.permOverrideMu.Lock()
	defer ws.permOverrideMu.Unlock()
	if _, err := ws.Sessions.SetPermissionSkip(ctx, sessionID, skip); err != nil {
		return err
	}

	if skip == nil {
		ws.Permissions.ClearSessionAutoApprove(sessionID)
	} else {
		ws.Permissions.SetSessionAutoApprove(sessionID, *skip)
	}
	// The event carries the effective flag, not the raw tri-state —
	// consumers toggling prompt behavior need the resolution result,
	// and the raw override ships on the session resource itself.
	publishPermissionsSkipChanged(ws, sessionID, ws.Permissions.SessionAutoApproves(sessionID))
	return nil
}

// GetSessionPendingPermissions returns the permission requests
// currently parked awaiting a decision for the given session. Requests
// raised by sub-agent sessions are included: sessionMatches resolves
// them to the top-level session, so a parent session's list shows every
// prompt its viewers may need to answer.
func (b *Backend) GetSessionPendingPermissions(ctx context.Context, workspaceID, sessionID string) ([]permission.PermissionRequest, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	if _, err := ws.Sessions.Get(ctx, sessionID); err != nil {
		return nil, err
	}
	if ws.Permissions == nil {
		return nil, nil
	}

	var out []permission.PermissionRequest
	for _, req := range ws.Permissions.PendingRequests() {
		if b.sessionMatches(ctx, ws, req.SessionID, sessionID) {
			out = append(out, req)
		}
	}
	return out, nil
}

// GetSessionPermissionsSkip returns whether permission prompts are
// skipped for a single session.
func (b *Backend) GetSessionPermissionsSkip(ctx context.Context, workspaceID, sessionID string) (bool, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return false, err
	}
	if _, err := ws.Sessions.Get(ctx, sessionID); err != nil {
		return false, err
	}

	return ws.Permissions.SessionAutoApproves(sessionID), nil
}
