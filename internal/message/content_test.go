package message

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/stretchr/testify/require"
)

func makeTestAttachments(n int, contentSize int) []Attachment {
	attachments := make([]Attachment, n)
	content := []byte(strings.Repeat("x", contentSize))
	for i := range n {
		attachments[i] = Attachment{
			FilePath: fmt.Sprintf("/path/to/file%d.txt", i),
			MimeType: "text/plain",
			Content:  content,
		}
	}
	return attachments
}

func TestToAIMessage_CorruptedMediaData(t *testing.T) {
	t.Parallel()

	msg := &Message{
		Role: Tool,
		Parts: []ContentPart{
			ToolResult{
				ToolCallID: "call_123",
				Name:       "screenshot",
				Content:    "Loaded image/png content",
				Data:       "abc\x80def",
				MIMEType:   "image/png",
			},
		},
	}

	messages := msg.ToAIMessage()
	require.Len(t, messages, 1)
	require.Len(t, messages[0].Content, 1)

	part, ok := messages[0].Content[0].(fantasy.ToolResultPart)
	require.True(t, ok)

	require.Equal(t, "call_123", part.ToolCallID)

	textContent, ok := part.Output.(fantasy.ToolResultOutputContentText)
	require.True(t, ok, "corrupted media should be downgraded to text")
	require.Equal(t, mediaLoadFailedPlaceholder, textContent.Text)
}

func TestToAIMessage_ValidMediaData(t *testing.T) {
	t.Parallel()

	validBase64 := base64.StdEncoding.EncodeToString([]byte{0x89, 0x50, 0x4E, 0x47})

	msg := &Message{
		Role: Tool,
		Parts: []ContentPart{
			ToolResult{
				ToolCallID: "call_456",
				Name:       "screenshot",
				Content:    "Loaded image/png content",
				Data:       validBase64,
				MIMEType:   "image/png",
			},
		},
	}

	messages := msg.ToAIMessage()
	require.Len(t, messages, 1)
	require.Len(t, messages[0].Content, 1)

	part, ok := messages[0].Content[0].(fantasy.ToolResultPart)
	require.True(t, ok)

	require.Equal(t, "call_456", part.ToolCallID)

	mediaContent, ok := part.Output.(fantasy.ToolResultOutputContentMedia)
	require.True(t, ok, "valid media should remain as media")
	require.Equal(t, validBase64, mediaContent.Data)
	require.Equal(t, "image/png", mediaContent.MediaType)
}

func TestToAIMessage_ASCIIButInvalidBase64(t *testing.T) {
	t.Parallel()

	msg := &Message{
		Role: Tool,
		Parts: []ContentPart{
			ToolResult{
				ToolCallID: "call_789",
				Name:       "screenshot",
				Content:    "Loaded image/png content",
				Data:       "not-valid-base64!!!",
				MIMEType:   "image/png",
			},
		},
	}

	messages := msg.ToAIMessage()
	require.Len(t, messages, 1)
	require.Len(t, messages[0].Content, 1)

	part, ok := messages[0].Content[0].(fantasy.ToolResultPart)
	require.True(t, ok)

	require.Equal(t, "call_789", part.ToolCallID)

	textContent, ok := part.Output.(fantasy.ToolResultOutputContentText)
	require.True(t, ok, "ASCII but invalid base64 should be downgraded to text")
	require.Equal(t, mediaLoadFailedPlaceholder, textContent.Text)
}

func BenchmarkPromptWithTextAttachments(b *testing.B) {
	cases := []struct {
		name        string
		numFiles    int
		contentSize int
	}{
		{"1file_100bytes", 1, 100},
		{"5files_1KB", 5, 1024},
		{"10files_10KB", 10, 10 * 1024},
		{"20files_50KB", 20, 50 * 1024},
	}

	for _, tc := range cases {
		attachments := makeTestAttachments(tc.numFiles, tc.contentSize)
		prompt := "Process these files"

		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = PromptWithTextAttachments(prompt, attachments)
			}
		})
	}
}

