package notebook

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// significantReadThreshold is the minimum output size (in bytes) for a
// file read to be considered significant. Reads below this threshold
// are grouped into a trivial exploration mini-entry.
const significantReadThreshold = 1000

// isSignificant returns true if a tool call warrants its own notebook
// entry.
func isSignificant(toolCall message.ToolCall, toolResult *message.ToolResult) bool {
	switch toolCall.Name {
	case "view", "read":
		if toolResult != nil {
			return len(toolResult.Content) > significantReadThreshold
		}
		return false
	case "edit", "write", "multiedit":
		return true
	case "bash":
		return true
	case "grep", "glob", "ls":
		return false
	default:
		return true
	}
}

// classifyAll builds one EntryInput per finished tool call in the
// messages, in chronological order. Each event's trivial flag records
// which bucket classifyEvents would place it in — callers needing
// merge order (the turn digest) consume this directly.
func classifyAll(msgs []message.Message) []EntryInput {
	var events []EntryInput
	for _, msg := range msgs {
		if msg.Role != message.Assistant {
			continue
		}
		for _, tc := range msg.ToolCalls() {
			if !tc.Finished {
				continue
			}
			result := findToolResult(msgs, tc.ID)
			input := EntryInput{
				ToolCall:   &tc,
				ToolResult: result,
				// A missing result means the call never completed —
				// the session was interrupted between the call and
				// its result. Unknown is not success: a write that
				// may never have run must not supersede reads.
				Succeeded: result != nil && !result.IsError,
			}
			if result != nil && result.IsError {
				input.ErrorHeadline = errorHeadline(result.Content)
			}
			input.Verified = verificationState(tc.Name, result)
			input.EventType = eventTypeForTool(tc.Name)
			// A plan write that never landed — errored or interrupted —
			// is not plan state; route it to general generation so the
			// failure stays visible without corrupting plan history.
			if input.EventType == EventPlan && (result == nil || result.IsError) {
				input.EventType = EventGeneral
			}
			input.Title = titleForTool(tc)
			input.Description = describeToolCall(tc, result)
			input.trivial = !isSignificant(tc, result)
			events = append(events, input)
		}
	}
	return events
}

// classifyEvents partitions the tool calls in a turn's messages into
// significant events (each gets its own entry) and trivial events
// (grouped into one exploration mini-entry).
func classifyEvents(msgs []message.Message) (significant []EntryInput, trivial []EntryInput) {
	for _, input := range classifyAll(msgs) {
		if input.trivial {
			trivial = append(trivial, input)
		} else {
			significant = append(significant, input)
		}
	}
	return significant, trivial
}

// findToolResult searches the tool messages for a result matching the
// given tool call ID.
func findToolResult(msgs []message.Message, toolCallID string) *message.ToolResult {
	for _, msg := range msgs {
		if msg.Role != message.Tool {
			continue
		}
		for _, tr := range msg.ToolResults() {
			if tr.ToolCallID == toolCallID {
				result := tr // Copy to avoid pointer to range variable.
				return &result
			}
		}
	}
	return nil
}

// eventTypeForTool maps a tool name to a notebook event type.
func eventTypeForTool(name string) string {
	switch name {
	case "view", "read":
		return EventFileRead
	case "edit", "write", "multiedit":
		return EventFileEdit
	case "bash":
		return EventCommand
	case "grep", "glob", "ls":
		return EventExploration
	case "todos":
		return EventPlan
	default:
		return EventGeneral
	}
}

