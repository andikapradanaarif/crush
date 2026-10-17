package message

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"github.com/charmbracelet/crush/internal/stringext"
	"github.com/charmbracelet/x/ansi"
)

type MessageRole string

const (
	Assistant MessageRole = "assistant"
	User      MessageRole = "user"
	System    MessageRole = "system"
	Tool      MessageRole = "tool"
)

// mediaLoadFailedPlaceholder is the text substituted for image data that
// cannot be decoded during session replay.
const mediaLoadFailedPlaceholder = "[Image data could not be loaded]"

type FinishReason string

const (
	FinishReasonEndTurn   FinishReason = "end_turn"
	FinishReasonMaxTokens FinishReason = "max_tokens"
	FinishReasonToolUse   FinishReason = "tool_use"
	FinishReasonCanceled  FinishReason = "canceled"
	FinishReasonError     FinishReason = "error"
	// FinishReasonContentFilter is a provider safety/refusal stop
	// (Anthropic stop_reason=refusal, OpenAI content_filter, etc.).
	// The TUI renders this as a REFUSED banner rather than a silent
	// empty turn.
	FinishReasonContentFilter FinishReason = "content_filter"

	// Should never happen
	FinishReasonUnknown FinishReason = "unknown"
)

type ContentPart interface {
	isPart()
}

type ReasoningContent struct {
	// ID is the stream's reasoning block id — fantasy emits one per
	// reasoning block, so it keys deltas and end-of-block signatures to
	// the right part on interleaved multi-block streams.
	ID               string                             `json:"id,omitempty"`
	Thinking         string                             `json:"thinking"`
	Signature        string                             `json:"signature"`
	ThoughtSignature string                             `json:"thought_signature"` // Used for google
	ToolID           string                             `json:"tool_id"`           // Used for openrouter google models
	ResponsesData    *openai.ResponsesReasoningMetadata `json:"responses_data"`
	StartedAt        int64                              `json:"started_at,omitempty"`
	FinishedAt       int64                              `json:"finished_at,omitempty"`
}

func (tc ReasoningContent) String() string {
	return tc.Thinking
}
func (ReasoningContent) isPart() {}

type TextContent struct {
	Text string `json:"text"`
	// Hidden marks generated user continuations that remain in model history.
	Hidden bool `json:"hidden,omitempty"`
}

func (tc TextContent) String() string {
	return tc.Text
}

func (TextContent) isPart() {}

type ImageURLContent struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

func (iuc ImageURLContent) String() string {
	return iuc.URL
}

func (ImageURLContent) isPart() {}

type BinaryContent struct {
	Path     string
	MIMEType string
	Data     []byte
}

func (bc BinaryContent) String(p catwalk.InferenceProvider) string {
	base64Encoded := base64.StdEncoding.EncodeToString(bc.Data)
	if p == catwalk.InferenceProviderOpenAI {
		return "data:" + bc.MIMEType + ";base64," + base64Encoded
	}
	return base64Encoded
}

func (BinaryContent) isPart() {}

type ToolCall struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Input            string `json:"input"`
	ProviderExecuted bool   `json:"provider_executed"`
	Finished         bool   `json:"finished"`
}

func (ToolCall) isPart() {}

type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Content    string `json:"content"`
	Data       string `json:"data"`
	MIMEType   string `json:"mime_type"`
	Metadata   string `json:"metadata"`
	IsError    bool   `json:"is_error"`
	// FileMtime is the file's modification time (UnixNano) observed
	// when a read result was produced — zero when unrecorded (older
	// results, non-file reads). It lets the flagger supersede on
	// observed modification rather than trusting which tool claims
	// to write.
	FileMtime int64 `json:"file_mtime,omitempty"`
	// Superseded marks this result as stale: a later successful write
	// changed the file it describes. The mark is metadata only —
	// Content is never rewritten — so notebook generation and recall
	// still see the original. Prompt assembly renders a stub once
	// Applied is set.
	Superseded *SupersededMark `json:"superseded,omitempty"`
}

// StubKind classifies why a tool result was marked for stubbing, so
// the rendered placeholder can state the right reason and recovery
// path.
type StubKind string

