package notebook

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func alignEvents() []EntryInput {
	return []EntryInput{
		{EventType: EventFileRead, Title: "Read a.go", Description: "a.go contents"},
		{EventType: EventCommand, Title: "Run: go test", Description: "test output"},
		{EventType: EventFileEdit, Title: "Edit b.go", Description: "edit diff"},
	}
}

// The '### Event N' echo is the binding contract: an entry lands on
// its declared input regardless of where it sits in the output.
func TestAlignGeneratedEntries_MarkersBindByID(t *testing.T) {
	t.Parallel()

	events := alignEvents()
	// Out of order on purpose — position would bind wrong.
	text := "### Event 2\n## Run: go test\nok #phase:command\n" +
		"### Event 1\n## Read a.go\ncontents #file:a.go\n" +
		"### Event 3\n## Edit b.go\ndiff #file:b.go\n"

	entries, parsed := alignGeneratedEntries(text, events, "")
	require.Equal(t, 3, parsed)
	require.Len(t, entries, 3)

	require.Equal(t, EventFileRead, entries[0].EventType)
	require.Contains(t, entries[0].Text, "contents")
	require.NotContains(t, entries[0].Text, "Event 1",
		"the marker line is metadata, not entry content")
	require.Equal(t, "Read a.go", entries[0].Title)

	require.Equal(t, EventCommand, entries[1].EventType)
	require.Contains(t, entries[1].Text, "go test")

	require.Equal(t, EventFileEdit, entries[2].EventType)
	require.Contains(t, entries[2].Text, "diff")
}

// A model that merges or skips an input must not shift every later
// entry onto the wrong event — the skipped slot gets the deterministic
// fallback at its own position.
func TestAlignGeneratedEntries_SkippedInputGetsFallbackInPlace(t *testing.T) {
	t.Parallel()

	events := alignEvents()
	// Event 2 produced nothing — merged into event 1's entry, say.
	text := "### Event 1\n## Read a.go\ncontents\n" +
		"### Event 3\n## Edit b.go\ndiff\n"

	entries, parsed := alignGeneratedEntries(text, events, "")
	require.Equal(t, 2, parsed)
	require.Len(t, entries, 3)

	require.Contains(t, entries[0].Text, "contents")
	require.Equal(t, EventCommand, entries[1].EventType)
	require.Contains(t, entries[1].Text, "Run: go test",
		"the unbound middle slot holds its deterministic fallback")
	require.Equal(t, EventFileEdit, entries[2].EventType)
	require.Contains(t, entries[2].Text, "diff",
		"event 3 binds by marker, not shifted position")
}

// A markdown '---' inside a marked entry's body is content, not a
// delimiter — the phantom-split that used to cascade misalignment.
func TestAlignGeneratedEntries_DelimiterInsideBodyIsContent(t *testing.T) {
	t.Parallel()

	events := alignEvents()
	text := "### Event 1\n## Read a.go\nfirst half\n---\nsecond half\n" +
		"### Event 2\n## Run: go test\nok\n" +
		"### Event 3\n## Edit b.go\ndiff\n"

	entries, parsed := alignGeneratedEntries(text, events, "")
	require.Equal(t, 3, parsed)
	require.Contains(t, entries[0].Text, "first half")
	require.Contains(t, entries[0].Text, "second half",
		"a '---' rule inside the body must not split the entry")
	require.Contains(t, entries[1].Text, "go test")
}

// Output with no markers at all keeps the legacy positional '---'
// contract — a model that ignores the echo instruction degrades to
// the old behavior rather than misbinding.
func TestAlignGeneratedEntries_NoMarkersFallsBackToPositional(t *testing.T) {
	t.Parallel()

	events := alignEvents()
	text := "## Read a.go\ncontents\n---\n## Run: go test\nok\n---\n## Edit b.go\ndiff\n"

	entries, parsed := alignGeneratedEntries(text, events, "")
	require.Equal(t, 3, parsed)
	require.Equal(t, EventFileRead, entries[0].EventType)
	require.Equal(t, EventCommand, entries[1].EventType)
	require.Equal(t, EventFileEdit, entries[2].EventType)
}

// Prose before the first marker is preamble, not an entry.
func TestAlignGeneratedEntries_PreambleDropped(t *testing.T) {
	t.Parallel()

	events := alignEvents()[:1]
	text := "Here are the entries you asked for:\n\n" +
		"### Event 1\n## Read a.go\ncontents\n"

	entries, parsed := alignGeneratedEntries(text, events, "")
	require.Equal(t, 1, parsed)
	require.Len(t, entries, 1)
	require.NotContains(t, entries[0].Text, "Here are the entries")
}

// Markers naming nonexistent inputs can't bind — they append as
// extras rather than positional-filling a wrong slot.
func TestAlignGeneratedEntries_OutOfRangeMarkerIsExtra(t *testing.T) {
	t.Parallel()

	events := alignEvents()[:1]
	text := "### Event 1\n## Read a.go\ncontents\n" +
		"### Event 9\n## Ghost\nhallucinated\n"

	entries, parsed := alignGeneratedEntries(text, events, "")
	require.Equal(t, 1, parsed)
	require.Len(t, entries, 2)
	require.Equal(t, EventFileRead, entries[0].EventType)
	require.Equal(t, EventGeneral, entries[1].EventType)
	require.Contains(t, entries[1].Text, "hallucinated")
}

// A duplicated marker keeps the first and treats the echo as an
// extra — it must not overwrite the already-bound slot.
func TestAlignGeneratedEntries_DuplicateMarkerIsExtra(t *testing.T) {
	t.Parallel()

	events := alignEvents()[:1]
	text := "### Event 1\n## Read a.go\ncontents\n" +
		"### Event 1\n## Read a.go again\nduplicate\n"

	entries, _ := alignGeneratedEntries(text, events, "")
	require.Len(t, entries, 2)
	require.Contains(t, entries[0].Text, "contents")
	require.NotContains(t, entries[0].Text, "again")
	require.Contains(t, entries[1].Text, "duplicate")
}

// A section after the last marker with no marker of its own folds
// into the preceding entry — marker mode never re-splits on '---',
// since a horizontal rule in an entry body is indistinguishable from
// a forgotten marker and folding can't misalign a later event.
func TestAlignGeneratedEntries_ForgottenMarkerFoldsForward(t *testing.T) {
	t.Parallel()

	events := alignEvents()
	text := "### Event 1\n## Read a.go\ncontents\n" +
		"## Run: go test\nok\n" +
		"### Event 3\n## Edit b.go\ndiff\n"

	entries, parsed := alignGeneratedEntries(text, events, "")
	require.Equal(t, 2, parsed)
	require.Contains(t, entries[0].Text, "contents")
	require.Contains(t, entries[0].Text, "go test",
		"the unmarked tail stays inside event 1's body")
	require.Equal(t, EventCommand, entries[1].EventType)
	require.Contains(t, entries[1].Text, "Run: go test",
		"event 2 gets the deterministic fallback, not a shifted bind")
	require.Equal(t, EventFileEdit, entries[2].EventType)
}
