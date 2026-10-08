package agent

import (
	"encoding/json"
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

	vectorOf := func(msgs []fantasy.Message, tools []fantasy.AgentTool) RequestVector {
		_, prev := attributeStep(msgs, tools, RequestVector{})
		return prev
	}

	t.Run("cold start", func(t *testing.T) {
		t.Parallel()
		attr, _ := attributeStep([]fantasy.Message{sys("s"), user("u")}, nil, RequestVector{})
		require.Equal(t, 0, attr.FirstChanged)
		require.Equal(t, "cold", attr.FirstChangedCause)
		require.NotEmpty(t, attr.PrefixHash)
		require.NotEmpty(t, attr.RequestHash)
	})

	t.Run("identical render", func(t *testing.T) {
		t.Parallel()
		msgs := []fantasy.Message{sys("s"), user("u1")}
		attr, _ := attributeStep(msgs, nil, vectorOf(msgs, nil))
		require.Equal(t, -1, attr.FirstChanged)
		require.Empty(t, attr.FirstChangedCause)
	})

	t.Run("tail append", func(t *testing.T) {
		t.Parallel()
		prev := []fantasy.Message{sys("s"), user("u1")}
		cur := []fantasy.Message{sys("s"), user("u1"), user("u2")}
		attr, _ := attributeStep(cur, nil, vectorOf(prev, nil))
		require.Equal(t, 2, attr.FirstChanged)
		require.Equal(t, "append", attr.FirstChangedCause)
	})

	t.Run("shrink", func(t *testing.T) {
		t.Parallel()
		prev := []fantasy.Message{sys("s"), user("u1"), user("u2")}
		cur := []fantasy.Message{sys("s"), user("u1")}
		attr, _ := attributeStep(cur, nil, vectorOf(prev, nil))
		require.Equal(t, 2, attr.FirstChanged)
		require.Equal(t, "shrink", attr.FirstChangedCause)
	})

	t.Run("notebook prefix rewrite", func(t *testing.T) {
		t.Parallel()
		prev := []fantasy.Message{sys("s"), nb("v1"), user("u1")}
		cur := []fantasy.Message{sys("s"), nb("v2"), user("u1")}
		attrPrev, prevVec := attributeStep(prev, nil, RequestVector{})
		attr, _ := attributeStep(cur, nil, prevVec)
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
		attr, _ := attributeStep(cur, nil, vectorOf(prev, nil))
		require.Equal(t, 0, attr.FirstChanged)
		require.Equal(t, "system-prompt", attr.FirstChangedCause)
	})

	t.Run("mid-history edit", func(t *testing.T) {
		t.Parallel()
		prev := []fantasy.Message{sys("s"), user("u1"), user("u2a")}
		cur := []fantasy.Message{sys("s"), user("u1"), user("u2b")}
		attr, _ := attributeStep(cur, nil, vectorOf(prev, nil))
		require.Equal(t, 2, attr.FirstChanged)
		require.Equal(t, "history", attr.FirstChangedCause)
	})

	t.Run("prefix hash stable across identical prefixes", func(t *testing.T) {
		t.Parallel()
		a := []fantasy.Message{sys("s"), user("u1")}
		b := []fantasy.Message{sys("s"), user("different tail")}
		attrA, _ := attributeStep(a, nil, RequestVector{})
		attrB, _ := attributeStep(b, nil, RequestVector{})
		require.Equal(t, attrA.PrefixHash, attrB.PrefixHash)
	})

	// A notebook block INSERTED into the system run likewise shifts
	// history — same notebook attribution.
	t.Run("notebook block insertion", func(t *testing.T) {
		t.Parallel()
		prev := []fantasy.Message{sys("s"), user("u1")}
		cur := []fantasy.Message{sys("s"), nb("v1"), user("u1")}
		attr, _ := attributeStep(cur, nil, vectorOf(prev, nil))
		require.Equal(t, 1, attr.FirstChanged)
		require.Equal(t, "notebook-prefix", attr.FirstChangedCause)
	})

	// Tool schemas hash in wire order — same tools, different order,
	// different request.
	t.Run("tool order changes the digest", func(t *testing.T) {
		t.Parallel()
		a := []fantasy.AgentTool{&fakeTool{name: "view"}, &fakeTool{name: "bash"}}
		b := []fantasy.AgentTool{&fakeTool{name: "bash"}, &fakeTool{name: "view"}}
		require.NotEqual(t, hashTools(a), hashTools(b))
	})

	// A schema-body change on one tool lands as tool-schemas —
	// no message index exists to point at, so the index stays -1.
	t.Run("tool schema change", func(t *testing.T) {
		t.Parallel()
		msgs := []fantasy.Message{sys("s"), user("u1")}
		before := []fantasy.AgentTool{&fakeTool{name: "bash", desc: "v1"}}
		after := []fantasy.AgentTool{&fakeTool{name: "bash", desc: "v2"}}
		attr, _ := attributeStep(msgs, after, vectorOf(msgs, before))
		require.Equal(t, -1, attr.FirstChanged)
		require.Equal(t, "tool-schemas", attr.FirstChangedCause)
	})
}

