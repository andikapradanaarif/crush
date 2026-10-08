package eval

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// numberedFile builds n lines named "<pfx>1"…"<pfx>n" — deterministic
// content whose line numbers double as the data the span math reads.
func numberedFile(pfx string, n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%s%d\n", pfx, i)
	}
	return b.String()
}

// replaceOnce applies one old→new substitution, mirroring the edit
// tool's post-edit content.
func replaceOnce(content, old, new string) string {
	return strings.Replace(content, old, new, 1)
}

// editMeta is the result-metadata JSON the edit tools persist.
func editMeta(oldContent, newContent string) string {
	return fmt.Sprintf(`{"additions":1,"removals":1,"old_content":%q,"new_content":%q}`,
		oldContent, newContent)
}

// viewMeta is the result-metadata JSON a view persists — content is
// the fetched slice, so n lines clamps the view's effective range.
func viewMeta(n int) string {
	return fmt.Sprintf(`{"file_path":"f","content":%q}`, strings.Repeat("l\n", n))
}

// writeProbeFixtureDB builds a session exercising every probe branch:
// in-window/overlap/out-of-window/unknown-span/no-prior-edit views,
// write and multiedit spans, bash-carried mutations (redirect-bound
// and unbound), and cross-turn re-opens of edited files.
func writeProbeFixtureDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	conn, err := db.Connect(context.Background(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dir)) })

	insertSession(t, conn, "s1", "")

	aOld := numberedFile("l", 100)
	aNew := replaceOnce(aOld, "l50\nl51", "x50\nx51\nx52") // Span 50-52 → window 40-62.
	eOld := numberedFile("e", 80)
	eNew := replaceOnce(replaceOnce(eOld, "e10", "E10"), "e60", "E60\nE61")

	msgs := []struct {
		id, role, parts string
	}{
		{"m01", "user", `[` + txtPart("t1") + `]`},
		// Read of a path with no mutation — no_prior_edit.
		{"m02", "assistant", `[` + tcPart("c1", "view", `{"file_path":"/w/unseen.go"}`) + `]`},
		{"m03", "tool", `[` + trPart("c1", "x", false, viewMeta(10)) + `]`},
		// The edit whose ±10 window later views classify against.
		{"m04", "assistant", `[` + tcPart("c2", "edit",
			`{"file_path":"/w/a.go","old_string":"l50\nl51","new_string":"x50\nx51\nx52"}`) + `]`},
		{"m05", "tool", `[` + trPart("c2", "ok", false, editMeta(aOld, aNew)) + `]`},
		// Fetched lines 45-54 ⊆ [40,62] — in_window.
		{"m06", "assistant", `[` + tcPart("c3", "view", `{"file_path":"a.go","offset":44,"limit":10}`) + `]`},
		{"m07", "tool", `[` + trPart("c3", "x", false, viewMeta(10)) + `]`},
		// Lines 80-89 disjoint — out_of_window.
		{"m08", "assistant", `[` + tcPart("c4", "view", `{"file_path":"a.go","offset":79,"limit":10}`) + `]`},
		{"m09", "tool", `[` + trPart("c4", "x", false, viewMeta(10)) + `]`},
		// Lines 59-68 intersect [40,62] without containment — overlap.
		{"m10", "assistant", `[` + tcPart("c5", "view", `{"file_path":"/w/a.go","offset":58,"limit":10}`) + `]`},
		{"m11", "tool", `[` + trPart("c5", "x", false, viewMeta(10)) + `]`},
		// Bare view fetches the whole 100-line file — superset → overlap.
		{"m12", "assistant", `[` + tcPart("c6", "view", `{"file_path":"a.go"}`) + `]`},
		{"m13", "tool", `[` + trPart("c6", "x", false, viewMeta(100)) + `]`},
		// A write's span is the whole file — any post-write view is in_window.
		{"m14", "assistant", `[` + tcPart("c7", "write", `{"file_path":"/w/b.go","content":"w1\nw2\nw3"}`) + `]`},
		{"m15", "tool", `[` + trPart("c7", "ok", false, "") + `]`},
		{"m16", "assistant", `[` + tcPart("c8", "view", `{"file_path":"b.go","offset":1,"limit":2}`) + `]`},
		{"m17", "tool", `[` + trPart("c8", "x", false, viewMeta(2)) + `]`},
		// Successful edit with no metadata — the span isn't derivable.
		{"m18", "assistant", `[` + tcPart("c9", "edit", `{"file_path":"/w/c.go","old_string":"a","new_string":"b"}`) + `]`},
		{"m19", "tool", `[` + trPart("c9", "ok", false, "") + `]`},
		{"m20", "assistant", `[` + tcPart("c10", "view", `{"file_path":"c.go"}`) + `]`},
		{"m21", "tool", `[` + trPart("c10", "x", false, viewMeta(5)) + `]`},
		// Another unedited path — no_prior_edit (precedes the bash
		// mutations, so the unbound flag can't reach it).
		{"m22", "assistant", `[` + tcPart("c11", "view", `{"file_path":"/w/d.go"}`) + `]`},
		{"m22b", "tool", `[` + trPart("c11", "x", false, viewMeta(10)) + `]`},
		// A bash redirect mutation binds its target — the path is
		// known but the written range isn't, so later views of it
		// are span_unknown.
		{"m23", "assistant", `[` + tcPart("c19", "bash", `{"command":"echo done > b2.go"}`) + `]`},
		{"m24", "tool", `[` + trPart("c19", "", false, "") + `]`},
		{"m25", "assistant", `[` + tcPart("c20", "view", `{"file_path":"b2.go"}`) + `]`},
		{"m26", "tool", `[` + trPart("c20", "x", false, viewMeta(3)) + `]`},
		// sed -i mutates but binds no path — an unbound mutation;
		// a later no-site view can't be proven pre-edit.
		{"m27", "assistant", `[` + tcPart("c21", "bash", `{"command":"sed -i 's/x/y/' z.go"}`) + `]`},
		{"m28", "tool", `[` + trPart("c21", "", false, "") + `]`},
		{"m29", "assistant", `[` + tcPart("c22", "view", `{"file_path":"/w/unseen2.go"}`) + `]`},
		{"m30", "tool", `[` + trPart("c22", "x", false, viewMeta(10)) + `]`},
		// Multiedit: two ops → windows [1,20] and [50,71].
		{"m31", "assistant", `[` + tcPart("c15", "multiedit",
			`{"file_path":"/w/e.go","edits":[{"old_string":"e10","new_string":"E10"},{"old_string":"e60","new_string":"E60\nE61"}]}`) + `]`},
		{"m32", "tool", `[` + trPart("c15", "ok", false, editMeta(eOld, eNew)) + `]`},
		{"m33", "assistant", `[` + tcPart("c16", "view", `{"file_path":"e.go","offset":9,"limit":5}`) + `]`},
		{"m34", "tool", `[` + trPart("c16", "x", false, viewMeta(5)) + `]`},
		// Turn 1 — re-opens of files mutated in turn 0.
		{"m35", "user", `[` + txtPart("t2") + `]`},
		{"m36", "assistant", `[` + tcPart("c12", "view", `{"file_path":"a.go","offset":50,"limit":5}`) + `]`},
		{"m37", "tool", `[` + trPart("c12", "x", false, viewMeta(5)) + `]`},
		{"m38", "assistant", `[` + tcPart("c13", "view", `{"file_path":"b.go","offset":0,"limit":3}`) + `]`},
		{"m39", "tool", `[` + trPart("c13", "x", false, viewMeta(3)) + `]`},
		{"m40", "assistant", `[` + tcPart("c17", "view", `{"file_path":"e.go","offset":55,"limit":10}`) + `]`},
		{"m41", "tool", `[` + trPart("c17", "x", false, viewMeta(10)) + `]`},
		// Same file, second view this turn — not a turn-start reread.
		{"m42", "assistant", `[` + tcPart("c18", "view", `{"file_path":"e.go","offset":0,"limit":5}`) + `]`},
		{"m43", "tool", `[` + trPart("c18", "x", false, viewMeta(5)) + `]`},
	}
	for _, m := range msgs {
		insertMsg(t, conn, m.id, "s1", m.role, m.parts, 1000, 0)
	}
	return filepath.Join(dir, "crush.db")
}

