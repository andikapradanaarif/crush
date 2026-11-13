package notebook

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// srcTagOf returns the entry's src:* tag in full, or "" when absent.
func srcTagOf(tags []string) string {
	if v := tagValue(tags, srcTagPrefix); v != "" {
		return srcTagPrefix + v
	}
	return ""
}

func provenanceEvents() []EntryInput {
	return []EntryInput{
		{
			EventType: EventFileRead, Title: "Read a.go", Description: "contents",
			ToolCall: &message.ToolCall{
				ID: "tc1", Name: "view", Input: `{"file_path":"a.go"}`, Finished: true,
			},
		},
		{EventType: EventDecision, Title: "Decision", Description: "chose X"},
	}
}

// Provenance is stamped by construction, not by the model: a tool
// event's entry is observed even when the model wrote #src:inferred,
// and an unbound input can never self-certify observed.
func TestAlignGeneratedEntries_SrcTagStampedByConstruction(t *testing.T) {
	t.Parallel()

	events := provenanceEvents()
	text := "### Event 1\n## Read a.go\ncontents #file:a.go #src:inferred\n" +
		"### Event 2\n## Decision\nchose X #src:observed\n" +
		"### Event 9\n## Orphan\nloose section\n"

	entries, _ := alignGeneratedEntries(text, events, "")
	require.Len(t, entries, 3)
	require.Equal(t, TagSrcObserved, srcTagOf(entries[0].Tags))
	require.Equal(t, TagSrcInferred, srcTagOf(entries[1].Tags))
	require.Equal(t, TagSrcInferred, srcTagOf(entries[2].Tags))
	// Exactly one src tag per entry — the spoofed one is stripped.
	for _, e := range entries {
		n := 0
		for _, tag := range e.Tags {
			if tag == TagSrcObserved || tag == TagSrcInferred {
				n++
			}
		}
		require.Equal(t, 1, n, "entry must carry exactly one src tag: %v", e.Tags)
	}
}

// Deterministic constructors all produce observed entries: they emit
// text clipped from real artifacts, never generated claims.
func TestDeterministicEntries_SrcObserved(t *testing.T) {
	t.Parallel()

	fallback := fallbackEntry(provenanceEvents()[0], "")
	require.Equal(t, TagSrcObserved, srcTagOf(fallback.Tags))

	intents := intentEntries([]message.Message{
		{Role: message.User, Parts: []message.ContentPart{
			message.TextContent{Text: "do not touch main"},
		}},
	})
	require.Len(t, intents, 1)
	require.Equal(t, TagSrcObserved, srcTagOf(intents[0].Tags))

	trivial := buildTrivialExplorationEntry([]EntryInput{{
		EventType: EventExploration,
		ToolCall:  &message.ToolCall{ID: "tc2", Name: "ls", Input: `{}`, Finished: true},
	}})
	require.Equal(t, TagSrcObserved, srcTagOf(trivial.Tags))

	plan := buildPlanEntry(EntryInput{
		EventType: EventPlan,
		ToolCall: &message.ToolCall{
			ID: "tc3", Name: "todos", Finished: true,
			Input: `{"todos":[{"content":"x","status":"pending"}]}`,
		},
	}, "")
	require.Equal(t, TagSrcObserved, srcTagOf(plan.Tags))
}

// Nil-model consolidation paths write deterministic truncations of
// real input — observed, not synthesis.
func TestNilModelConsolidations_SrcObserved(t *testing.T) {
	t.Parallel()

	g := &llmGenerator{resolveModel: func() fantasy.LanguageModel { return nil }}

	cp, err := g.GenerateCheckpoint(context.Background(), "s", "input")
	require.NoError(t, err)
	require.Equal(t, TagSrcObserved, srcTagOf(cp.Tags))

	dg, err := g.GenerateDigest(context.Background(), "s", "input")
	require.NoError(t, err)
	require.Equal(t, TagSrcObserved, srcTagOf(dg.Tags))
}

// A seed is foreign synthesis regardless of the origin entry's own
// provenance — an observed memory hydrates as inferred.
func TestBuildSeedEntry_SrcInferred(t *testing.T) {
	t.Parallel()

	seed := buildSeedEntry(memoryItem{
		Title:     "Checkpoint",
		Body:      "established fact",
		SessionID: "origin-session",
		EventType: EventCheckpoint,
		Tags:      []string{TagSrcObserved, "file:a.go"},
	})
	require.Equal(t, TagSrcInferred, srcTagOf(seed.Tags))
	require.Contains(t, seed.Tags, TagHydrated)
	require.NotContains(t, seed.Tags, TagSrcObserved)
}