const (
	// StubKindSuperseded is a file-read result invalidated by a
	// successful write tool. It is the zero value so marks written
	// before kinds existed keep their meaning.
	StubKindSuperseded StubKind = ""
	// StubKindModified is a read whose file changed on disk after the
	// read with no write tool to attribute — bash redirection, an MCP
	// file write, an external editor.
	StubKindModified StubKind = "modified"
	// StubKindDeleted is a read whose file no longer exists.
	StubKindDeleted StubKind = "deleted"
	// StubKindDuplicate is output identical to a later re-run; the
	// newest copy stays verbatim.
	StubKindDuplicate StubKind = "duplicate"
	// StubKindRerun is output that differs from a later re-run. The
	// difference is evidence of delta (fail→pass means the fix
	// worked), so the earlier result degrades to a digest carrying
	// its first line, not a bare stub.
	StubKindRerun StubKind = "rerun"
	// StubKindStale is re-derivable command output old enough to
	// collapse; the head-prefix kept in the stub is the digest.
	StubKindStale StubKind = "stale"
)

// String returns the kind's telemetry label. StubKindSuperseded is the
// empty string so marks written before kinds existed keep their
// meaning — wherever kinds become map keys it must surface as
// "superseded", never "".
func (k StubKind) String() string {
	if k == StubKindSuperseded {
		return "superseded"
	}
	return string(k)
}

// StubKinds enumerates every declared stub kind — the canonical list
// for places that must cover all of them (eval coverage fields,
// tests), so a new kind can't silently lack a telemetry field.
func StubKinds() []StubKind {
	return []StubKind{
		StubKindSuperseded,
		StubKindModified,
		StubKindDeleted,
		StubKindDuplicate,
		StubKindRerun,
		StubKindStale,
	}
}

// SupersededMark records why a tool result's stored content no longer
// needs to replay verbatim in the raw window.
type SupersededMark struct {
	Path    string   `json:"path"`    // File whose newer state superseded this result.
	ByTool  string   `json:"by_tool"` // edit, write, or multiedit.
	Turn    int64    `json:"turn"`    // Turn containing the superseding event.
	Applied bool     `json:"applied"` // True once the result renders as a stub.
	Kind    StubKind `json:"kind,omitempty"`
	// Bytes is the result's total content length at flag time — the
	// stub label reports it and stats recompute exact savings from it
	// without fetching full content.
	Bytes int64 `json:"bytes,omitempty"`
}

// stubHeadPrefixBytes caps the head-prefix kept inside stale-result
// stubs — the prefix is the digest the model keeps.
const stubHeadPrefixBytes = 320

// stubHeadLineBytes caps the first-line excerpt kept in rerun digests.
const stubHeadLineBytes = 160

// StubText renders the placeholder a stubbed tool result shows in the
// raw window. Every stub self-labels what was cut, why, and how to
// recover — recall("result:<tool_call_id>") resolves to the stored
// original, which is never rewritten.
func (m SupersededMark) StubText(tr ToolResult) string {
	recall := fmt.Sprintf(`recall("result:%s")`, tr.ToolCallID)
	switch m.Kind {
	case StubKindModified:
		return fmt.Sprintf("[content of %s is stale — the file changed on disk after this read (turn %d); re-view for current state, or %s for the snapshot]",
			m.Path, m.Turn, recall)
	case StubKindDeleted:
		return fmt.Sprintf("[%s was deleted after this read (turn %d); %s for the last snapshot]",
			m.Path, m.Turn, recall)
	case StubKindDuplicate:
		return fmt.Sprintf("[identical %s output superseded by a later run at turn %d; %s for this copy]",
			tr.Name, m.Turn, recall)
	case StubKindRerun:
		return fmt.Sprintf("[earlier %s output differed from a re-run at turn %d — it began %q; %s for the full output]",
			tr.Name, m.Turn, headLine(tr.Content, stubHeadLineBytes), recall)
	case StubKindStale:
		total := m.Bytes
		if total == 0 {
			total = int64(len(tr.Content))
		}
		return fmt.Sprintf("%s\n[rest of %s output elided (%d bytes total, turn %d); %s or re-run for the rest]",
			headPrefix(tr.Content, stubHeadPrefixBytes), tr.Name, total, m.Turn, recall)
	default:
		return fmt.Sprintf("[content of %s superseded by %s at turn %d; re-view for current state, or %s for the pre-edit snapshot]",
			m.Path, m.ByTool, m.Turn, recall)
	}
}

