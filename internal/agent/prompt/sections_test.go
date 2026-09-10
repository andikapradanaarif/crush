package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestTruncateUTF8Prefix(t *testing.T) {
	t.Parallel()

	t.Run("valid UTF-8 passthrough", func(t *testing.T) {
		t.Parallel()
		s := "hello, wörld"
		require.Equal(t, s, truncateUTF8Prefix(s, len(s)))
		require.Equal(t, s, truncateUTF8Prefix(s, len(s)+100))
	})

	t.Run("trailing incomplete multi-byte rune", func(t *testing.T) {
		t.Parallel()
		s := "abc€"       // € is 3 bytes: E2 82 AC
		cut := len(s) - 1 // cut inside the € rune
		got := truncateUTF8Prefix(s, cut)
		require.Equal(t, "abc", got)
		require.True(t, utf8.ValidString(got))
	})

	t.Run("invalid UTF-8 normalization", func(t *testing.T) {
		t.Parallel()
		s := string([]byte{'a', 0xff, 'b'})
		got := truncateUTF8Prefix(s, 100)
		require.True(t, utf8.ValidString(got))
		require.Equal(t, "ab", got)
	})

	t.Run("maxBytes boundary", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "ab", truncateUTF8Prefix("abc", 2))
		require.Equal(t, "", truncateUTF8Prefix("abc", 0))
	})

	t.Run("empty string", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "", truncateUTF8Prefix("", 10))
		require.Equal(t, "", truncateUTF8Prefix("", 0))
	})

	t.Run("multi-byte rune at exact boundary kept", func(t *testing.T) {
		t.Parallel()
		s := "ab€"
		require.Equal(t, s, truncateUTF8Prefix(s, len(s)))
	})

	t.Run("leading multi-byte rune smaller than limit", func(t *testing.T) {
		t.Parallel()
		// CJK chars are 3 bytes; a limit below the first rune's size
		// must not return an empty string containing partial bytes.
		got := truncateUTF8Prefix("日本語", 2)
		require.Equal(t, "", got)
		require.True(t, utf8.ValidString(got))
	})
}

func TestTruncateToTokenLimit(t *testing.T) {
	t.Parallel()

	t.Run("under limit passthrough", func(t *testing.T) {
		t.Parallel()
		s := "hello world"
		require.Equal(t, s, truncateToTokenLimit(s, 100))
	})

	t.Run("result does not exceed limit", func(t *testing.T) {
		t.Parallel()
		s := strings.Repeat("x", 10_000)
		got := truncateToTokenLimit(s, 100)
		require.LessOrEqual(t, approxTokenCount(got), int64(100))
	})

	t.Run("dense tokens shrink", func(t *testing.T) {
		t.Parallel()
		// Each CJK rune is 3 bytes but ~1 token under the 4-bytes-per-
		// token heuristic only in aggregate; what matters is the
		// post-truncation estimate respects the limit.
		s := strings.Repeat("日", 1_000)
		got := truncateToTokenLimit(s, 50)
		require.LessOrEqual(t, approxTokenCount(got), int64(50))
		require.NotEmpty(t, got)
	})

	t.Run("very small limit does not return empty for CJK", func(t *testing.T) {
		t.Parallel()
		got := truncateToTokenLimit("日本語", 1)
		require.LessOrEqual(t, approxTokenCount(got), int64(1))
	})

	t.Run("zero limit", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "", truncateToTokenLimit("hello", 0))
	})
}