// The render gate: inferred entries render only when they cite a
// resolvable artifact; observed and legacy tool-bound entries are
// unaffected.
func TestRenderEntries_DropsUnsupportedInferred(t *testing.T) {
	t.Parallel()

	entries := []Entry{
		{
			Title: "observed", EventType: EventFileEdit,
			EntryText: "## Edit\ntext KEEP_OBSERVED", Tags: []string{TagSrcObserved},
		},
		{
			Title: "inferred bare", EventType: EventCheckpoint,
			EntryText: "## Checkpoint\nclaims DROP_BARE", Tags: []string{TagSrcInferred},
		},
		{
			Title: "inferred tag handle", EventType: EventCheckpoint,
			EntryText: "## Checkpoint\nclaims KEEP_TAG", Tags: []string{TagSrcInferred, "file:a.go"},
		},
		{
			Title: "inferred body handle", EventType: EventDecision,
			EntryText: "## Decision\nKEEP_BODY — evidence: file:a.go", Tags: []string{TagSrcInferred},
		},
		{
			Title: "inferred origin", EventType: EventCheckpoint,
			EntryText: "## Checkpoint\nKEEP_ORIGIN", Tags: []string{TagSrcInferred, "origin:s1"},
		},
		{
			Title: "legacy checkpoint", EventType: EventCheckpoint,
			EntryText: "## Checkpoint\nclaims DROP_LEGACY",
		},
		{
			Title: "legacy tool", EventType: EventFileRead,
			EntryText: "## Read\ntext KEEP_LEGACY_TOOL",
		},
		{
			Title: "legacy decision handle", EventType: EventDecision,
			EntryText: "## Decision\nKEEP_LEGACY_HANDLE result:tc1 used",
		},
		{
			Title: "legacy decision bare", EventType: EventDecision,
			EntryText: "## Decision\nDROP_LEGACY_DECISION unverified claim",
		},
	}

	rendered := RenderEntries(entries)
	for _, want := range []string{
		"KEEP_OBSERVED", "KEEP_TAG", "KEEP_BODY", "KEEP_ORIGIN",
		"KEEP_LEGACY_TOOL", "KEEP_LEGACY_HANDLE",
	} {
		require.Contains(t, rendered, want)
	}
	for _, drop := range []string{
		"DROP_BARE", "DROP_LEGACY", "DROP_LEGACY_DECISION",
	} {
		require.NotContains(t, rendered, drop)
	}
}

// Compressed inferred entries keep their support through the tag
// list — compression preserves tags, so a compacted checkpoint with
// file: tags still cites artifacts.
func TestRenderEntries_CompressedInferredKeepsHandles(t *testing.T) {
	t.Parallel()

	entries := []Entry{
		{
			Title: "compressed checkpoint", EventType: EventCheckpoint,
			EntryText:        "## Checkpoint\nEstablished claims without inline handles",
			CompressionLevel: CompressionTagsOnly,
			Tags:             []string{TagSrcInferred, "file:a.go"},
		},
	}
	require.NotEmpty(t, RenderEntries(entries))
	require.Contains(t, RenderEntries(entries), "file:a.go")
}

func TestUnsupportedInference(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		entry Entry
		want  bool
	}{
		{"observed no handles", Entry{
			EventType: EventFileEdit, Tags: []string{TagSrcObserved},
		}, false},
		{"inferred no handles", Entry{
			EventType: EventCheckpoint, Tags: []string{TagSrcInferred},
		}, true},
		{"inferred file tag", Entry{
			EventType: EventCheckpoint, Tags: []string{TagSrcInferred, "file:a.go"},
		}, false},
		{"inferred msg tag", Entry{
			EventType: EventDecision, Tags: []string{TagSrcInferred, "msg:m1"},
		}, false},
		{"inferred result in body", Entry{
			EventType: EventDecision, Tags: []string{TagSrcInferred},
			EntryText: "## Decision\nchose result:tc1",
		}, false},
		{"inferred provenance body", Entry{
			EventType: EventGeneral, Tags: []string{TagSrcInferred},
			EntryText: "Seeded from an earlier session. origin-result:tc9 points there.",
		}, false},
		{"inferred file only in full text", Entry{
			EventType: EventCheckpoint, Tags: []string{TagSrcInferred},
			EntryText: "## Checkpoint\nclaims", EntryTextFull: "cites file:a.go",
		}, false},
		{"legacy checkpoint", Entry{EventType: EventCheckpoint}, true},
		{"legacy decision", Entry{EventType: EventDecision}, true},
		{"legacy tool entry", Entry{EventType: EventCommand}, false},
		{"legacy plan", Entry{EventType: EventPlan}, false},
		{"legacy intent", Entry{EventType: EventUserIntent}, false},
		{"unsupported word boundary", Entry{
			EventType: EventDecision, Tags: []string{TagSrcInferred},
			EntryText: "the profile:auth mention is not a handle",
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, UnsupportedInference(tc.entry))
		})
	}
}
