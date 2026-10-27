package session

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestEstimatedUsageStateSurvivesFetchModifySave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.PromptTokens = 100
	created.CompletionTokens = 50
	created.EstimatedUsage = true

	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.True(t, saved.EstimatedUsage)

	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, fetched.EstimatedUsage)

	fetched.Todos = []PlanItem{{
		Content:    "Check estimate state",
		Status:     PlanItemInProgress,
		ActiveForm: "Checking estimate state",
	}}

	updated, err := sessions.Save(t.Context(), fetched)
	require.NoError(t, err)
	require.True(t, updated.EstimatedUsage)

	refetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, refetched.EstimatedUsage)
}

// TestSessionOverridesPersistAcrossServiceRestart covers both new
// session-owned settings: permission_skip (tri-state) and
// model_override (JSON). A fresh service instance on the same
// database must read back what the previous instance wrote — that is
// the restart path the lazy rehydration in permission/agent relies on.
func TestSessionOverridesPersistAcrossServiceRestart(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)
	ctx := t.Context()

	created, err := sessions.Create(ctx, "overrides")
	require.NoError(t, err)
	require.Nil(t, created.PermissionSkip)
	require.Nil(t, created.ModelOverride)

	// Explicit off and an override selection round-trip.
	raw, err := json.Marshal(map[string]string{"provider": "openai", "model": "gpt-4"})
	require.NoError(t, err)
	off := false
	updated, err := sessions.SetPermissionSkip(ctx, created.ID, &off)
	require.NoError(t, err)
	require.NotNil(t, updated.PermissionSkip)
	require.False(t, *updated.PermissionSkip)
	updated, err = sessions.SetModelOverride(ctx, created.ID, raw)
	require.NoError(t, err)
	require.JSONEq(t, string(raw), string(updated.ModelOverride))

	// A new service instance — the process-restart stand-in — reads
	// the persisted columns without the writer's in-memory state.
	restarted := NewService(db.New(conn), conn)
	fetched, err := restarted.Get(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, fetched.PermissionSkip)
	require.False(t, *fetched.PermissionSkip)
	require.JSONEq(t, string(raw), string(fetched.ModelOverride))

	// Explicit on round-trips too (0/1 are both distinct from NULL).
	on := true
	_, err = sessions.SetPermissionSkip(ctx, created.ID, &on)
	require.NoError(t, err)
	fetched, err = restarted.Get(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, fetched.PermissionSkip)
	require.True(t, *fetched.PermissionSkip)

	// Clearing both writes NULL back — inherit mode survives restart.
	_, err = sessions.SetPermissionSkip(ctx, created.ID, nil)
	require.NoError(t, err)
	_, err = sessions.SetModelOverride(ctx, created.ID, nil)
	require.NoError(t, err)
	fetched, err = restarted.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Nil(t, fetched.PermissionSkip)
	require.Nil(t, fetched.ModelOverride)
}

// Legacy rows carry no ids — the shim mints them deterministically on
// read so in-flight sessions don't orphan, and every read agrees.
func TestUnmarshalTodosMintsDeterministicIDs(t *testing.T) {
	t.Parallel()

	legacy := `[{"content":"write the fix","status":"in_progress"},{"content":"run tests","status":"pending"}]`
	todos, err := unmarshalTodos(legacy)
	require.NoError(t, err)
	require.Len(t, todos, 2)
	require.Equal(t, MintPlanItemID("", "write the fix"), todos[0].ID)
	require.Equal(t, MintPlanItemID("", "run tests"), todos[1].ID)

	// Deterministic: a second read mints the same ids.
	again, err := unmarshalTodos(legacy)
	require.NoError(t, err)
	require.Equal(t, todos[0].ID, again[0].ID)

	// Duplicate legacy content would collide on the content hash —
	// the ordinal suffix keeps ids unique, including 3+ repeats.
	dups, err := unmarshalTodos(`[{"content":"x","status":"pending"},{"content":"x","status":"pending"},{"content":"x","status":"pending"}]`)
	require.NoError(t, err)
	require.NotEqual(t, dups[0].ID, dups[1].ID)
	require.NotEqual(t, dups[1].ID, dups[2].ID)
	require.NotEqual(t, dups[0].ID, dups[2].ID)

	// A stored row already holding the ordinal form doesn't collide
	// with a freshly minted suffix.
	mixed, err := unmarshalTodos(`[{"id":"` + MintPlanItemID("", "x") + `#1","content":"x","status":"pending"},{"content":"x","status":"pending"},{"content":"x","status":"pending"}]`)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, it := range mixed {
		require.False(t, ids[it.ID], "duplicate minted id %q", it.ID)
		ids[it.ID] = true
	}

	// A stored ID later in the list must not collide with a minted ID
	// earlier in the list — stored IDs are all collected before
	// minting runs.
	storedBase := MintPlanItemID("", "x")
	ordered, err := unmarshalTodos(`[{"content":"x","status":"pending"},{"id":"` + storedBase + `","content":"y","status":"pending"}]`)
	require.NoError(t, err)
	require.Equal(t, storedBase+"#1", ordered[0].ID)
	require.Equal(t, storedBase, ordered[1].ID)

	// Newer rows carry ids and keys — both survive untouched.
	kept, err := unmarshalTodos(`[{"id":"abc","key":"k","content":"y","status":"pending"}]`)
	require.NoError(t, err)
	require.Equal(t, "abc", kept[0].ID)
	require.Equal(t, "k", kept[0].Key)
}

func TestSessionChannelPersists(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "channel")
	require.NoError(t, err)
	updated, err := sessions.SetChannel(t.Context(), created.ID, "signal")
	require.NoError(t, err)
	require.Equal(t, "signal", updated.Channel)

	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "signal", fetched.Channel)
}

func TestMCPServerDisabledRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	disabled, err := sessions.MCPDisabledServers(t.Context())
	require.NoError(t, err)
	require.Empty(t, disabled, "a new repository must default to the config")

	require.NoError(t, sessions.SetMCPServerDisabled(t.Context(), "docker", true))
	require.NoError(t, sessions.SetMCPServerDisabled(t.Context(), "serena", true))
	require.NoError(t, sessions.SetMCPServerDisabled(t.Context(), "docker", true), "disabling twice must be idempotent")

	disabled, err = sessions.MCPDisabledServers(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"docker", "serena"}, disabled)

	require.NoError(t, sessions.SetMCPServerDisabled(t.Context(), "docker", false))
	disabled, err = sessions.MCPDisabledServers(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"serena"}, disabled)

	// Enabling records an enabled override so a config-disabled server
	// stays enabled across restarts; disabling removes it again.
	enabled, err := sessions.MCPServersEnabled(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"docker"}, enabled)

	require.NoError(t, sessions.SetMCPServerDisabled(t.Context(), "docker", true))
	enabled, err = sessions.MCPServersEnabled(t.Context())
	require.NoError(t, err)
	require.Empty(t, enabled)
}

func TestEstimatedUsageStateCanBeClearedByExplicitSave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.PromptTokens = 100
	created.CompletionTokens = 50
	created.EstimatedUsage = true

	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.True(t, saved.EstimatedUsage)

	saved.EstimatedUsage = false
	updated, err := sessions.Save(t.Context(), saved)
	require.NoError(t, err)
	require.False(t, updated.EstimatedUsage)

	refetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.False(t, refetched.EstimatedUsage)
}