func TestProbePostEditWindow(t *testing.T) {
	t.Parallel()
	rep, err := RunProbe(context.Background(), writeProbeFixtureDB(t), "post-edit-window",
		AnalyzeOptions{Workdir: "/w", Turns: []string{"t1", "t2"}})
	require.NoError(t, err)

	// in_window: c3,c8,c12,c13,c16,c17,c18. overlap: c5 (partial) and
	// c6 (superset fetch). out: c4. span_unknown: c10 (no metadata),
	// c20 (bash redirect target — path known, range not), c22 (the
	// unbound sed -i poisons every later no-site view). no_prior_edit:
	// c1,c11 — both precede the bash mutations.
	require.Equal(t, 7, rep.Counts["in_window"])
	require.Equal(t, 2, rep.Counts["overlap"])
	require.Equal(t, 1, rep.Counts["out_of_window"])
	require.Equal(t, 3, rep.Counts["span_unknown"])
	require.Equal(t, 2, rep.Counts["no_prior_edit"])
	require.Equal(t, 1, rep.Counts["unbound_mutations"])
	require.Equal(t, 15, rep.Counts["views"])
	require.Equal(t, 7, rep.Counts["servable"])
}

func TestProbeViewEditSameFile(t *testing.T) {
	t.Parallel()
	rep, err := RunProbe(context.Background(), writeProbeFixtureDB(t), "view-edit-same-file",
		AnalyzeOptions{Workdir: "/w", Turns: []string{"t1", "t2"}})
	require.NoError(t, err)

	// Edited paths: a.go (views c3,c4,c5,c6,c12), b.go (c8,c13),
	// c.go (c10), b2.go (c20 — the bash redirect bound it),
	// e.go (c16,c17,c18). unseen/d.go never mutated; the sed -i
	// mutation is unbound — tallied, not armed.
	require.Equal(t, 5, rep.Counts["edited_files"])
	require.Equal(t, 12, rep.Counts["views_on_edited_files"])
	require.Equal(t, 15, rep.Counts["views_total"])
	require.Equal(t, 1, rep.Counts["unbound_mutations"])
	require.Len(t, rep.Rows, 5)
}