func TestResetStreamedContent(t *testing.T) {
	t.Parallel()

	msg := &Message{}
	msg.AddImageURL("https://example.com/img.png", "high")
	msg.AppendContent("partial answer")
	msg.AppendReasoningContent("", "thinking...")
	msg.AddToolCall(ToolCall{ID: "1", Name: "bash"})
	msg.AddToolResult(ToolResult{ToolCallID: "1", Content: "output"})
	msg.AddFinish(FinishReasonError, "boom", "stream died")

	msg.ResetStreamedContent()

	// Streamed parts are gone.
	require.Empty(t, msg.Content().Text, "text should be cleared")
	require.Empty(t, msg.ReasoningContent().Thinking, "reasoning should be cleared")
	require.Empty(t, msg.ToolCalls(), "tool calls should be cleared")
	require.Nil(t, msg.FinishPart(), "finish should be cleared")

	// Non-streamed parts survive.
	require.Len(t, msg.ImageURLContent(), 1, "image should survive")
	require.Len(t, msg.ToolResults(), 1, "tool results should survive")
}

func TestResetStreamedContentEmpty(t *testing.T) {
	t.Parallel()

	// Reset on an empty message is a no-op and must not panic.
	msg := &Message{}
	msg.ResetStreamedContent()
	require.Empty(t, msg.Parts)
}

func TestReasoningMutators_PreserveProviderFields(t *testing.T) {
	t.Parallel()

	// Every reasoning mutator is copy-and-mutate: provider metadata
	// written mid-stream survives the delta → end → finish sequence.
	t.Run("google thought signature", func(t *testing.T) {
		t.Parallel()
		msg := &Message{}
		msg.AppendReasoningContent("b1", "thinking ")
		msg.AppendThoughtSignature("b1", "gsig", "call-1")
		msg.AppendReasoningContent("b1", "more")
		msg.FinishThinking("b1")

		got := msg.ReasoningContent()
		require.Equal(t, "thinking more", got.Thinking)
		require.Equal(t, "gsig", got.ThoughtSignature)
		require.Equal(t, "call-1", got.ToolID)
		require.NotZero(t, got.FinishedAt)
	})

	t.Run("anthropic signature", func(t *testing.T) {
		t.Parallel()
		msg := &Message{}
		msg.AppendReasoningContent("b1", "thinking")
		msg.AppendReasoningSignature("b1", "asig")
		msg.AppendReasoningContent("b1", " more")
		msg.FinishThinking("b1")

		got := msg.ReasoningContent()
		require.Equal(t, "thinking more", got.Thinking)
		require.Equal(t, "asig", got.Signature)
		require.NotZero(t, got.FinishedAt)
	})

	t.Run("openai responses metadata", func(t *testing.T) {
		t.Parallel()
		data := &openai.ResponsesReasoningMetadata{ItemID: "rs_1"}
		msg := &Message{}
		msg.AppendReasoningContent("b1", "thinking")
		msg.SetReasoningResponsesData("b1", data)
		msg.AppendReasoningContent("b1", " more")
		msg.FinishThinking("b1")

		got := msg.ReasoningContent()
		require.Equal(t, "thinking more", got.Thinking)
		require.Same(t, data, got.ResponsesData)
		require.NotZero(t, got.FinishedAt)
	})

	t.Run("all providers on one part", func(t *testing.T) {
		t.Parallel()
		data := &openai.ResponsesReasoningMetadata{ItemID: "rs_1"}
		msg := &Message{}
		msg.AppendReasoningContent("b1", "thinking")
		msg.AppendReasoningSignature("b1", "asig")
		msg.AppendThoughtSignature("b1", "gsig", "call-1")
		msg.SetReasoningResponsesData("b1", data)
		msg.FinishThinking("b1")
		msg.AppendReasoningContent("b1", "more")

		got := msg.ReasoningContent()
		require.Equal(t, "thinkingmore", got.Thinking)
		require.Equal(t, "asig", got.Signature)
		require.Equal(t, "gsig", got.ThoughtSignature)
		require.Equal(t, "call-1", got.ToolID)
		require.Same(t, data, got.ResponsesData)
	})
}

