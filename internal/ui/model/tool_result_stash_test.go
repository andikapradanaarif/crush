package model

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/ui/chat"
)

// A tool result can reach the UI before the assistant-message update
// that creates its tool item — event reorder or a missed update. The
// result must be kept and attached when the item is built: dropping it
// leaves the item rendering "Waiting for tool response..." until a
// full session reload rebuilds the items from storage.
func TestToolResultBeforeToolItemIsAttached(t *testing.T) {
	t.Parallel()
	m := newBusyUI(&countingWorkspace{})

	result := message.ToolResult{ToolCallID: "call-1", Name: "bash", Content: "done"}
	_, cmd := m.Update(pubsub.Event[message.Message]{
		Type: pubsub.CreatedEvent,
		Payload: message.Message{
			ID: "res-1", SessionID: "s1", Role: message.Tool,
			Parts: []message.ContentPart{result},
		},
	})
	runCmds(m, cmd)

	call := message.ToolCall{ID: "call-1", Name: "bash", Input: "{}", Finished: true}
	_, cmd = m.Update(pubsub.Event[message.Message]{
		Type: pubsub.UpdatedEvent,
		Payload: message.Message{
			ID: "asst-1", SessionID: "s1", Role: message.Assistant,
			Parts: []message.ContentPart{call},
		},
	})
	runCmds(m, cmd)

	item, ok := m.chat.MessageItem("call-1").(chat.ToolMessageItem)
	require.True(t, ok, "the assistant update must create the tool item")
	fin, ok := item.(interface{ Finished() bool })
	require.True(t, ok)
	require.True(t, fin.Finished(),
		"the early result must attach to the new item — without it the item renders 'Waiting for tool response...' forever")
}
