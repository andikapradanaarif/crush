package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/notebook"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// hydrateTestConfig builds a ConfigStore with the mem0 server
// declared in MCP config — the gate maybeHydrateNotebook checks.
// No t.Parallel(): it isolates HOME via env vars.
func hydrateTestConfig(t *testing.T) *config.ConfigStore {
	t.Helper()
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))
	t.Setenv("CRUSH_GLOBAL_CONFIG", filepath.Join(isolated, ".config", "crush"))
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(isolated, ".local", "share", "crush"))
	store, err := config.Load(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)
	store.Config().MCP = map[string]config.MCPConfig{"mem0": {}}
	return store
}

// newHydrationTestAgent extends the segment fixture with hydration
// wiring: option on, memory server configured, fetch stubbed.
func newHydrationTestAgent(t *testing.T, fetch func(ctx context.Context, cfg *config.ConfigStore, serverName string) ([]map[string]any, error)) (*sessionAgent, session.Service, notebook.Service, session.Session) {
	t.Helper()
	a, _, nb, sessionID := newSegmentTestAgent(t, &countingGen{})
	a.notebookHydration = true
	a.notebookMemoryServer = "mem0"
	a.configStore = hydrateTestConfig(t)
	a.hydrateFetch = fetch
	sess, err := a.sessions.Get(context.Background(), sessionID)
	require.NoError(t, err)
	return a, a.sessions, nb, sess
}

func TestMaybeHydrate_FetchFailureLeavesNoMarker(t *testing.T) {
	a, _, nb, sess := newHydrationTestAgent(t, func(context.Context, *config.ConfigStore, string) ([]map[string]any, error) {
		return nil, fmt.Errorf("server unreachable")
	})
	ctx := context.Background()

	a.maybeHydrateNotebook(ctx, sess)

	// The attempt counts (bounded retries are intentional), but NO
	// marker commits — not even a plan-only seed. Next turn retries.
	attempts, err := nb.SessionCounter(ctx, sess.ID, notebook.CounterHydrationAttempts)
	require.NoError(t, err)
	require.Equal(t, int64(1), attempts)
	entries, err := nb.GetEntries(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, entries, "a failed fetch must not commit the hydrated marker")
}

func TestMaybeHydrate_AttemptCapBoundsRetries(t *testing.T) {
	a, _, nb, sess := newHydrationTestAgent(t, func(context.Context, *config.ConfigStore, string) ([]map[string]any, error) {
		return nil, fmt.Errorf("server unreachable")
	})
	ctx := context.Background()
	require.NoError(t, nb.BumpSessionCounter(ctx, sess.ID, notebook.CounterHydrationAttempts, maxHydrationAttempts))

	a.maybeHydrateNotebook(ctx, sess)

	attempts, err := nb.SessionCounter(ctx, sess.ID, notebook.CounterHydrationAttempts)
	require.NoError(t, err)
	require.Equal(t, int64(maxHydrationAttempts), attempts, "capped sessions stop retrying")
}

func TestMaybeHydrate_SeedsMemoryAndLocalPlan(t *testing.T) {
	a, sessions, nb, sess := newHydrationTestAgent(t, func(context.Context, *config.ConfigStore, string) ([]map[string]any, error) {
		return []map[string]any{{
			"memory": "## Checkpoint\nestablished prior knowledge",
			"metadata": map[string]any{
				"working_dir":  "/repo/a",
				"session_id":   "s0",
				"event_type":   notebook.EventCheckpoint,
				"turn_number":  float64(4),
				"event_number": float64(2),
				"tags":         []any{"granularity:session"},
				"compression":  float64(0),
			},
		}}, nil
	})
	ctx := context.Background()

	// A prior top-level session with an open item — and a child
	// session whose open todos must NOT seed the agenda.
	prior, err := sessions.Create(ctx, "prior work")
	require.NoError(t, err)
	prior.Todos = []session.PlanItem{{Content: "finish the refactor", Status: session.PlanItemPending}}
	_, err = sessions.Save(ctx, prior)
	require.NoError(t, err)
	child, err := sessions.CreateTaskSession(ctx, "tc-1", prior.ID, "child task")
	require.NoError(t, err)
	child.Todos = []session.PlanItem{{Content: "child-only todo", Status: session.PlanItemPending}}
	_, err = sessions.Save(ctx, child)
	require.NoError(t, err)

	a.maybeHydrateNotebook(ctx, sess)

	entries, err := nb.GetEntries(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	var planEntry, memEntry *notebook.Entry
	for i := range entries {
		switch entries[i].EventType {
		case notebook.EventPlan:
			planEntry = &entries[i]
		case notebook.EventCheckpoint:
			memEntry = &entries[i]
		}
		require.Equal(t, notebook.HydrationTurnNumber, entries[i].TurnNumber)
		require.Contains(t, entries[i].Tags, notebook.TagHydrated)
	}
	require.NotNil(t, memEntry)
	require.Contains(t, memEntry.EntryText, "established prior knowledge")
	require.NotNil(t, planEntry)
	require.Contains(t, planEntry.EntryText, "finish the refactor")
	require.NotContains(t, planEntry.EntryText, "child-only todo")

	// Second invocation: marker present → no re-fetch, no dup.
	called := false
	a.hydrateFetch = func(context.Context, *config.ConfigStore, string) ([]map[string]any, error) {
		called = true
		return nil, nil
	}
	a.maybeHydrateNotebook(ctx, sess)
	require.False(t, called, "seeded sessions must not fetch again")
	entries, err = nb.GetEntries(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, entries, 2)
}

func TestMaybeHydrate_SkipsSubAgentSession(t *testing.T) {
	a, _, nb, sess := newHydrationTestAgent(t, func(context.Context, *config.ConfigStore, string) ([]map[string]any, error) {
		return []map[string]any{{"memory": "x", "metadata": map[string]any{"working_dir": "/repo/a"}}}, nil
	})
	sess.ParentSessionID = "parent-1"
	a.maybeHydrateNotebook(context.Background(), sess)
	entries, err := nb.GetEntries(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, entries, "sub-agent sessions never hydrate")
}

func TestMaybeHydrate_DisabledOption(t *testing.T) {
	a, _, nb, sess := newHydrationTestAgent(t, func(context.Context, *config.ConfigStore, string) ([]map[string]any, error) {
		return []map[string]any{{"memory": "x", "metadata": map[string]any{"working_dir": "/repo/a"}}}, nil
	})
	a.notebookHydration = false
	a.maybeHydrateNotebook(context.Background(), sess)
	entries, err := nb.GetEntries(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestMaybeHydrate_EmptyFetchStillCountsAnAttempt(t *testing.T) {
	a, _, nb, sess := newHydrationTestAgent(t, func(context.Context, *config.ConfigStore, string) ([]map[string]any, error) {
		return nil, nil // healthy server, verifiably empty store
	})
	ctx := context.Background()

	a.maybeHydrateNotebook(ctx, sess)

	// No marker commits on an empty result, but the fetch counts —
	// otherwise a healthy-but-empty store re-fetches every turn for
	// the life of the session, unbounded.
	attempts, err := nb.SessionCounter(ctx, sess.ID, notebook.CounterHydrationAttempts)
	require.NoError(t, err)
	require.Equal(t, int64(1), attempts)
	entries, err := nb.GetEntries(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, entries)
}
