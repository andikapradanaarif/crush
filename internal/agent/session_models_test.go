package agent

import (
	"encoding/json"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// sessionModelTestEnv wires the minimum a coordinator's model-override
// path touches: a real session service for persistence and a config
// carrying one offline-buildable provider.
type sessionModelTestEnv struct {
	coord    *coordinator
	sessions session.Service
}

func newSessionModelTestEnv(t *testing.T) *sessionModelTestEnv {
	t.Helper()
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })

	sessions := session.NewService(db.New(conn), conn)
	cfg, err := config.Init(t.Context(), t.TempDir(), "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set("test-openai-compat", config.ProviderConfig{
		ID:      "test-openai-compat",
		Name:    "Test",
		Type:    openaicompat.Name,
		BaseURL: "http://127.0.0.1:0/v1",
		APIKey:  "test",
		Models:  []catwalk.Model{{ID: "override-model"}},
	})

	return &sessionModelTestEnv{
		coord: &coordinator{
			cfg:            cfg,
			sessions:       sessions,
			modelOverrides: csync.NewMap[string, Model](),
		},
		sessions: sessions,
	}
}

// TestSessionModels_OverrideResolutionOrder pins the resolution chain:
// a session override replaces the shared Large selection while Small
// stays shared, other sessions see the shared set, and clearing the
// override restores the shared selection.
func TestSessionModels_OverrideResolutionOrder(t *testing.T) {
	t.Parallel()
	env := newSessionModelTestEnv(t)
	ctx := t.Context()

	sess, err := env.sessions.Create(ctx, "override target")
	require.NoError(t, err)

	sharedLarge := Model{ModelCfg: config.SelectedModel{Provider: "shared", Model: "shared-large"}}
	sharedSmall := Model{ModelCfg: config.SelectedModel{Provider: "shared", Model: "shared-small"}}
	resolve := env.coord.sessionModels(func() ModelSet {
		return ModelSet{Large: sharedLarge, Small: sharedSmall}
	}, false)

	// No override: the session resolves the shared set.
	got := resolve(sess.ID)
	require.Equal(t, "shared-large", got.Large.ModelCfg.Model)
	require.Equal(t, "shared-small", got.Small.ModelCfg.Model)

	sel := config.SelectedModel{Provider: "test-openai-compat", Model: "override-model"}
	require.NoError(t, env.coord.SetSessionModelOverride(ctx, sess.ID, sel))

	got = resolve(sess.ID)
	require.Equal(t, "override-model", got.Large.ModelCfg.Model,
		"the session override must win the large slot")
	require.Equal(t, "shared-small", got.Small.ModelCfg.Model,
		"the small slot stays shared under an override")

	got = resolve("other-session")
	require.Equal(t, "shared-large", got.Large.ModelCfg.Model,
		"another session still resolves the shared selection")

	env.coord.ClearSessionModelOverride(sess.ID)
	got = resolve(sess.ID)
	require.Equal(t, "shared-large", got.Large.ModelCfg.Model,
		"clearing the override restores the shared selection")
}

// TestSessionModelOverride_RehydratesFromSessionRow covers the restart
// path: an override persisted by a previous process is rebuilt lazily
// on first resolution — no startup scan, no API call needed.
func TestSessionModelOverride_RehydratesFromSessionRow(t *testing.T) {
	t.Parallel()
	env := newSessionModelTestEnv(t)
	ctx := t.Context()

	sess, err := env.sessions.Create(ctx, "persisted override")
	require.NoError(t, err)

	sel := config.SelectedModel{Provider: "test-openai-compat", Model: "override-model"}
	raw, err := json.Marshal(sel)
	require.NoError(t, err)
	_, err = env.sessions.SetModelOverride(ctx, sess.ID, raw)
	require.NoError(t, err)

	// modelOverrides is empty — the restart stand-in — so resolution
	// must reach the session row and build the persisted selection.
	sharedLarge := Model{ModelCfg: config.SelectedModel{Provider: "shared", Model: "shared-large"}}
	resolve := env.coord.sessionModels(func() ModelSet {
		return ModelSet{Large: sharedLarge, Small: sharedLarge}
	}, false)
	got := resolve(sess.ID)
	require.Equal(t, "override-model", got.Large.ModelCfg.Model,
		"a persisted override must apply after a restart")

	// A session whose row carries NULL keeps the shared selection and
	// caches the miss — no repeated store reads.
	sess2, err := env.sessions.Create(ctx, "plain")
	require.NoError(t, err)
	got = resolve(sess2.ID)
	require.Equal(t, "shared-large", got.Large.ModelCfg.Model)
	marker, ok := env.coord.modelOverrides.Get(sess2.ID)
	require.True(t, ok, "the no-override verdict should be cached")
	require.Nil(t, marker.Model)
}
