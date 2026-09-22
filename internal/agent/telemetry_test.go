package agent

import (
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/notebook"
	"github.com/stretchr/testify/require"
)

func telemetryMsg(role fantasy.MessageRole, text string) fantasy.Message {
	return fantasy.Message{
		Role:    role,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}},
	}
}

func TestAttributeStep(t *testing.T) {
	t.Parallel()
	sys := func(text string) fantasy.Message { return telemetryMsg(fantasy.MessageRoleSystem, text) }
	user := func(text string) fantasy.Message { return telemetryMsg(fantasy.MessageRoleUser, text) }
	nb := func(text string) fantasy.Message { return sys("<notebook>\n" + text + "\n</notebook>") }

	hashesOf := func(msgs []fantasy.Message) []uint64 {
		_, prev := attributeStep(msgs, nil)
		return prev
	}

	t.Run("cold start", func(t *testing.T) {
		t.Parallel()
		attr, _ := attributeStep([]fantasy.Message{sys("s"), user("u")}, nil)
		require.Equal(t, 0, attr.FirstChanged)
		require.Equal(t, "cold", attr.FirstChangedCause)
		require.NotEmpty(t, attr.PrefixHash)
	})

	t.Run("identical render", func(t *testing.T) {
		t.Parallel()
		msgs := []fantasy.Message{sys("s"), user("u1")}
		attr, _ := attributeStep(msgs, hashesOf(msgs))
		require.Equal(t, -1, attr.FirstChanged)
		require.Empty(t, attr.FirstChangedCause)
	})

	t.Run("tail append", func(t *testing.T) {
		t.Parallel()
		prev := []fantasy.Message{sys("s"), user("u1")}
		cur := []fantasy.Message{sys("s"), user("u1"), user("u2")}
		attr, _ := attributeStep(cur, hashesOf(prev))
		require.Equal(t, 2, attr.FirstChanged)
		require.Equal(t, "append", attr.FirstChangedCause)
	})

	t.Run("shrink", func(t *testing.T) {
		t.Parallel()
		prev := []fantasy.Message{sys("s"), user("u1"), user("u2")}
		cur := []fantasy.Message{sys("s"), user("u1")}
		attr, _ := attributeStep(cur, hashesOf(prev))
		require.Equal(t, 2, attr.FirstChanged)
		require.Equal(t, "shrink", attr.FirstChangedCause)
	})

	t.Run("notebook prefix rewrite", func(t *testing.T) {
		t.Parallel()
		prev := []fantasy.Message{sys("s"), nb("v1"), user("u1")}
		cur := []fantasy.Message{sys("s"), nb("v2"), user("u1")}
		attrPrev, prevHashes := attributeStep(prev, nil)
		attr, _ := attributeStep(cur, prevHashes)
		require.Equal(t, 1, attr.FirstChanged)
		require.Equal(t, "notebook-prefix", attr.FirstChangedCause)
		// The prefix hash covers the whole leading system run, so a
		// notebook rewrite moves it even at a fixed message count.
		require.NotEqual(t, attrPrev.PrefixHash, attr.PrefixHash)
	})

	t.Run("system prompt rewrite", func(t *testing.T) {
		t.Parallel()
		prev := []fantasy.Message{sys("v1"), user("u1")}
		cur := []fantasy.Message{sys("v2"), user("u1")}
		attr, _ := attributeStep(cur, hashesOf(prev))
		require.Equal(t, 0, attr.FirstChanged)
		require.Equal(t, "system-prompt", attr.FirstChangedCause)
	})

	t.Run("mid-history edit", func(t *testing.T) {
		t.Parallel()
		prev := []fantasy.Message{sys("s"), user("u1"), user("u2a")}
		cur := []fantasy.Message{sys("s"), user("u1"), user("u2b")}
		attr, _ := attributeStep(cur, hashesOf(prev))
		require.Equal(t, 2, attr.FirstChanged)
		require.Equal(t, "history", attr.FirstChangedCause)
	})

	t.Run("prefix hash stable across identical prefixes", func(t *testing.T) {
		t.Parallel()
		a := []fantasy.Message{sys("s"), user("u1")}
		b := []fantasy.Message{sys("s"), user("different tail")}
		attrA, _ := attributeStep(a, nil)
		attrB, _ := attributeStep(b, nil)
		require.Equal(t, attrA.PrefixHash, attrB.PrefixHash)
	})
}

func TestNoteBoundaryAdvance_VerbatimArm(t *testing.T) {
	t.Parallel()
	// A verbatim control carries no collapse/supersession machinery —
	// the counter must still record prefix churn.
	a := &sessionAgent{stubStats: csync.NewMap[string, stubStats]()}
	a.noteBoundaryAdvance("sess")
	a.noteBoundaryAdvance("sess")
	s, ok := a.stubStats.Get("sess")
	require.True(t, ok)
	require.Equal(t, 2, s.BoundaryAdvances)
}

func TestRecordGeneratorUsage(t *testing.T) {
	t.Parallel()
	c := &coordinator{nbStats: csync.NewMap[string, notebook.Stats]()}
	c.RecordGeneratorUsage("sess", fantasy.Usage{InputTokens: 100, OutputTokens: 10, CacheCreationTokens: 5})
	c.RecordGeneratorUsage("sess", fantasy.Usage{InputTokens: 50, OutputTokens: 5, CacheReadTokens: 7})
	n, ok := c.nbStats.Get("sess")
	require.True(t, ok)
	require.Equal(t, 2, n.GeneratorCalls)
	require.Equal(t, int64(150), n.GeneratorInputTokens)
	require.Equal(t, int64(15), n.GeneratorOutputTokens)
	require.Equal(t, int64(5), n.GeneratorCacheWriteTokens)
	require.Equal(t, int64(7), n.GeneratorCacheReadTokens)

	// Notebook generation runs detached — concurrent callbacks for one
	// session must not lose counts.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.RecordGeneratorUsage("sess", fantasy.Usage{InputTokens: 1})
		}()
	}
	wg.Wait()
	n, _ = c.nbStats.Get("sess")
	require.Equal(t, 10, n.GeneratorCalls)
	require.Equal(t, int64(158), n.GeneratorInputTokens)

	// A nil map and an empty session ID are no-ops.
	c2 := &coordinator{}
	c2.RecordGeneratorUsage("sess", fantasy.Usage{InputTokens: 1})
	c.RecordGeneratorUsage("", fantasy.Usage{InputTokens: 1})
	_, ok = c.nbStats.Get("")
	require.False(t, ok)
}
