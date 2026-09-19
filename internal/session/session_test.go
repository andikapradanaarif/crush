package session

import (
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