// titleForTool generates a short title for a tool call.
func titleForTool(tc message.ToolCall) string {
	switch tc.Name {
	case "view", "read":
		path := extractPathFromInput(tc.Input)
		if path != "" {
			return "Read " + path
		}
		return "Read file"
	case "edit":
		path := extractPathFromInput(tc.Input)
		if path != "" {
			return "Edit " + path
		}
		return "Edit file"
	case "write":
		path := extractPathFromInput(tc.Input)
		if path != "" {
			return "Write " + path
		}
		return "Write file"
	case "multiedit":
		path := extractPathFromInput(tc.Input)
		if path != "" {
			return "Multi-edit " + path
		}
		return "Multi-edit file"
	case "bash":
		cmd := extractCommandFromInput(tc.Input)
		if cmd != "" {
			return "Run: " + truncate(cmd, 60)
		}
		return "Run command"
	case "grep":
		return "Grep search"
	case "glob":
		return "Glob search"
	case "ls":
		return "List directory"
	default:
		return tc.Name
	}
}

// describeToolCall produces a human-readable description of a tool call
// and its result for the generator.
func describeToolCall(tc message.ToolCall, result *message.ToolResult) string {
	var sb strings.Builder
	sb.WriteString("Tool: ")
	sb.WriteString(tc.Name)
	sb.WriteString("\nInput: ")
	sb.WriteString(truncate(tc.Input, 2000))
	if result != nil {
		sb.WriteString("\nResult: ")
		if result.IsError {
			sb.WriteString("[ERROR] ")
		}
		sb.WriteString(truncate(result.Content, 2000))
	}
	return sb.String()
}

// fileTags returns the file: tags for path: the project-relative path
// as the primary, unambiguous tag, plus the basename as an alias so
// recall("file:name.go") and basename-keyed consumers keep matching.
// An absolute path outside workDir — or any path when workDir is
// empty — falls back to the basename alone.
func fileTags(path, workDir string) []string {
	p := strings.TrimRight(path, "/\\")
	if p == "" {
		return nil
	}
	rel := filepath.ToSlash(filepath.Clean(p))
	if filepath.IsAbs(p) {
		if r, err := filepath.Rel(workDir, p); workDir != "" && err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			rel = filepath.ToSlash(r)
		} else {
			rel = filepath.Base(p)
		}
	}
	if rel == "." {
		rel = filepath.Base(p)
	}
	base := filepath.Base(p)
	if rel == base {
		return []string{"file:" + rel}
	}
	return []string{"file:" + rel, "file:" + base}
}

// extractPathFromInput attempts to extract a file path from a tool
// call's JSON input.
func extractPathFromInput(input string) string {
	// Simple JSON field extraction for common path keys.
	for _, key := range []string{`"file_path"`, `"path"`, `"file"`} {
		if val := extractJSONString(input, key); val != "" {
			return val
		}
	}
	return ""
}

// extractCommandFromInput attempts to extract a command from a bash
// tool call's JSON input.
func extractCommandFromInput(input string) string {
	return extractJSONString(input, `"command"`)
}

// extractJSONString extracts a string value for a JSON key from a
// JSON input string. This is a lightweight parser that avoids full JSON
// unmarshalling for simple flat objects.
func extractJSONString(input, key string) string {
	idx := strings.Index(input, key)
	if idx == -1 {
		return ""
	}
	// Move past the key and the colon.
	rest := input[idx+len(key):]
	rest = strings.TrimLeft(rest, " \t:")
	// Find the opening quote.
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	rest = rest[1:]
	// Find the closing quote (handle escaped quotes).
	var sb strings.Builder
	for i := 0; i < len(rest); i++ {
		if rest[i] == '\\' && i+1 < len(rest) {
			sb.WriteByte(rest[i+1])
			i++
			continue
		}
		if rest[i] == '"' {
			break
		}
		sb.WriteByte(rest[i])
	}
	return sb.String()
}

// truncate clips a string to maxLen characters, appending an ellipsis.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}

// hasDecision checks if the assistant's text response contains a
// decision (heuristic: contains keywords like "decided", "chose",
// "will defer", "let's go with").
func hasDecision(msgs []message.Message) bool {
	keywords := []string{"decided", "chose", "will defer", "let's go with", "going with", "opted for"}
	for _, msg := range msgs {
		if msg.Role != message.Assistant {
			continue
		}
		text := strings.ToLower(msg.Content().Text)
		for _, kw := range keywords {
			if strings.Contains(text, kw) {
				return true
			}
		}
	}
	return false
}