// Removing a notebook system block shifts every later index — the
// segmented vector keeps it a notebook cause, not history (#115).
func TestReviewCacheNotebookRemoval(t *testing.T) {
	t.Parallel()
	sys := func(text string) fantasy.Message { return telemetryMsg(fantasy.MessageRoleSystem, text) }
	user := func(text string) fantasy.Message { return telemetryMsg(fantasy.MessageRoleUser, text) }
	nb := func(text string) fantasy.Message { return sys("<notebook>\n" + text + "\n</notebook>") }
	prev := []fantasy.Message{sys("s"), nb("v1"), user("u1")}
	cur := []fantasy.Message{sys("s"), user("u1")}
	_, prevVec := attributeStep(prev, nil, RequestVector{})
	attr, _ := attributeStep(cur, nil, prevVec)
	require.Equal(t, 1, attr.FirstChanged)
	require.Equal(t, "notebook-prefix", attr.FirstChangedCause)
}

// Tool-call identity lives inside the full-request fingerprint —
// two requests differing only in ToolCallID hash differently (#115).
func TestReviewCacheToolIDFidelity(t *testing.T) {
	t.Parallel()
	mk := func(id string) fantasy.Message {
		return fantasy.Message{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{fantasy.ToolCallPart{
				ToolCallID: id, ToolName: "bash", Input: "x",
			}},
		}
	}
	va := hashRequest([]fantasy.Message{mk("call_1")}, nil)
	vb := hashRequest([]fantasy.Message{mk("call_9")}, nil)
	require.NotEqual(t, digestVector(va), digestVector(vb))
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
		_, prevVec := attributeStep(prev, nil, RequestVector{})
		attr, _ := attributeStep(cur, nil, prevVec)
		require.Equal(t, 1, attr.FirstChanged)
		require.Equal(t, "history", attr.FirstChangedCause)
	})
}

// The eval driver restarts the process per turn — the prior
// process's final vector arrives via CRUSH_EVAL_REQUEST_VECTOR, so
// a notebook change at a restart boundary attributes as
// notebook-prefix, not cold (#115). No t.Parallel: t.Setenv.
func TestReviewCacheRestartAttribution(t *testing.T) {
	sys := func(text string) fantasy.Message { return telemetryMsg(fantasy.MessageRoleSystem, text) }
	user := func(text string) fantasy.Message { return telemetryMsg(fantasy.MessageRoleUser, text) }
	nb := func(text string) fantasy.Message { return sys("<notebook>\n" + text + "\n</notebook>") }
	prev := []fantasy.Message{sys("s"), nb("v1"), user("u1")}
	cur := []fantasy.Message{sys("s"), nb("v2"), user("u1")}
	_, seed := attributeStep(prev, nil, RequestVector{})
	seed.SessionID = "sess"
	raw, err := json.Marshal(seed)
	require.NoError(t, err)
	t.Setenv(EvalRequestVectorEnvVar, string(raw))
	// The restarted process's first step diffs against the handoff —
	// a real cause, not cold.
	attr, _ := attributeStep(cur, nil, restartVector("sess"))
	require.Equal(t, 1, attr.FirstChanged)
	require.Equal(t, "notebook-prefix", attr.FirstChangedCause)
}

// The restart handoff must never let one session's fingerprint seed
// another's diff — a mismatched session ID or malformed payload
// degrades to a genuine cold start, not a bogus attribution (#115).
// No t.Parallel: t.Setenv.
func TestRestartVector_ScopeGuards(t *testing.T) {
	sys := telemetryMsg(fantasy.MessageRoleSystem, "s")
	_, v := attributeStep([]fantasy.Message{sys, telemetryMsg(fantasy.MessageRoleUser, "u")}, nil, RequestVector{})
	v.SessionID = "sess-a"
	raw, err := json.Marshal(v)
	require.NoError(t, err)

	t.Run("matching session seeds the diff", func(t *testing.T) {
		t.Setenv(EvalRequestVectorEnvVar, string(raw))
		got := restartVector("sess-a")
		require.False(t, got.Empty())
		require.Equal(t, v.System, got.System)
	})

	t.Run("foreign session is ignored", func(t *testing.T) {
		t.Setenv(EvalRequestVectorEnvVar, string(raw))
		require.True(t, restartVector("sess-b").Empty())
	})

	t.Run("malformed payload is ignored", func(t *testing.T) {
		t.Setenv(EvalRequestVectorEnvVar, "{not json")
		require.True(t, restartVector("sess-a").Empty())
	})

	t.Run("absent env is cold", func(t *testing.T) {
		require.True(t, restartVector("sess-a").Empty())
	})
}

