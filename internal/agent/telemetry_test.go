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

func TestHashMessage_PartTypeAndIDs(t *testing.T) {
	t.Parallel()
	user := func(text string) fantasy.Message { return telemetryMsg(fantasy.MessageRoleUser, text) }

	t.Run("part type tag separates same-text parts", func(t *testing.T) {
		t.Parallel()
		text := fantasy.Message{
			Role:    fantasy.MessageRoleUser,
			Content: []fantasy.MessagePart{fantasy.TextPart{Text: "same"}},
		}
		reasoning := fantasy.Message{
			Role:    fantasy.MessageRoleUser,
			Content: []fantasy.MessagePart{fantasy.ReasoningPart{Text: "same"}},
		}
		require.NotEqual(t, hashMessage(text), hashMessage(reasoning))
	})

	t.Run("tool call id participates", func(t *testing.T) {
		t.Parallel()
		mk := func(id string) fantasy.Message {
			return fantasy.Message{
				Role: fantasy.MessageRoleAssistant,
				Content: []fantasy.MessagePart{fantasy.ToolCallPart{
					ToolCallID: id, ToolName: "bash", Input: `{"cmd":"ls"}`,
				}},
			}
		}
		require.NotEqual(t, hashMessage(mk("call_1")), hashMessage(mk("call_2")))
		// Same everything including the ID — identical render.
		require.Equal(t, hashMessage(mk("call_1")), hashMessage(mk("call_1")))
	})

	t.Run("tool result id participates", func(t *testing.T) {
		t.Parallel()
		mk := func(id string) fantasy.Message {
			return fantasy.Message{
				Role: fantasy.MessageRoleTool,
				Content: []fantasy.MessagePart{fantasy.ToolResultPart{
					ToolCallID: id,
					Output:     fantasy.ToolResultOutputContentText{Text: "out"},
				}},
			}
		}
		require.NotEqual(t, hashMessage(mk("call_1")), hashMessage(mk("call_2")))
	})

	t.Run("unknown part types hash by discriminator", func(t *testing.T) {
		t.Parallel()
		mk := func(kind fantasy.ContentType) fantasy.Message {
			return fantasy.Message{
				Role:    fantasy.MessageRoleUser,
				Content: []fantasy.MessagePart{unknownPart{kind: kind}},
			}
		}
		// A part kind hashPart doesn't know still separates on its
		// discriminator — two different unknown kinds can't collapse
		// into an identical hash.
		require.NotEqual(t, hashMessage(mk("future-a")), hashMessage(mk("future-b")))
	})

	t.Run("field concatenation can't bleed", func(t *testing.T) {
		t.Parallel()
		mk := func(id, name string) fantasy.Message {
			return fantasy.Message{
				Role: fantasy.MessageRoleAssistant,
				Content: []fantasy.MessagePart{fantasy.ToolCallPart{
					ToolCallID: id, ToolName: name, Input: "x",
				}},
			}
		}
		// "ab"+"c" vs "a"+"bc" — separators keep the boundary real.
		require.NotEqual(t, hashMessage(mk("ab", "c")), hashMessage(mk("a", "bc")))
	})

	t.Run("diff lands on the changed index", func(t *testing.T) {
		t.Parallel()
		prev := []fantasy.Message{
			user("u1"),
			{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
				fantasy.ToolCallPart{ToolCallID: "call_1", ToolName: "bash", Input: "x"},
			}},
		}
		cur := []fantasy.Message{
			user("u1"),
			{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
				fantasy.ToolCallPart{ToolCallID: "call_9", ToolName: "bash", Input: "x"},
			}},
		}
		_, prevHashes := attributeStep(prev, nil)
		attr, _ := attributeStep(cur, prevHashes)
		require.Equal(t, 1, attr.FirstChanged)
		require.Equal(t, "history", attr.FirstChangedCause)
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

// unknownPart stands in for a MessagePart kind hashPart doesn't know
// — the hash must still separate on the discriminator.
type unknownPart struct{ kind fantasy.ContentType }

func (u unknownPart) GetType() fantasy.ContentType     { return u.kind }
func (u unknownPart) Options() fantasy.ProviderOptions { return nil }