// headPrefix returns the first n bytes of s, preferring a line
// boundary inside the last quarter and never splitting a rune or an
// ANSI escape sequence.
func headPrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	if i := strings.LastIndexByte(s[:cut], '\n'); i >= n*3/4 {
		cut = i + 1
	}
	return s[:stringext.CutANSISafeLeft(s, cut)]
}

// headLine returns the first line of s, capped at n bytes on a rune
// and ANSI-escape boundary.
func headLine(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:stringext.CutANSISafeLeft(s, n)] + "…"
	}
	return s
}

func (ToolResult) isPart() {}

type Finish struct {
	Reason  FinishReason `json:"reason"`
	Time    int64        `json:"time"`
	Message string       `json:"message,omitempty"`
	Details string       `json:"details,omitempty"`
}

func (Finish) isPart() {}

// ShellCommand stores a bang-mode shell command and its output as a
// distinct content part so it can be reconstructed on session restore.
type ShellCommand struct {
	Command  string `json:"command"`
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

func (ShellCommand) isPart() {}

// HasShellCommand reports whether the message contains any ShellCommand parts.
func (m *Message) HasShellCommand() bool {
	for _, part := range m.Parts {
		if _, ok := part.(ShellCommand); ok {
			return true
		}
	}
	return false
}

// ShellCommands returns all ShellCommand parts from the message.
func (m *Message) ShellCommands() []ShellCommand {
	var cmds []ShellCommand
	for _, part := range m.Parts {
		if sc, ok := part.(ShellCommand); ok {
			cmds = append(cmds, sc)
		}
	}
	return cmds
}

type Message struct {
	ID               string
	Role             MessageRole
	SessionID        string
	Parts            []ContentPart
	Model            string
	Provider         string
	CreatedAt        int64
	UpdatedAt        int64
	IsSummaryMessage bool
	// PrismModelID and PrismModelName identify the model that actually
	// served the turn, as reported by the Hyper Prism model router
	// headers. Empty when the turn was not routed through Prism.
	PrismModelID   string
	PrismModelName string
	// PrismHypercreditSavings and PrismDollarSavings are the savings
	// from routing through Prism, as reported by its savings trailers.
	// Nil when not reported. When both are present the hypercredit
	// figure is the one shown.
	PrismHypercreditSavings *float64
	PrismDollarSavings      *float64
}

func (m *Message) Content() TextContent {
	for _, part := range m.Parts {
		if c, ok := part.(TextContent); ok {
			return c
		}
	}
	return TextContent{}
}

// JoinedText concatenates every text part on the message. Content()
// returns only the first, which hides text — including sentinel markers
// like the plan-ready marker — in later parts when a provider fragments
// its response. Marker detection and plan extraction should use this.
func (m *Message) JoinedText() string {
	var texts []string
	for _, part := range m.Parts {
		if c, ok := part.(TextContent); ok {
			texts = append(texts, c.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func (m *Message) ReasoningContent() ReasoningContent {
	for _, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			return c
		}
	}
	return ReasoningContent{}
}

func (m *Message) ImageURLContent() []ImageURLContent {
	imageURLContents := make([]ImageURLContent, 0)
	for _, part := range m.Parts {
		if c, ok := part.(ImageURLContent); ok {
			imageURLContents = append(imageURLContents, c)
		}
	}
	return imageURLContents
}

func (m *Message) BinaryContent() []BinaryContent {
	binaryContents := make([]BinaryContent, 0)
	for _, part := range m.Parts {
		if c, ok := part.(BinaryContent); ok {
			binaryContents = append(binaryContents, c)
		}
	}
	return binaryContents
}

func (m *Message) ToolCalls() []ToolCall {
	toolCalls := make([]ToolCall, 0)
	for _, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			toolCalls = append(toolCalls, c)
		}
	}
	return toolCalls
}

func (m *Message) ToolResults() []ToolResult {
	toolResults := make([]ToolResult, 0)
	for _, part := range m.Parts {
		if c, ok := part.(ToolResult); ok {
			toolResults = append(toolResults, c)
		}
	}
	return toolResults
}

func (m *Message) IsFinished() bool {
	for _, part := range m.Parts {
		if _, ok := part.(Finish); ok {
			return true
		}
	}
	return false
}

func (m *Message) FinishPart() *Finish {
	for _, part := range m.Parts {
		if c, ok := part.(Finish); ok {
			return &c
		}
	}
	return nil
}

func (m *Message) FinishReason() FinishReason {
	for _, part := range m.Parts {
		if c, ok := part.(Finish); ok {
			return c.Reason
		}
	}
	return ""
}

// IsErrorLike reports whether the message finished with an error-style
// banner (a real error or a provider safety refusal). The TUI renders
// both through the same banner path.
func (m *Message) IsErrorLike() bool {
	switch m.FinishReason() {
	case FinishReasonError, FinishReasonContentFilter:
		return true
	}
	return false
}

func (m *Message) IsThinking() bool {
	if m.ReasoningContent().Thinking != "" && m.Content().Text == "" && !m.IsFinished() {
		return true
	}
	return false
}

func (m *Message) AppendContent(delta string) {
	found := false
	for i, part := range m.Parts {
		if c, ok := part.(TextContent); ok {
			m.Parts[i] = TextContent{Text: c.Text + delta, Hidden: c.Hidden}
			found = true
		}
	}
	if !found {
		m.Parts = append(m.Parts, TextContent{Text: delta})
	}
}

// reasoningPartIndex returns the index of the reasoning part a stream
// event addresses: the part carrying the event's block id when one is
// supplied, else the last still-open reasoning part, else the last
// reasoning part at all. The empty-id fallback keeps id-less streams
// accumulating on a single part — a fresh part there would be invisible
// to ReasoningContent(), which reads the first. Returns -1 when no part
// matches.
func (m *Message) reasoningPartIndex(id string) int {
	if id != "" {
		for i, part := range m.Parts {
			if c, ok := part.(ReasoningContent); ok && c.ID == id {
				return i
			}
		}
		return -1
	}
	last, lastOpen := -1, -1
	for i, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			last = i
			if c.FinishedAt == 0 {
				lastOpen = i
			}
		}
	}
	if lastOpen >= 0 {
		return lastOpen
	}
	return last
}

