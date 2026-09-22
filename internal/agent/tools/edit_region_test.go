package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/diff"
	"github.com/stretchr/testify/require"
)

func TestHunkNewSpans(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		diff string
		want []hunkSpan
	}{
		{"simple", "@@ -1,3 +1,4 @@", []hunkSpan{{1, 4}}},
		{"implicit len", "@@ -5 +5 @@", []hunkSpan{{5, 1}}},
		{"pure delete", "@@ -5,3 +4,0 @@", []hunkSpan{{4, 0}}},
		{"insert at start", "@@ -0,0 +1,3 @@", []hunkSpan{{1, 3}}},
		{"two hunks", "@@ -1,2 +1,2 @@\n ctx\n@@ -20,3 +22,5 @@", []hunkSpan{{1, 2}, {22, 5}}},
		{"no hunk", "no headers here", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, hunkNewSpans(tc.diff))
		})
	}
}

func TestPostEditRegion_SingleHunk(t *testing.T) {
	t.Parallel()

	old := strings.Join([]string{"l1", "l2", "l3", "l4", "l5"}, "\n")
	new := strings.Join([]string{"l1", "l2", "CHANGED", "l4", "l5"}, "\n")
	unified, _, _ := diff.GenerateDiff(old, new, "f.txt")

	region := postEditRegion(unified, new)
	require.Contains(t, region, "     3|CHANGED")
	require.Contains(t, region, "     2|l2")
	require.Contains(t, region, "     4|l4")
	require.NotContains(t, region, "truncated")
	// Line numbers match the post-edit file, not the pre-edit one.
	require.NotContains(t, region, "|l3\n")
}

func TestPostEditRegion_ReplaceAllScatteredHunks(t *testing.T) {
	t.Parallel()

	var oldLines, newLines []string
	for i := 1; i <= 40; i++ {
		oldLines = append(oldLines, fmt.Sprintf("line-%02d", i))
		newLines = append(newLines, fmt.Sprintf("line-%02d", i))
	}
	oldLines[2], oldLines[30] = "foo", "foo"
	newLines[2], newLines[30] = "bar", "bar"
	old, new := strings.Join(oldLines, "\n"), strings.Join(newLines, "\n")
	unified, _, _ := diff.GenerateDiff(old, new, "f.txt")

	region := postEditRegion(unified, new)
	// Both changed occurrences appear, in separate windows.
	require.Contains(t, region, "     3|bar")
	require.Contains(t, region, "    31|bar")
	require.Contains(t, region, "     ⋯")
}

func TestPostEditRegion_DeleteJunction(t *testing.T) {
	t.Parallel()

	old := "a\nb\nc\nd\ne"
	new := "a\nb\ne" // deleted c, d
	unified, _, _ := diff.GenerateDiff(old, new, "f.txt")

	region := postEditRegion(unified, new)
	require.NotEmpty(t, region)
	// Junction context: surrounding lines of the post-edit file.
	require.Contains(t, region, "|b")
	require.Contains(t, region, "|e")
	require.NotContains(t, region, "|c")
	require.NotContains(t, region, "|d")
}

func TestPostEditRegion_CapsWholeFileRewrite(t *testing.T) {
	t.Parallel()

	var oldLines, newLines []string
	for i := 0; i < 200; i++ {
		oldLines = append(oldLines, fmt.Sprintf("old-%03d", i))
		newLines = append(newLines, fmt.Sprintf("new-%03d", i))
	}
	old, new := strings.Join(oldLines, "\n"), strings.Join(newLines, "\n")
	unified, _, _ := diff.GenerateDiff(old, new, "f.txt")

	region := postEditRegion(unified, new)
	emitted := strings.Count(region, "|")
	require.LessOrEqual(t, emitted, postEditMaxLines+1) // +1 for the ⋯ separator/marker text
	require.Contains(t, region, "truncated")
}

func TestPostEditRegion_CRLFStripped(t *testing.T) {
	t.Parallel()

	old := "a\nb\nc"
	new := "a\nB\nc"
	unified, _, _ := diff.GenerateDiff(old, new, "f.txt")
	crlfNew := strings.ReplaceAll(new, "\n", "\r\n")

	region := postEditRegion(unified, crlfNew)
	require.NotContains(t, region, "\r")
	require.Contains(t, region, "     2|B")
}

func TestPostEditRegion_NoHunks(t *testing.T) {
	t.Parallel()
	require.Empty(t, postEditRegion("", "content"))
	require.Empty(t, postEditRegion("garbage", "content"))
}

// End to end through replaceContent: the response carries the
// numbered post-edit region after the confirmation.
func TestReplaceContent_AppendsPostEditRegion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.txt")
	original := "package p\n\nfunc one() int {\n\treturn 1\n}\n\nfunc two() int {\n\treturn 2\n}\n"
	require.NoError(t, os.WriteFile(filePath, []byte(original), 0o644))

	tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}
	edit := editContext{
		ctx:         context.WithValue(t.Context(), SessionIDContextKey, "session"),
		permissions: &mockPermissionService{},
		files:       &mockHistoryService{},
		filetracker: tracker,
		workingDir:  dir,
	}

	resp, err := replaceContent(edit, filePath, "return 1", "return 42", false, fantasy.ToolCall{ID: "call"})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Content replaced in file: "+filePath)
	require.Contains(t, resp.Content, "|")
	require.Contains(t, resp.Content, "return 42")
	// The region shows post-edit state — the replaced text is absent.
	region := resp.Content[strings.Index(resp.Content, "\n\n"):]
	require.NotContains(t, region, "return 1")
}
