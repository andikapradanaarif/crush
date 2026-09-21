package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const seedPlanText = PlanStartMarker + `

# Plan

Do the things.

## Critical Files

- foo.go

` + "```crush-plan-items\n" + `[
  {"key":"parse","content":"Add the parser","active_form":"Adding the parser","evidence_paths":["internal/session/planseed.go"]},
  {"key":"wire","content":"Wire the handoff","depends_on":["parse"],"evidence_paths":["internal/ui/model/ui.go"],"evidence_checks":["unit-tests"]}
]
` + "```\n\n" + PlanReadyMarker

func TestParsePlanSeed(t *testing.T) {
	t.Parallel()

	t.Run("extracts typed items with evidence and deps", func(t *testing.T) {
		t.Parallel()
		items, err := ParsePlanSeed(seedPlanText)
		require.NoError(t, err)
		require.Len(t, items, 2)

		require.Equal(t, "parse", items[0].Key)
		require.Equal(t, "Add the parser", items[0].Content)
		require.Equal(t, PlanItemPending, items[0].Status)
		require.Equal(t, MintPlanItemID("parse", ""), items[0].ID)
		require.Equal(t, []string{"internal/session/planseed.go"}, items[0].EvidencePaths)

		require.Equal(t, "wire", items[1].Key)
		require.Equal(t, []string{MintPlanItemID("parse", "")}, items[1].DependsOn,
			"depends_on keys resolve to minted IDs")
	})

	t.Run("no block returns nil without error", func(t *testing.T) {
		t.Parallel()
		items, err := ParsePlanSeed(PlanStartMarker + "\nplan\n" + PlanReadyMarker)
		require.NoError(t, err)
		require.Nil(t, items)
	})

	t.Run("malformed block errors", func(t *testing.T) {
		t.Parallel()
		_, err := ParsePlanSeed("```crush-plan-items\n[{oops]\n```")
		require.Error(t, err)
	})

	t.Run("items without content or with duplicate keys are skipped", func(t *testing.T) {
		t.Parallel()
		items, err := ParsePlanSeed("```crush-plan-items\n" +
			`[{"key":"a","content":""},{"key":"a","content":"first"},{"key":"a","content":"dup"},{"content":"keyless"}]` +
			"\n```")
		require.NoError(t, err)
		require.Len(t, items, 2)
		require.Equal(t, "first", items[0].Content)
		require.Equal(t, "keyless", items[1].Content)
	})

	t.Run("deps on unknown or dropped keys are filtered", func(t *testing.T) {
		t.Parallel()
		items, err := ParsePlanSeed("```crush-plan-items\n" +
			`[{"key":"a","content":"A","depends_on":["ghost","a"],"evidence_paths":["x.go"]}]` +
			"\n```")
		require.NoError(t, err)
		require.Len(t, items, 1)
		require.Empty(t, items[0].DependsOn)
	})

	t.Run("unclosed block is ignored", func(t *testing.T) {
		t.Parallel()
		items, err := ParsePlanSeed("plan\n```crush-plan-items\n[{}]")
		require.NoError(t, err)
		require.Nil(t, items)
	})
}

func TestMergePlanSeed(t *testing.T) {
	t.Parallel()

	t.Run("surviving keyed IDs keep their status", func(t *testing.T) {
		t.Parallel()
		done := PlanItem{
			ID:     MintPlanItemID("parse", "Add the parser"),
			Key:    "parse",
			Status: PlanItemCompleted,
		}
		seeds, err := ParsePlanSeed(seedPlanText)
		require.NoError(t, err)
		// Reworded content under the same key mints the same ID.
		seeds[0].Content = "Add the items parser"
		seeds[0].ID = MintPlanItemID("parse", seeds[0].Content)

		merged := MergePlanSeed([]PlanItem{done, {ID: "gone", Status: PlanItemInProgress}}, seeds)
		require.Len(t, merged, 2)
		require.Equal(t, PlanItemCompleted, merged[0].Status,
			"a re-approved plan must not resurrect completed work as pending")
		require.Equal(t, "Add the items parser", merged[0].Content)
		require.Equal(t, PlanItemPending, merged[1].Status)
	})

	t.Run("reworded keyless items re-mint and drop the stale row", func(t *testing.T) {
		t.Parallel()
		old := PlanItem{
			ID:      MintPlanItemID("", "old wording"),
			Content: "old wording",
			Status:  PlanItemInProgress,
		}
		seeds := []PlanItem{{
			ID:      MintPlanItemID("", "new wording"),
			Content: "new wording",
			Status:  PlanItemPending,
		}}
		merged := MergePlanSeed([]PlanItem{old}, seeds)
		require.Len(t, merged, 1)
		require.Equal(t, "new wording", merged[0].Content)
		require.Equal(t, PlanItemPending, merged[0].Status)
	})
}

func TestStripPlanItems(t *testing.T) {
	t.Parallel()

	stripped := StripPlanItems(seedPlanText)
	require.NotContains(t, stripped, "crush-plan-items")
	require.NotContains(t, stripped, `"key"`)
	require.Contains(t, stripped, "# Plan")
	require.Contains(t, stripped, PlanReadyMarker, "markers are stripped elsewhere")

	require.Equal(t, "no block", StripPlanItems("no block"))
}

func TestPlanItemsBound(t *testing.T) {
	t.Parallel()

	require.False(t, PlanItemsBound(nil))
	require.False(t, PlanItemsBound([]PlanItem{{Content: "bare"}}))
	require.False(t, PlanItemsBound([]PlanItem{
		{Content: "bound", EvidencePaths: []string{"x.go"}},
		{Content: "bare"},
	}))
	require.True(t, PlanItemsBound([]PlanItem{{Content: "bound", EvidenceChecks: []string{"unit-tests"}}}))
}