func (m *Message) AppendReasoningContent(id string, delta string) {
	if i := m.reasoningPartIndex(id); i >= 0 {
		c := m.Parts[i].(ReasoningContent)
		c.Thinking += delta
		m.Parts[i] = c
		return
	}
	m.Parts = append(m.Parts, ReasoningContent{
		ID:        id,
		Thinking:  delta,
		StartedAt: time.Now().Unix(),
	})
}

func (m *Message) AppendThoughtSignature(id string, signature string, toolCallID string) {
	if i := m.reasoningPartIndex(id); i >= 0 {
		c := m.Parts[i].(ReasoningContent)
		c.ThoughtSignature += signature
		c.ToolID = toolCallID
		m.Parts[i] = c
		return
	}
	m.Parts = append(m.Parts, ReasoningContent{
		ID:               id,
		ThoughtSignature: signature,
		ToolID:           toolCallID,
	})
}

func (m *Message) AppendReasoningSignature(id string, signature string) {
	if i := m.reasoningPartIndex(id); i >= 0 {
		c := m.Parts[i].(ReasoningContent)
		c.Signature += signature
		m.Parts[i] = c
		return
	}
	m.Parts = append(m.Parts, ReasoningContent{ID: id, Signature: signature})
}

func (m *Message) SetReasoningResponsesData(id string, data *openai.ResponsesReasoningMetadata) {
	if i := m.reasoningPartIndex(id); i >= 0 {
		c := m.Parts[i].(ReasoningContent)
		c.ResponsesData = data
		m.Parts[i] = c
		return
	}
	m.Parts = append(m.Parts, ReasoningContent{ID: id, ResponsesData: data})
}

