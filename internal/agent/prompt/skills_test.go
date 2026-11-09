package prompt

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestSkillOverridesPersistAndApplyOnNextBuild(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills", "toggle-fixture")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: toggle-fixture
description: Exercise repository skill overrides.
---
Test instructions.
`), 0o644))
	store := newTestStore(t, dir)
	store.Config().Options.SkillsPaths = []string{filepath.Dir(skillDir)}
	store.Config().Options.DisabledSkills = []string{"toggle-fixture", "jq"}

	dataDir := t.TempDir()
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := session.NewService(db.New(conn), conn)

	p, err := NewPrompt("test", `{{.AvailSkillXML}}`, WithSkillOverrides(
		func(ctx context.Context) []string {
			names, err := sessions.SkillsDisabled(ctx)
			require.NoError(t, err)
			return names
		},
		func(ctx context.Context) []string {
			names, err := sessions.SkillsEnabled(ctx)
			require.NoError(t, err)
			return names
		},
	))
	require.NoError(t, err)
	assertVisible := func(visible bool) {
		t.Helper()
		built, err := p.Build(t.Context(), "prov", "model", store)
		require.NoError(t, err)
		if visible {
			require.Contains(t, built.Text, "<name>toggle-fixture</name>")
		} else {
			require.NotContains(t, built.Text, "<name>toggle-fixture</name>")
		}
		require.NotContains(t, built.Text, "<name>jq</name>", "unrelated config-disabled skills stay disabled")
	}

	assertVisible(false)
	require.NoError(t, sessions.SetSkillDisabled(t.Context(), "toggle-fixture", false, true))
	assertVisible(true)

	// Reopen the database to prove the override survives a restart.
	require.NoError(t, db.Release(dataDir))
	conn, err = db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions = session.NewService(db.New(conn), conn)
	assertVisible(true)

	require.NoError(t, sessions.SetSkillDisabled(t.Context(), "toggle-fixture", true, true))
	assertVisible(false)
	require.NoError(t, sessions.SetSkillDisabled(t.Context(), "toggle-fixture", false, true))
	assertVisible(true)
}