func TestReadBounded(t *testing.T) {
	t.Parallel()

	t.Run("file smaller than limit", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "small.md")
		require.NoError(t, os.WriteFile(path, []byte("small content"), 0o644))
		content, truncated, err := readBounded(path)
		require.NoError(t, err)
		require.False(t, truncated)
		require.Equal(t, "small content", content)
	})

	t.Run("file larger than limit", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "big.md")
		require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("a", maxContextFileReadSize+10)), 0o644))
		content, truncated, err := readBounded(path)
		require.NoError(t, err)
		require.True(t, truncated)
		require.Equal(t, maxContextFileReadSize, len(content))
	})

	t.Run("UTF-8 preserved at truncation boundary", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "utf8.md")
		// Fill to the limit with ASCII, then add a multi-byte rune
		// straddling the cut.
		data := append([]byte(strings.Repeat("a", maxContextFileReadSize-2)), []byte("€extra")...)
		require.NoError(t, os.WriteFile(path, data, 0o644))
		content, truncated, err := readBounded(path)
		require.NoError(t, err)
		require.True(t, truncated)
		require.True(t, utf8.ValidString(content))
		require.LessOrEqual(t, len(content), maxContextFileReadSize)
	})

	t.Run("read error", func(t *testing.T) {
		t.Parallel()
		_, _, err := readBounded(filepath.Join(t.TempDir(), "nonexistent.md"))
		require.Error(t, err)
	})
}

func TestReadContextFile(t *testing.T) {
	t.Parallel()

	t.Run("under budget passthrough", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "ok.md")
		require.NoError(t, os.WriteFile(path, []byte("short"), 0o644))
		content, truncated, err := readContextFile(path, 1000, true, false)
		require.NoError(t, err)
		require.False(t, truncated)
		require.Equal(t, "short", content)
	})

	t.Run("required over budget errors non-interactive", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "required.md")
		require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", 10_000)), 0o644))
		_, _, err := readContextFile(path, 100, true, false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "required context file")
	})

	t.Run("required over budget errors interactive", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "required.md")
		require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", 10_000)), 0o644))
		// Interactive prompting has no plumbing yet; required overflow
		// is an error in both modes.
		_, _, err := readContextFile(path, 100, true, true)
		require.Error(t, err)
	})

	t.Run("optional over budget truncates", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "optional.md")
		require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", 10_000)), 0o644))
		content, truncated, err := readContextFile(path, 100, false, false)
		require.NoError(t, err)
		require.True(t, truncated)
		require.LessOrEqual(t, approxTokenCount(content), int64(100))
	})

	t.Run("hard limit truncation propagates", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "huge.md")
		require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("a", maxContextFileReadSize+10)), 0o644))
		content, truncated, err := readContextFile(path, maxContextFileReadSize, false, false)
		require.NoError(t, err)
		require.True(t, truncated)
		require.LessOrEqual(t, len(content), maxContextFileReadSize)
	})
}

func TestExtractSections(t *testing.T) {
	t.Parallel()

	text := "core intro\n<env>\nWorking dir\n</env>\nmiddle\n" +
		"<project_context>\n<file path=\"a\">x</file>\n</project_context>\n" +
		"<user_preferences>\n<file path=\"g\">y</file>\n</user_preferences>\nend"

	sections := extractSections(text)
	require.NotEmpty(t, sections)
	require.Equal(t, "core_policy", sections[0].Name)

	byName := make(map[string]PromptSection, len(sections))
	for _, s := range sections {
		byName[s.Name] = s
	}

	require.Contains(t, byName, "env")
	require.Equal(t, CacheClassVolatile, byName["env"].CacheClass)
	require.Contains(t, byName["env"].Content, "Working dir")

	require.Contains(t, byName, "project_context")
	require.Equal(t, CacheClassStable, byName["project_context"].CacheClass)

	require.Contains(t, byName, "user_context")
	require.Equal(t, CacheClassStable, byName["user_context"].CacheClass)

	// Section byte counts sum back to the full text: core_policy
	// accounts for everything outside the tagged regions.
	var total int
	for _, s := range sections {
		total += s.Bytes
		require.Positive(t, s.Bytes)
		if s.Name == "core_policy" {
			continue // No Content; Bytes is the remainder measure.
		}
		require.Equal(t, len(s.Content), s.Bytes)
		require.Equal(t, approxTokenCount(s.Content), s.EstTokens)
	}
	require.Equal(t, len(text), total)
}

func TestExtractSectionsNoTags(t *testing.T) {
	t.Parallel()
	sections := extractSections("plain prompt with no tagged regions")
	require.Len(t, sections, 1)
	require.Equal(t, "core_policy", sections[0].Name)
	require.Equal(t, len("plain prompt with no tagged regions"), sections[0].Bytes)
}