func (m *Message) FinishThinking(id string) {
	if i := m.reasoningPartIndex(id); i >= 0 {
		c := m.Parts[i].(ReasoningContent)
		if c.FinishedAt == 0 {
			c.FinishedAt = time.Now().Unix()
			m.Parts[i] = c
		}
	}
}

func (m *Message) ThinkingDuration() time.Duration {
	reasoning := m.ReasoningContent()
	if reasoning.StartedAt == 0 {
		return 0
	}

	endTime := reasoning.FinishedAt
	if endTime == 0 {
		endTime = time.Now().Unix()
	}

	return time.Duration(endTime-reasoning.StartedAt) * time.Second
}

func (m *Message) FinishToolCall(toolCallID string) {
	for i, part := range m.Parts {
		if c, ok := part.(ToolCall); ok && c.ID == toolCallID {
			c.Finished = true
			m.Parts[i] = c
			return
		}
	}
}

func (m *Message) AppendToolCallInput(toolCallID string, inputDelta string) {
	for i, part := range m.Parts {
		if c, ok := part.(ToolCall); ok && c.ID == toolCallID {
			c.Input += inputDelta
			m.Parts[i] = c
			return
		}
	}
}

func (m *Message) AddToolCall(tc ToolCall) {
	for i, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			if c.ID == tc.ID {
				m.Parts[i] = tc
				return
			}
		}
	}
	m.Parts = append(m.Parts, tc)
}

func (m *Message) SetToolCalls(tc []ToolCall) {
	// remove any existing tool call part it could have multiple
	parts := make([]ContentPart, 0)
	for _, part := range m.Parts {
		if _, ok := part.(ToolCall); ok {
			continue
		}
		parts = append(parts, part)
	}
	m.Parts = parts
	for _, toolCall := range tc {
		m.Parts = append(m.Parts, toolCall)
	}
}

func (m *Message) AddToolResult(tr ToolResult) {
	m.Parts = append(m.Parts, tr)
}

func (m *Message) SetToolResults(tr []ToolResult) {
	for _, toolResult := range tr {
		m.Parts = append(m.Parts, toolResult)
	}
}

// Clone returns a deep copy of the message with an independent Parts slice.
// This prevents race conditions when the message is modified concurrently.
func (m *Message) Clone() Message {
	clone := *m
	clone.Parts = make([]ContentPart, len(m.Parts))
	copy(clone.Parts, m.Parts)
	return clone
}

// ResetStreamedContent removes all parts that were added during streaming
// (text, reasoning, tool calls, finish) so the message is ready for a
// retry. Non-streamed parts (images, binary attachments, tool results,
// shell commands) are preserved.
func (m *Message) ResetStreamedContent() {
	kept := m.Parts[:0]
	for _, part := range m.Parts {
		switch part.(type) {
		case TextContent, ReasoningContent, ToolCall, Finish:
			// Drop streamed parts.
		default:
			kept = append(kept, part)
		}
	}
	m.Parts = kept
}

func (m *Message) AddFinish(reason FinishReason, message, details string) {
	// remove any existing finish part
	for i, part := range m.Parts {
		if _, ok := part.(Finish); ok {
			m.Parts = slices.Delete(m.Parts, i, i+1)
			break
		}
	}
	m.Parts = append(m.Parts, Finish{Reason: reason, Time: time.Now().Unix(), Message: message, Details: details})
}

func (m *Message) AddImageURL(url, detail string) {
	m.Parts = append(m.Parts, ImageURLContent{URL: url, Detail: detail})
}

func (m *Message) AddBinary(mimeType string, data []byte) {
	m.Parts = append(m.Parts, BinaryContent{MIMEType: mimeType, Data: data})
}

func PromptWithTextAttachments(prompt string, attachments []Attachment) string {
	var sb strings.Builder
	sb.WriteString(prompt)
	addedAttachments := false
	for _, content := range attachments {
		if !content.IsText() {
			continue
		}
		if !addedAttachments {
			sb.WriteString("\n<system_info>The files below have been attached by the user, consider them in your response</system_info>\n")
			addedAttachments = true
		}
		if content.FilePath != "" {
			fmt.Fprintf(&sb, "<file path='%s'>\n", content.FilePath)
		} else {
			sb.WriteString("<file>\n")
		}
		sb.WriteString("\n")
		sb.Write(content.Content)
		sb.WriteString("\n</file>\n")
	}
	return sb.String()
}

