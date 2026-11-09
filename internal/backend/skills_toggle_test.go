package backend

import (
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/stretchr/testify/require"
)

func TestSkillTogglesAffectServerCatalog(t *testing.T) {
	t.Parallel()

	b, _ := newTestBackend(t)
	ws, sessions, _ := insertMessageWorkspace(t, b, nil)
	cfg, err := config.Init(t.Context(), t.TempDir(), filepath.Join(t.TempDir(), "data"), false)
	require.NoError(t, err)
	cfg.Config().Options.DisabledSkills = []string{"jq"}
	ws.Cfg = cfg
	all := skills.DiscoverBuiltin()
	ws.Skills = skills.NewManager(all, skills.Filter(all, []string{"jq"}), nil)
	t.Cleanup(ws.Skills.Shutdown)
	id := skills.BuiltinPrefix + "jq/SKILL.md"

	assertVisible := func(visible bool) {
		t.Helper()
		remoteEntries, err := b.ListSkills(t.Context(), ws.ID)
		require.NoError(t, err)
		var remoteNames []string
		for _, entry := range remoteEntries {
			remoteNames = append(remoteNames, entry.Name)
		}
		_, _, remoteErr := b.ReadSkill(t.Context(), ws.ID, id)
		if visible {
			require.Contains(t, remoteNames, "jq")
			require.NoError(t, remoteErr)
		} else {
			require.NotContains(t, remoteNames, "jq")
			require.ErrorIs(t, remoteErr, skills.ErrSkillNotFound)
		}
	}

	assertVisible(false)
	require.NoError(t, b.SetSkillDisabled(t.Context(), ws.ID, "jq", false))
	assertVisible(true)
	require.NoError(t, sessions.SetSkillDisabled(t.Context(), "jq", true, true))
	assertVisible(false)

	// Config reloads must also apply without rebuilding discovery state.
	cfg.Config().Options.DisabledSkills = nil
	require.NoError(t, b.SetSkillDisabled(t.Context(), ws.ID, "jq", false))
	assertVisible(true)
}