// buildTrivialExplorationEntry creates a mini-entry for grouped trivial
// tool calls (grep, glob, ls).
func buildTrivialExplorationEntry(trivial []EntryInput) GeneratedEntry {
	var sb strings.Builder
	sb.WriteString("## Trivial exploration\n")
	for _, t := range trivial {
		if t.ToolCall != nil {
			sb.WriteString("- ")
			sb.WriteString(t.ToolCall.Name)
			if t.ToolResult != nil && t.ToolResult.Content != "" {
				sb.WriteString(" → ")
				sb.WriteString(truncate(t.ToolResult.Content, 100))
			}
			sb.WriteString("\n")
		}
	}
	sb.WriteString("#phase:exploration")
	return GeneratedEntry{
		EventType: EventExploration,
		Title:     "Trivial exploration",
		Text:      sb.String(),
		Tags:      []string{"phase:exploration"},
	}
}

// planItemsFromResult recovers the landed plan list from a todos
// result's metadata — the post-validation list with minted ids.
func planItemsFromResult(result *message.ToolResult) []session.PlanItem {
	if result == nil || result.Metadata == "" {
		return nil
	}
	var items []session.PlanItem
	if raw := gjson.Get(result.Metadata, "todos"); raw.Exists() {
		_ = json.Unmarshal([]byte(raw.Raw), &items)
	}
	return items
}

// planItemsFromCall recovers the submitted list from the call input —
// the fallback for results that predate response metadata. The
// depends_on values here are item keys, not minted ids.
func planItemsFromCall(tc *message.ToolCall) []session.PlanItem {
	if tc == nil {
		return nil
	}
	var items []session.PlanItem
	if raw := gjson.Get(tc.Input, "todos"); raw.Exists() {
		_ = json.Unmarshal([]byte(raw.Raw), &items)
	}
	return items
}