func (m *Message) ToAIMessage() []fantasy.Message {
	var messages []fantasy.Message
	switch m.Role {
	case User:
		var parts []fantasy.MessagePart
		text := strings.TrimSpace(m.Content().Text)
		var textAttachments []Attachment
		for _, content := range m.BinaryContent() {
			if !strings.HasPrefix(content.MIMEType, "text/") {
				continue
			}
			textAttachments = append(textAttachments, Attachment{
				FilePath: content.Path,
				MimeType: content.MIMEType,
				Content:  content.Data,
			})
		}
		text = PromptWithTextAttachments(text, textAttachments)
		// Include bang-mode shell commands as context for the agent.
		for _, sc := range m.ShellCommands() {
			shellText := fmt.Sprintf("$ %s\n%s\n(exit code %d)", sc.Command, ansi.Strip(sc.Output), sc.ExitCode)
			if text != "" {
				text += "\n\n" + shellText
			} else {
				text = shellText
			}
		}
		if text != "" {
			parts = append(parts, fantasy.TextPart{Text: text})
		}
		for _, content := range m.BinaryContent() {
			// skip text attachements
			if strings.HasPrefix(content.MIMEType, "text/") {
				continue
			}
			parts = append(parts, fantasy.FilePart{
				Filename:  content.Path,
				Data:      content.Data,
				MediaType: content.MIMEType,
			})
		}
		messages = append(messages, fantasy.Message{
			Role:    fantasy.MessageRoleUser,
			Content: parts,
		})
	case Assistant:
		var parts []fantasy.MessagePart
		text := strings.TrimSpace(m.Content().Text)
		if text != "" {
			parts = append(parts, fantasy.TextPart{Text: text})
		}
		reasoning := m.ReasoningContent()
		if reasoning.Thinking != "" {
			reasoningPart := fantasy.ReasoningPart{Text: reasoning.Thinking, ProviderOptions: fantasy.ProviderOptions{}}
			if reasoning.Signature != "" {
				reasoningPart.ProviderOptions[anthropic.Name] = &anthropic.ReasoningOptionMetadata{
					Signature: reasoning.Signature,
				}
			}
			if reasoning.ResponsesData != nil {
				reasoningPart.ProviderOptions[openai.Name] = reasoning.ResponsesData
			}
			if reasoning.ThoughtSignature != "" {
				reasoningPart.ProviderOptions[google.Name] = &google.ReasoningMetadata{
					Signature: reasoning.ThoughtSignature,
					ToolID:    reasoning.ToolID,
				}
			}
			parts = append(parts, reasoningPart)
		}
		for _, call := range m.ToolCalls() {
			parts = append(parts, fantasy.ToolCallPart{
				ToolCallID:       call.ID,
				ToolName:         call.Name,
				Input:            call.Input,
				ProviderExecuted: call.ProviderExecuted,
			})
		}
		messages = append(messages, fantasy.Message{
			Role:    fantasy.MessageRoleAssistant,
			Content: parts,
		})
	case Tool:
		var parts []fantasy.MessagePart
		for _, result := range m.ToolResults() {
			var content fantasy.ToolResultOutputContent
			if result.IsError {
				content = fantasy.ToolResultOutputContentError{
					Error: errors.New(result.Content),
				}
			} else if result.Data != "" {
				if stringext.IsValidBase64(result.Data) {
					content = fantasy.ToolResultOutputContentMedia{
						Data:      result.Data,
						MediaType: result.MIMEType,
					}
				} else {
					content = fantasy.ToolResultOutputContentText{
						Text: mediaLoadFailedPlaceholder,
					}
				}
			} else {
				content = fantasy.ToolResultOutputContentText{
					Text: result.Content,
				}
			}
			parts = append(parts, fantasy.ToolResultPart{
				ToolCallID: result.ToolCallID,
				Output:     content,
			})
		}
		messages = append(messages, fantasy.Message{
			Role:    fantasy.MessageRoleTool,
			Content: parts,
		})
	}
	return messages
}