func TestProbeTurnStartReread(t *testing.T) {
	t.Parallel()
	rep, err := RunProbe(context.Background(), writeProbeFixtureDB(t), "turn-start-reread",
		AnalyzeOptions{Workdir: "/w", Turns: []string{"t1", "t2"}})
	require.NoError(t, err)

	// a.go, b.go, e.go each get a first view in turn 1 after being
	// mutated in turn 0 — seqs are positional (the bash and multiedit
	// calls shifted them). c18 is the second e.go view in the same
	// turn and must not count; b2.go was bash-mutated in turn 0 but
	// never re-viewed.
	require.Equal(t, 3, rep.Counts["turn_start_rereads"])
	require.Equal(t, 1, rep.Counts["unbound_mutations"])
	require.Len(t, rep.Rows, 3)
	require.Equal(t, []string{"1", "17", "/w/a.go", "0", "1"}, rep.Rows[0])
	require.Equal(t, []string{"1", "19", "/w/e.go", "0", "3"}, rep.Rows[2])
}

func TestRunProbe_Unknown(t *testing.T) {
	t.Parallel()
	_, err := RunProbe(context.Background(), writeProbeFixtureDB(t), "nope", AnalyzeOptions{})
	require.ErrorContains(t, err, "unknown probe")
	require.ErrorContains(t, err, "post-edit-window")
}