// buildPlanEntry renders a plan write deterministically — the landed
// item list is already structured, so a generator paraphrase would only
// lose information. Dependency edges render as item keys when the id
// resolves inside the list.
func buildPlanEntry(input EntryInput, workDir string) GeneratedEntry {
	items := planItemsFromResult(input.ToolResult)
	if len(items) == 0 {
		items = planItemsFromCall(input.ToolCall)
	}
	keyByID := session.PlanKeyByID(items)
	var pending, inProgress, completed int
	for _, it := range items {
		switch it.Status {
		case session.PlanItemPending:
			pending++
		case session.PlanItemInProgress:
			inProgress++
		case session.PlanItemCompleted:
			completed++
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Plan update: %d item(s) — %d pending, %d in progress, %d completed\n",
		len(items), pending, inProgress, completed)
	tags := []string{"plan"}
	for _, it := range items {
		sb.WriteString(session.FormatPlanItemLine(it, keyByID) + "\n")
		for _, p := range it.EvidencePaths {
			for _, tag := range fileTags(p, workDir) {
				if !slices.Contains(tags, tag) {
					tags = append(tags, tag)
				}
			}
		}
	}
	for _, tag := range tags {
		sb.WriteString("#" + tag + " ")
	}
	return GeneratedEntry{
		EventType: EventPlan,
		Title:     "Plan update",
		Text:      strings.TrimSpace(sb.String()),
		Tags:      tags,
	}
}

// significantEntries produces one entry per significant input: plan
// events render deterministically (the structured list IS the entry),
// everything else goes through the generator. Returns entries aligned
// with significant plus any generator extras appended — the same
// contract generator.Generate used to satisfy directly.
func (s *service) significantEntries(ctx context.Context, sessionID string, significant []EntryInput) ([]GeneratedEntry, error) {
	llmIdx := make([]int, 0, len(significant))
	var llmInputs []EntryInput
	for i, in := range significant {
		if in.EventType == EventPlan {
			continue
		}
		llmIdx = append(llmIdx, i)
		llmInputs = append(llmInputs, in)
	}
	var generated []GeneratedEntry
	if len(llmInputs) > 0 && s.shouldGenerate(sessionID) {
		var err error
		generated, err = s.generator.Generate(ctx, sessionID, llmInputs)
		if err != nil {
			return nil, err
		}
	}
	genAt := make(map[int]GeneratedEntry, len(generated))
	var extras []GeneratedEntry
	for j, e := range generated {
		if j < len(llmIdx) {
			genAt[llmIdx[j]] = e
		} else {
			extras = append(extras, e)
		}
	}
	entries := make([]GeneratedEntry, 0, len(significant)+len(extras))
	for i, in := range significant {
		if in.EventType == EventPlan {
			entries = append(entries, buildPlanEntry(in, s.opts.WorkingDir))
			continue
		}
		entry, ok := genAt[i]
		if !ok {
			// The generator under-produced (merged inputs into one
			// entry) — store a deterministic fallback rather than a
			// zero-value entry, matching the no-model path.
			entry = GeneratedEntry{
				EventType: in.EventType,
				Title:     in.Title,
				Text:      fmt.Sprintf("## %s\n\n%s\n", in.Title, truncate(in.Description, 800)),
				Tags:      defaultTagsForEvent(in, s.opts.WorkingDir),
			}
		}
		entries = append(entries, entry)
	}
	return append(entries, extras...), nil
}

// errorHeadline extracts a one-line digest from a failed tool result:
// the first non-empty line plus the trailing exit code when the result
// carries one (failed bash output ends with "Exit code N"). It
// survives entry compaction so later turns can compare repeated
// failures.
func errorHeadline(content string) string {
	var first, exitCode string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if first == "" {
			first = line
		}
		if strings.HasPrefix(line, "Exit code") {
			exitCode = line
		}
	}
	if exitCode != "" && exitCode != first {
		// Keep the exit code intact; trim the first line so the
		// combined digest still fits.
		limit := 200 - len(exitCode) - 3
		if limit > 0 && len(first) > limit {
			first = first[:limit-1] + "…"
		}
		return first + " — " + exitCode
	}
	if len(first) > 200 {
		first = first[:199] + "…"
	}
	return first
}

// isMutationTool reports whether a tool mutates files — the set the
// verifyingTool decorator covers and the only events that carry
// verification state. Keep it in sync with writeToolNames.
func isMutationTool(name string) bool {
	switch name {
	case "edit", "write", "multiedit", "lsp_rename", "lsp_replace_symbol":
		return true
	}
	return false
}

// Entry-level verification states. The check-level states on tool
// metadata are passed/failed/pending/unverified; an entry aggregates
// them into verified/unverified/failed.
const (
	entryVerified   = "verified"
	entryUnverified = "unverified"
	entryFailed     = "failed"
)

// verificationState aggregates a tool result's "verification" metadata
// into an entry-level state: worst wins over the check list (failed >
// unverified > verified), and pending maps to unverified — a leftover
// pending means the gate never ran, which is not a pass. Returns ""
// for non-mutation tools and missing results, and "unverified" for a
// mutation result carrying no verification data.
func verificationState(name string, result *message.ToolResult) string {
	if !isMutationTool(name) || result == nil {
		return ""
	}
	var checks []message.VerificationCheck
	if raw := gjson.Get(result.Metadata, "verification"); raw.Exists() {
		_ = json.Unmarshal([]byte(raw.Raw), &checks)
	}
	if len(checks) == 0 {
		return entryUnverified
	}
	state := entryVerified
	for _, c := range checks {
		switch c.State {
		case message.VerificationFailed:
			return entryFailed
		case message.VerificationPending, message.VerificationUnverified:
			state = entryUnverified
		}
	}
	return state
}

