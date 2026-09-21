package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/stretchr/testify/require"
)

func postPlanApprove(t *testing.T, c *controllerV1, wsID, sid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/v1/workspaces/"+wsID+"/agent/sessions/"+sid+"/plan/approve", nil)
	req.SetPathValue("id", wsID)
	req.SetPathValue("sid", sid)
	rec := httptest.NewRecorder()
	c.handlePostWorkspaceAgentSessionPlanApprove(rec, req)
	return rec
}

func TestPostPlanApprove_Success(t *testing.T) {
	t.Parallel()

	coord := newRunCoordinator(func(context.Context) error { return nil })
	c, wsID := buildAgentWorkspace(t, coord)

	rec := postPlanApprove(t, c, wsID, "sess-1")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "sess-1", coord.approvedSession.Load())
}

func TestPostPlanApprove_BusyConflict(t *testing.T) {
	t.Parallel()

	coord := newRunCoordinator(func(context.Context) error { return nil })
	coord.busy = true
	c, wsID := buildAgentWorkspace(t, coord)

	rec := postPlanApprove(t, c, wsID, "sess-1")
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Nil(t, coord.approvedSession.Load(), "busy agent must not approve")
}

func TestPostPlanApprove_NoReadyPlanConflict(t *testing.T) {
	t.Parallel()

	coord := newRunCoordinator(func(context.Context) error { return nil })
	coord.approvePlanErr = agent.ErrNoReadyPlan
	c, wsID := buildAgentWorkspace(t, coord)

	rec := postPlanApprove(t, c, wsID, "sess-1")
	require.Equal(t, http.StatusConflict, rec.Code)
}

func TestPostPlanApprove_WorkspaceNotFound(t *testing.T) {
	t.Parallel()

	c, _ := buildAgentWorkspace(t, newRunCoordinator(func(context.Context) error { return nil }))

	rec := postPlanApprove(t, c, "does-not-exist", "sess-1")
	require.Equal(t, http.StatusNotFound, rec.Code)
}
