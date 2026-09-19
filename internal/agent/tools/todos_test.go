package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func newTodosTestSession(t *testing.T) (session.Service, string) {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := session.NewService(db.New(conn), conn)
	sess, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	return sessions, sess.ID
}

func runTodosTool(t *testing.T, tool fantasy.AgentTool, sessionID string, items []TodoItem) (fantasy.ToolResponse, error) {
	t.Helper()
	params, err := json.Marshal(TodosParams{Todos: items})
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, sessionID)
	return tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: TodosToolName, Input: string(params)})
}

func TestValidatePlanItems(t *testing.T) {
	t.Parallel()
	bindable := map[string]bool{"verify:build": true}
	cases := []struct {
		name    string
		items   []TodoItem
		wantErr string
	}{
		{
			name: "valid list with deps and evidence",
			items: []TodoItem{
				{Content: "a", Status: "pending", Key: "first"},
				{Content: "b", Status: "pending", DependsOn: []string{"first"}, EvidenceChecks: []string{"verify:build"}},
			},
		},
		{
			name: "invalid status",
			items: []TodoItem{
				{Content: "a", Status: "done"},
			},
			wantErr: "invalid status",
		},
		{
			name: "duplicate key",
			items: []TodoItem{
				{Content: "a", Status: "pending", Key: "x"},
				{Content: "b", Status: "pending", Key: "x"},
			},
			wantErr: "duplicate key",
		},
		{
			name: "identical content",
			items: []TodoItem{
				{Content: "same", Status: "pending", Key: "a"},
				{Content: "same", Status: "pending", Key: "b"},
			},
			wantErr: "identical content",
		},
		{
			name: "depends on unknown key",
			items: []TodoItem{
				{Content: "a", Status: "pending", DependsOn: []string{"ghost"}},
			},
			wantErr: "unknown key",
		},
		{
			name: "self dependency",
			items: []TodoItem{
				{Content: "a", Status: "pending", Key: "x", DependsOn: []string{"x"}},
			},
			wantErr: "depends on itself",
		},
		{
			name: "dependency cycle",
			items: []TodoItem{
				{Content: "a", Status: "pending", Key: "x", DependsOn: []string{"y"}},
				{Content: "b", Status: "pending", Key: "y", DependsOn: []string{"x"}},
			},
			wantErr: "cycle",
		},
		{
			name: "unbindable check name",
			items: []TodoItem{
				{Content: "a", Status: "pending", EvidenceChecks: []string{"package-test:pkg"}},
			},
			wantErr: "unknown check",
		},
		{
			name: "empty check entry",
			items: []TodoItem{
				{Content: "a", Status: "pending", EvidenceChecks: []string{" "}},
			},
			wantErr: "empty evidence_checks",
		},
		{
			name: "empty path entry",
			items: []TodoItem{
				{Content: "a", Status: "pending", EvidencePaths: []string{""}},
			},
			wantErr: "empty evidence_paths",
		},
		{
			name: "path covering the working directory",
			items: []TodoItem{
				{Content: "a", Status: "pending", EvidencePaths: []string{"."}},
			},
			wantErr: "covers the whole working directory",
		},
		{
			name: "ancestor path covering the working directory",
			items: []TodoItem{
				{Content: "a", Status: "pending", EvidencePaths: []string{".."}},
			},
			wantErr: "covers the whole working directory",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validatePlanItems(tc.items, bindable, t.TempDir())
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestTodosToolMintsDeterministicIDs(t *testing.T) {
	t.Parallel()
	sessions, sessionID := newTodosTestSession(t)
	tool := NewTodosTool(sessions, nil, t.TempDir())

	// First write: keyed dep target, keyless item, depends_on by key.
	resp, err := runTodosTool(t, tool, sessionID, []TodoItem{
		{Content: "set up the harness", Status: "pending", Key: "setup"},
		{Content: "run the checks", Status: "pending", DependsOn: []string{"setup"}},
		{Content: "unkeyed work", Status: "pending"},
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	sess, err := sessions.Get(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, sess.Todos, 3)

	setupID := session.MintPlanItemID("setup", "")
	require.Equal(t, setupID, sess.Todos[0].ID)
	require.Equal(t, []string{setupID}, sess.Todos[1].DependsOn)
	require.Equal(t, session.MintPlanItemID("", "unkeyed work"), sess.Todos[2].ID)

	// Rewrite: keyed item reworded keeps its ID (key match); the
	// depends_on ref stays valid. Unkeyed item reworded re-mints.
	resp, err = runTodosTool(t, tool, sessionID, []TodoItem{
		{Content: "set up the harness properly", Status: "pending", Key: "setup"},
		{Content: "run the checks", Status: "pending", DependsOn: []string{"setup"}},
		{Content: "unkeyed work reworded", Status: "pending"},
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	sess, err = sessions.Get(t.Context(), sessionID)
	require.NoError(t, err)
	require.Equal(t, setupID, sess.Todos[0].ID, "kept key must preserve the ID across rewording")
	require.Equal(t, []string{setupID}, sess.Todos[1].DependsOn)
	require.NotEqual(t, session.MintPlanItemID("", "unkeyed work"), sess.Todos[2].ID)
}

func TestTodosToolDescriptionSurfacesCheckNames(t *testing.T) {
	t.Parallel()
	sessions, _ := newTodosTestSession(t)
	tool := NewTodosTool(sessions, []string{"verify:build", "verify:lint"}, t.TempDir())
	require.Contains(t, tool.Info().Description, "verify:build")
	require.Contains(t, tool.Info().Description, "verify:lint")

	sessions2, _ := newTodosTestSession(t)
	bare := NewTodosTool(sessions2, nil, t.TempDir())
	require.NotContains(t, bare.Info().Description, "verify:")
}