// verificationTag maps an entry-level verification state to its
// structural tag. Empty state means the event is not a mutation — no
// tag.
func verificationTag(verified string) string {
	switch verified {
	case entryVerified:
		return "verified"
	case entryFailed:
		return "verification-failed"
	case entryUnverified:
		return "unverified"
	}
	return ""
}

// applyVerificationTag injects the verification tag structurally —
// the outcome claim comes from the field, not from generated prose, so
// a failed-verification entry carries verification-failed regardless
// of what the generator wrote.
func applyVerificationTag(entry *GeneratedEntry, verified string) {
	tag := verificationTag(verified)
	if tag == "" || slices.Contains(entry.Tags, tag) {
		return
	}
	entry.Tags = append(entry.Tags, tag)
}

// storeEntry persists a generated entry to the database. succeeded is
// the success flag of the originating event; entries not backed by a
// tool result are stored as succeeded. headline is the failure digest
// for failed tool events. verified is the entry-level verification
// state ("" when the event is not a mutation).
func storeEntry(ctx context.Context, q *db.Queries, sessionID string, turnNumber, segmentNumber, eventNumber int64, entry GeneratedEntry, succeeded bool, headline string, verified string) error {
	tokenCount := estimateTokens(entry.Text)
	id := uuid.New().String()
	now := time.Now().Unix()

	_, err := q.CreateNotebookEntry(ctx, db.CreateNotebookEntryParams{
		ID:               id,
		SessionID:        sessionID,
		TurnNumber:       turnNumber,
		SegmentNumber:    segmentNumber,
		EventNumber:      eventNumber,
		EventType:        entry.EventType,
		Title:            entry.Title,
		EntryText:        entry.Text,
		EntryTextFull:    sql.NullString{String: entry.Text, Valid: true},
		TokenCount:       tokenCount,
		CompressionLevel: CompressionFull,
		Succeeded:        boolToInt64(succeeded),
		ErrorHeadline:    headline,
		Verified:         verified,
		CreatedAt:        now,
	})
	if err != nil {
		return fmt.Errorf("failed to create notebook entry: %w", err)
	}

	for _, tag := range entry.Tags {
		if err := q.CreateNotebookTag(ctx, db.CreateNotebookTagParams{
			EntryID: id,
			Tag:     tag,
		}); err != nil {
			slog.Error("Failed to create notebook tag", "error", err, "tag", tag)
		}
	}
	return nil
}

// estimateTokens provides a rough token estimate (~4 chars per token).
func estimateTokens(text string) int64 {
	return int64(len(text) / 4)
}

// eventsSucceeded reports whether every classified event succeeded.
func eventsSucceeded(events []EntryInput) bool {
	for _, e := range events {
		if !e.Succeeded {
			return false
		}
	}
	return true
}

func boolToInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// GenerateEntries implements the Service interface.
func (s *service) GenerateEntries(ctx context.Context, sessionID string, turnNumber int64, msgs []message.Message) error {
	significant, trivial := classifyEvents(msgs)

	// Check for a decision in the assistant response. Decisions are
	// significant events in their own right, alongside file edits/reads.
	// A decision entry is added even when there are other significant
	// events, because the decision context is distinct from the tool
	// events.
	if hasDecision(msgs) {
		significant = append(significant, EntryInput{
			EventType:   EventDecision,
			Title:       "Decision",
			Description: extractAssistantText(msgs),
			Succeeded:   true,
		})
	}

	// If there are no significant events, no trivial events, and no
	// decision, this is a trivial turn — skip entirely.
	if len(significant) == 0 && len(trivial) == 0 {
		return nil
	}

	// Store trivial exploration mini-entry.
	if len(trivial) > 0 {
		entry := buildTrivialExplorationEntry(trivial)
		if err := storeEntry(ctx, s.q, sessionID, turnNumber, 0, 0, entry, eventsSucceeded(trivial), "", ""); err != nil {
			slog.Error("Failed to store trivial exploration entry", "error", err)
		}
	}

	// Generate significant entries — plan events render
	// deterministically, everything else via the small model.
	if len(significant) == 0 {
		return nil
	}

	entries, err := s.significantEntries(ctx, sessionID, significant)
	if err != nil {
		return fmt.Errorf("failed to generate notebook entries: %w", err)
	}

	for i, entry := range entries {
		var verified string
		if i < len(significant) {
			verified = significant[i].Verified
		}
		applyVerificationTag(&entry, verified)
		// Enforce max entry token cap via truncation.
		if estimateTokens(entry.Text) > s.opts.MaxEntryTokens {
			entry.Text = truncateEntry(entry.Text, s.opts.MaxEntryTokens)
		}
		// Entries are index-aligned with significant events; entries
		// beyond the input list are generator extras and default to
		// succeeded.
		succeeded := i >= len(significant) || significant[i].Succeeded
		var headline string
		if i < len(significant) {
			headline = significant[i].ErrorHeadline
		}
		if err := storeEntry(ctx, s.q, sessionID, turnNumber, 0, int64(i+1), entry, succeeded, headline, verified); err != nil {
			slog.Error("Failed to store notebook entry", "error", err)
		}
	}

	// Compact if needed.
	if err := s.Compact(ctx, sessionID); err != nil {
		slog.Error("Failed to compact notebook", "error", err)
	}

	return nil
}

// EntryTruncatedMarker is the line truncateEntry appends where it cut
// generated text. It names no tool: recovery pointers are a
// render-time concern gated on the prompted agent's tool set, and a
// stored pointer to a tool the agent lacks is dead text. The marker
// stays tool-neutral for a second reason — entry_text_full holds the
// same truncated body, so recall has no fuller text to return and a
// "use recall for full details" pointer overpromises even when the
// tool exists.
const EntryTruncatedMarker = "[Entry truncated.]"

// LegacyEntryTruncatedMarker is the marker entries stored before it
// stopped naming recall. Hydration rewrites it to EntryTruncatedMarker
// unconditionally — the pointer overpromises for every agent since
// entry_text_full holds the same truncated body.
const LegacyEntryTruncatedMarker = "[Entry truncated. Use recall tool for full details.]"

// RewriteTruncationMarker normalizes stored entry text at hydration:
// entries written while the marker named recall carry
// LegacyEntryTruncatedMarker, and no consumer should ship it —
// entry_text_full holds the same truncated body, so the pointer
// overpromises regardless of which tools the agent has. Applied in
// enrichEntries so prompt renders, auto-inject, recall output,
// checkpoint input, and compaction all see clean text from one choke
// point.
func RewriteTruncationMarker(text string) string {
	return strings.ReplaceAll(text, LegacyEntryTruncatedMarker, EntryTruncatedMarker)
}

// truncateEntry clips an entry to the max token budget, preserving
// tags at the bottom.
func truncateEntry(text string, maxTokens int64) string {
	maxChars := int(maxTokens * 4)
	if len(text) <= maxChars {
		return text
	}
	// Try to preserve the last few lines (tags section).
	lines := strings.Split(text, "\n")
	var tagLines []string
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			tagLines = append([]string{lines[i]}, tagLines...)
			continue
		}
		break
	}
	body := strings.Join(lines[:len(lines)-len(tagLines)], "\n")
	// The tag block plus marker can exceed the budget on pathological
	// entries (huge or all-tag generated text) — clamp the cut rather
	// than slice out of range.
	keep := maxChars - len(strings.Join(tagLines, "\n")) - len(EntryTruncatedMarker) - 2
	body = body[:min(max(keep, 0), len(body))]
	return body + "\n" + EntryTruncatedMarker + "\n" + strings.Join(tagLines, "\n")
}

// extractAssistantText returns the concatenated text of all assistant
// messages in the turn.
func extractAssistantText(msgs []message.Message) string {
	var sb strings.Builder
	for _, msg := range msgs {
		if msg.Role == message.Assistant {
			sb.WriteString(msg.Content().Text)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}
