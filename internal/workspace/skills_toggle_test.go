package workspace

import (
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/stretchr/testify/require"
)

func TestSkillTogglesAffectLocalCatalog(t *testing.T) {
	t.Parallel()

	cfg, err := config.Init(t.Context(), t.TempDir(), filepath.Join(t.TempDir(), "data"), false)
	require.NoError(t, err)
	cfg.Config().Options.DisabledSkills = []string{"jq"}
	dataDir := t.TempDir()
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := session.NewService(db.New(conn), conn)
	all := skills.DiscoverBuiltin()
	mgr := skills.NewManager(all, skills.Filter(all, []string{"jq"}), nil)
	t.Cleanup(mgr.Shutdown)
	w := NewAppWorkspace(&app.App{Sessions: sessions, Skills: mgr}, cfg)
	id := skills.BuiltinPrefix + "jq/SKILL.md"

	entries, err := w.ListSkills(t.Context())
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotEqual(t, "jq", entry.Name)
	}
	_, _, err = w.ReadSkill(t.Context(), id)
	require.ErrorIs(t, err, skills.ErrSkillNotFound)

	require.NoError(t, w.SkillSetDisabled(t.Context(), "jq", false))
	entries, err = w.ListSkills(t.Context())
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	require.Contains(t, names, "jq")
	_, _, err = w.ReadSkill(t.Context(), id)
	require.NoError(t, err)

	require.NoError(t, w.SkillSetDisabled(t.Context(), "jq", true))
	_, _, err = w.ReadSkill(t.Context(), id)
	require.ErrorIs(t, err, skills.ErrSkillNotFound)
}