func TestReasoningMutators_MultiBlockAddressing(t *testing.T) {
	t.Parallel()

	// Interleaved blocks keep their ids: deltas, signatures, and finish
	// land on the addressed part, never first-part-everything.
	msg := &Message{}
	msg.AppendReasoningContent("a", "ta ")
	msg.AppendReasoningContent("b", "tb ")
	msg.AppendReasoningContent("a", "more-a")
	msg.AppendThoughtSignature("a", "gsig-a", "call-a")
	msg.AppendReasoningSignature("b", "asig-b")
	msg.FinishThinking("a")
	msg.AppendReasoningContent("b", "more-b")

	var a, b ReasoningContent
	for _, part := range msg.Parts {
		if c, ok := part.(ReasoningContent); ok {
			switch c.ID {
			case "a":
				a = c
			case "b":
				b = c
			}
		}
	}
	require.Equal(t, "a", a.ID)
	require.Equal(t, "b", b.ID)
	require.Equal(t, "ta more-a", a.Thinking)
	require.Equal(t, "gsig-a", a.ThoughtSignature)
	require.Equal(t, "call-a", a.ToolID)
	require.NotZero(t, a.FinishedAt)
	require.Equal(t, "tb more-b", b.Thinking)
	require.Equal(t, "asig-b", b.Signature)
	require.Zero(t, b.FinishedAt, "FinishThinking(\"a\") must not close block b")
}

func TestReasoningMutators_EmptyIDFallback(t *testing.T) {
	t.Parallel()

	// Id-less streams accumulate on one part: deltas append to the
	// last-open part, finish closes it, and a later block's deltas merge
	// into it rather than spawn a part ReasoningContent() cannot see.
	msg := &Message{}
	msg.AppendReasoningContent("", "one")
	msg.FinishThinking("")
	msg.AppendReasoningContent("", " two")
	msg.AppendReasoningSignature("", "sig")

	count := 0
	for _, part := range msg.Parts {
		if _, ok := part.(ReasoningContent); ok {
			count++
		}
	}
	require.Equal(t, 1, count, "id-less stream must stay single-part")
	got := msg.ReasoningContent()
	require.Equal(t, "one two", got.Thinking)
	require.Equal(t, "sig", got.Signature)
	require.NotZero(t, got.FinishedAt)
}

func TestReasoningMutators_UnknownIDCreatesPart(t *testing.T) {
	t.Parallel()

	// A signature for a block whose start never streamed still lands —
	// dropped metadata is worse than a carrier part.
	msg := &Message{}
	msg.AppendThoughtSignature("late", "gsig", "call-9")

	got := msg.ReasoningContent()
	require.Equal(t, "late", got.ID)
	require.Equal(t, "gsig", got.ThoughtSignature)
	require.Equal(t, "call-9", got.ToolID)
}

func TestToolCallMutators_PreserveProviderExecuted(t *testing.T) {
	t.Parallel()

	msg := &Message{}
	msg.AddToolCall(ToolCall{ID: "t1", Name: "web_search", ProviderExecuted: true, Input: "{"})
	msg.AppendToolCallInput("t1", `"q":"x"}`)
	msg.FinishToolCall("t1")

	calls := msg.ToolCalls()
	require.Len(t, calls, 1)
	require.True(t, calls[0].ProviderExecuted, "provider-executed flag must survive input/finish rebuilds")
	require.Equal(t, `{"q":"x"}`, calls[0].Input)
	require.True(t, calls[0].Finished)
}