// An identical request with a cache_read regression carries no local
// cause — the record labels it provider-side instead of inventing a
// component (#115).
func TestCacheAnomaly(t *testing.T) {
	t.Parallel()
	msgs := []fantasy.Message{
		telemetryMsg(fantasy.MessageRoleSystem, "s"),
		telemetryMsg(fantasy.MessageRoleUser, "u"),
	}
	_, prev := attributeStep(msgs, nil, RequestVector{})
	prev.CacheRead = 4096 // The fold stamps the producing step's hits.
	cur, _ := attributeStep(msgs, nil, prev)
	// Identical render, cache dropped to zero — provider-side.
	require.True(t, cacheAnomaly(cur, 0))
	// Identical render, cache grew — a warm cache is not an anomaly.
	require.False(t, cacheAnomaly(cur, 8192))
	// A changed request never claims provider-side even at zero.
	changed := []fantasy.Message{msgs[0], telemetryMsg(fantasy.MessageRoleUser, "u2")}
	cur2, _ := attributeStep(changed, nil, prev)
	require.False(t, cacheAnomaly(cur2, 0))
	// No baseline — a cold start's zero cache can't be a regression.
	cold, _ := attributeStep(msgs, nil, RequestVector{})
	require.False(t, cacheAnomaly(cold, 0))
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

// The usage ledger counts every invocation — continuations and
// summarize calls included — unlike the returned AgentResult, which
// only covers whichever call came back last.
func TestRecordUsage_LedgerAccumulatesAllInvocations(t *testing.T) {
	t.Parallel()

	a := &sessionAgent{usageLedger: csync.NewMap[string, ledgerUsage]()}
	a.recordUsage("sess", &fantasy.AgentResult{
		TotalUsage: fantasy.Usage{InputTokens: 100, OutputTokens: 10},
		Steps:      make([]fantasy.StepResult, 2),
	})
	// A second invocation — a queue continuation or a summarize call —
	// adds to the same session's total rather than replacing it.
	a.recordUsage("sess", &fantasy.AgentResult{
		TotalUsage: fantasy.Usage{InputTokens: 50, OutputTokens: 5, CacheReadTokens: 7},
		Steps:      make([]fantasy.StepResult, 3),
	})

	u, ok := a.usageLedger.Get("sess")
	require.True(t, ok)
	require.Equal(t, int64(150), u.InputTokens)
	require.Equal(t, int64(15), u.OutputTokens)
	require.Equal(t, int64(7), u.CacheReadTokens)
	require.Equal(t, 5, u.Steps)

	// A nil result (failed stream), a nil ledger (ledger-less agent),
	// and an empty session ID are all no-ops.
	a.recordUsage("sess", nil)
	(&sessionAgent{}).recordUsage("sess", &fantasy.AgentResult{TotalUsage: fantasy.Usage{InputTokens: 1}})
	a.recordUsage("", &fantasy.AgentResult{TotalUsage: fantasy.Usage{InputTokens: 1}})
	u, _ = a.usageLedger.Get("sess")
	require.Equal(t, int64(150), u.InputTokens)
	_, ok = a.usageLedger.Get("")
	require.False(t, ok)
}

func TestSessionTelemetry_LedgerUsage(t *testing.T) {
	t.Parallel()

	a := &sessionAgent{
		usageLedger: csync.NewMap[string, ledgerUsage](),
		stubStats:   csync.NewMap[string, stubStats](),
		nbStats:     csync.NewMap[string, notebook.Stats](),
	}
	a.usageLedger.Set("sess", ledgerUsage{InputTokens: 42, OutputTokens: 7, Steps: 5})
	c := &coordinator{mainAgent: a}

	tel := c.SessionTelemetry("sess")
	require.Equal(t, int64(42), tel.LedgerUsage.InputTokens)
	require.Equal(t, int64(7), tel.LedgerUsage.OutputTokens)
	require.Equal(t, 5, tel.LedgerSteps)
}

func TestSessionTelemetry_Tail(t *testing.T) {
	t.Parallel()

	a := &sessionAgent{
		tailAudit: csync.NewMap[string, TailAudit](),
		stubStats: csync.NewMap[string, stubStats](),
		nbStats:   csync.NewMap[string, notebook.Stats](),
	}
	a.tailAudit.Set("sess", TailAudit{
		Sections: []TailSection{{Name: "open_failures", Bytes: 120}},
		Bytes:    120,
		SHA256:   "abc",
		Text:     "<open_failures>\n- make test: FAIL\n</open_failures>\n",
	})
	c := &coordinator{mainAgent: a}

	tel := c.SessionTelemetry("sess")
	require.NotNil(t, tel.Tail)
	require.Equal(t, "open_failures", tel.Tail.Sections[0].Name)
	require.Equal(t, 120, tel.Tail.Bytes)
	require.Equal(t, "abc", tel.Tail.SHA256)
	require.Contains(t, tel.Tail.Text, "make test")

	// A session that never rendered a tail reports nil — "no tail"
	// must not alias "telemetry absent".
	require.Nil(t, c.SessionTelemetry("other").Tail)
}

// unknownPart stands in for a MessagePart kind hashPart doesn't know
// — the hash must still separate on the discriminator.
type unknownPart struct{ kind fantasy.ContentType }

func (u unknownPart) GetType() fantasy.ContentType     { return u.kind }
func (u unknownPart) Options() fantasy.ProviderOptions { return nil }
