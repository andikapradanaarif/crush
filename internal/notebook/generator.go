package notebook

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"charm.land/fantasy"
)

//go:embed notebook_entry.md
var notebookEntryPrompt []byte

//go:embed checkpoint_entry.md
var checkpointEntryPrompt []byte

//go:embed turn_digest_entry.md
var turnDigestPrompt []byte

// llmGenerator implements the Generator interface using a small LLM
// model to produce structured notebook entries.
type llmGenerator struct {
	resolveModel   func() fantasy.LanguageModel
	maxEntryTokens int64
	// workDir makes file: tags project-relative; set by NewService
	// from Options.WorkingDir. Empty tags basename-only.
	workDir string
	// onUsage reports each generation call's token usage — the
	// sidecar spend the run's token totals can't see. Nil-safe.
	onUsage func(sessionID string, usage fantasy.Usage)
}

// NewLLMGenerator creates a Generator that uses the given model
// resolver to obtain the small model at generation time. The resolver
// is called lazily so the model can be configured after the notebook
// service is created. onUsage, when non-nil, receives every successful
// generation call's usage for run telemetry.
func NewLLMGenerator(modelResolver func() fantasy.LanguageModel, onUsage func(string, fantasy.Usage)) Generator {
	return &llmGenerator{
		resolveModel:   modelResolver,
		maxEntryTokens: 1000, // Default; overridden by service.
		onUsage:        onUsage,
	}
}

// reportUsage forwards a generation call's usage to the sink.
func (g *llmGenerator) reportUsage(sessionID string, usage fantasy.Usage) {
	if g.onUsage != nil {
		g.onUsage(sessionID, usage)
	}
}

// computeMaxTokens returns a scaled max output token budget for a
// batched generation call. Each event may produce up to
// opts.MaxEntryTokens, so we scale linearly with a 25% buffer for
// markdown overhead and delimiters. Clamped to [2000, 16000].
func computeMaxTokens(numEvents int, maxEntryTokens int64) int64 {
	if numEvents <= 0 {
		numEvents = 1
	}
	tokens := int64(numEvents) * maxEntryTokens * 5 / 4
	if tokens < 2000 {
		return 2000
	}
	if tokens > 16000 {
		return 16000
	}
	return tokens
}

// Generate takes classified events and returns structured entry texts
// via a single batched LLM call.
func (g *llmGenerator) Generate(ctx context.Context, sessionID string, events []EntryInput) ([]GeneratedEntry, error) {
	if len(events) == 0 {
		return nil, nil
	}

	model := g.resolveModel()
	if model == nil {
		// Fallback: create simple entries without LLM.
		var entries []GeneratedEntry
		for _, event := range events {
			entries = append(entries, fallbackEntry(event, g.workDir))
		}
		return entries, nil
	}

	prompt := buildGeneratePrompt(events)

	agent := fantasy.NewAgent(
		model,
		fantasy.WithSystemPrompt(string(notebookEntryPrompt)),
		fantasy.WithMaxOutputTokens(computeMaxTokens(len(events), g.maxEntryTokens)),
	)

	resp, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		Prompt: prompt,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to generate notebook entries: %w", err)
	}
	g.reportUsage(sessionID, resp.TotalUsage)

	text := resp.Response.Content.Text()
	entries, parsed := alignGeneratedEntries(text, events, g.workDir)
	if parsed == 0 {
		slog.Warn("LLM produced no bound notebook entries, using fallbacks",
			"extras", len(entries)-len(events))
	}
	return entries, nil
}

// GenerateCheckpoint produces one consolidated checkpoint entry from
// the rendered input block — committed entry digests plus raw tail
// event descriptions. A nil model falls back to a digest of the input
// head so the position still lands, thin but structured.
func (g *llmGenerator) GenerateCheckpoint(ctx context.Context, sessionID string, input string) (GeneratedEntry, error) {
	model := g.resolveModel()
	if model == nil {
		return GeneratedEntry{
			EventType: EventCheckpoint,
			Title:     "Checkpoint",
			Text:      "## Checkpoint\n\n" + truncate(input, 2000) + "\n",
		}, nil
	}

	agent := fantasy.NewAgent(
		model,
		fantasy.WithSystemPrompt(string(checkpointEntryPrompt)),
		fantasy.WithMaxOutputTokens(g.maxEntryTokens*5/4+500),
	)
	resp, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		Prompt: input,
	})
	if err != nil {
		return GeneratedEntry{}, fmt.Errorf("failed to generate checkpoint: %w", err)
	}
	g.reportUsage(sessionID, resp.TotalUsage)
	text := strings.TrimSpace(resp.Response.Content.Text())
	if text == "" {
		slog.Warn("LLM returned an empty checkpoint, using fallback")
		return GeneratedEntry{
			EventType: EventCheckpoint,
			Title:     "Checkpoint",
			Text:      "## Checkpoint\n\n" + truncate(input, 2000) + "\n",
		}, nil
	}
	return GeneratedEntry{
		EventType: EventCheckpoint,
		Title:     extractTitle(text),
		Text:      text,
		Tags:      extractTags(text),
	}, nil
}

// GenerateDigest produces one turn-granularity digest entry from the
// rendered input block — the classified events of a single finished
// turn. A nil model falls back to a digest of the input head so the
// turn's summary still lands, thin but structured.
func (g *llmGenerator) GenerateDigest(ctx context.Context, sessionID string, input string) (GeneratedEntry, error) {
	model := g.resolveModel()
	if model == nil {
		return GeneratedEntry{
			EventType: EventCheckpoint,
			Title:     "Turn digest",
			Text:      "## Turn digest\n\n" + truncate(input, 2000) + "\n",
		}, nil
	}

	agent := fantasy.NewAgent(
		model,
		fantasy.WithSystemPrompt(string(turnDigestPrompt)),
		fantasy.WithMaxOutputTokens(g.maxEntryTokens*5/4+500),
	)
	resp, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		Prompt: input,
	})
	if err != nil {
		return GeneratedEntry{}, fmt.Errorf("failed to generate turn digest: %w", err)
	}
	g.reportUsage(sessionID, resp.TotalUsage)
	text := strings.TrimSpace(resp.Response.Content.Text())
	if text == "" {
		slog.Warn("LLM returned an empty turn digest, using fallback")
		return GeneratedEntry{
			EventType: EventCheckpoint,
			Title:     "Turn digest",
			Text:      "## Turn digest\n\n" + truncate(input, 2000) + "\n",
		}, nil
	}
	return GeneratedEntry{
		EventType: EventCheckpoint,
		Title:     extractTitle(text),
		Text:      text,
		Tags:      extractTags(text),
	}, nil
}

// buildGeneratePrompt renders the batched entry prompt. Failure events
// additionally carry their ErrorHeadline — the distilled first line
// plus the "Exit code N" tail that describeToolCall's 2000-char cut
// can lose, so a failure entry anchors on a digest guaranteed to
// survive truncation. The "### Event N" echo is the binding contract:
// the parser aligns entries to inputs by marker, not position.
func buildGeneratePrompt(events []EntryInput) string {
	var promptSB strings.Builder
	promptSB.WriteString("Generate a notebook entry for each of the following events. ")
	promptSB.WriteString("Use the exact format from the instructions. ")
	promptSB.WriteString("Head each entry with '### Event N' on its own line, echoing its input's number.\n\n")

	for i, event := range events {
		fmt.Fprintf(&promptSB, "### Event %d\n", i+1)
		promptSB.WriteString("Type: ")
		promptSB.WriteString(event.EventType)
		promptSB.WriteString("\n")
		promptSB.WriteString("Title: ")
		promptSB.WriteString(event.Title)
		promptSB.WriteString("\n")
		promptSB.WriteString("Details:\n")
		promptSB.WriteString(event.Description)
		if event.ErrorHeadline != "" {
			promptSB.WriteString("\nError headline: ")
			promptSB.WriteString(event.ErrorHeadline)
		}
		if event.Verified != "" {
			promptSB.WriteString("\nVerification: ")
			promptSB.WriteString(event.Verified)
		}
		promptSB.WriteString("\n\n")
	}
	return promptSB.String()
}

// eventMarkerRe matches the "### Event N" line the prompt asks the
// model to echo — lenient on heading depth, case, and trailing text
// so "## event 3 — auth fix" still binds. The capture is 1-based,
// the same numbering buildGeneratePrompt stamps on its inputs.
var eventMarkerRe = regexp.MustCompile(`(?i)^#{2,6}\s+Event\s+(\d+)\b`)

// trailingDelimRe matches a markdown break line ('---', '***', '___',
// spaced variants) so a model that mixes the old delimiter
// convention into marked output doesn't leave it dangling in a body.
var trailingDelimRe = regexp.MustCompile(`^\s*[-*_](?:\s*[-*_]){2,}\s*$`)

// markedSection is one parse region: the model's body text plus the
// 0-based input index its "### Event N" header declared (-1 when the
// section carries no marker — the legacy '---' shape).
type markedSection struct {
	idx  int
	body string
}

// splitMarkedSections cuts the model output on "### Event N" marker
// lines when any are present — a marker binds its section to input N
// regardless of position, so merges, splits, and reorderings can't
// cascade into misalignment. Text before the first marker is preamble
// and dropped. A "---" line inside a marked section is body content,
// not a delimiter — only a trailing delimiter line is stripped.
// Returns nil when no markers appear; the caller then uses the
// legacy positional split. Not fence-aware: a literal "### Event N"
// inside a fenced code block still splits — a narrower residual of
// the hazard '---' had.
func splitMarkedSections(text string) []markedSection {
	var sections []markedSection
	var cur strings.Builder
	curIdx := -2 // -2 = preamble, not yet inside a marked section.
	flush := func() {
		body := strings.TrimSpace(cur.String())
		cur.Reset()
		if body != "" && curIdx >= 0 {
			sections = append(sections, markedSection{idx: curIdx, body: body})
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if m := eventMarkerRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			// A marker that can't declare a valid input ('Event 0',
			// overflow) folds forward like a forgotten marker —
			// its body joins the current section instead of
			// vanishing or stealing a slot.
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				flush()
				curIdx = n - 1
			}
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
	}
	flush()
	// Strip a trailing separator line each section's body picked
	// up when the model still emitted delimiters between markers.
	for i := range sections {
		lines := strings.Split(strings.TrimRight(sections[i].body, "\n"), "\n")
		for len(lines) > 0 && trailingDelimRe.MatchString(lines[len(lines)-1]) {
			lines = lines[:len(lines)-1]
		}
		sections[i].body = strings.TrimSpace(strings.Join(lines, "\n"))
	}
	// A section that was ONLY a trailing delimiter is gone now.
	out := sections[:0]
	for _, s := range sections {
		if s.body != "" {
			out = append(out, s)
		}
	}
	return out
}

// fallbackEntry is the deterministic entry for an input the model
// produced nothing for — same shape as the no-model path.
func fallbackEntry(event EntryInput, workDir string) GeneratedEntry {
	return GeneratedEntry{
		EventType: event.EventType,
		Title:     event.Title,
		Text:      fmt.Sprintf("## %s\n\n%s\n", event.Title, truncate(event.Description, 800)),
		Tags:      defaultTagsForEvent(event, workDir),
	}
}

// alignGeneratedEntries parses the model output and returns entries
// ALIGNED to the input order: the entry bound to input i sits at
// position i, inputs the model merged or skipped get the
// deterministic fallback at their own position, and out-of-range,
// duplicate, or overflowing sections append after the aligned run.
// The second return counts model-produced entries (0 = all
// fallbacks). Positional binding is therefore correct by
// construction — callers map aligned[j] to events[j] without
// re-deriving correspondence.
func alignGeneratedEntries(text string, events []EntryInput, workDir string) ([]GeneratedEntry, int) {
	newEntry := func(body string, idx int) GeneratedEntry {
		return GeneratedEntry{
			EventType: events[idx].EventType,
			Title:     extractTitle(body),
			Text:      body,
			Tags:      extractTags(body),
		}
	}
	var sections []markedSection
	if marked := splitMarkedSections(text); len(marked) > 0 {
		sections = marked
	} else {
		// No markers at all — the model ignored the echo contract.
		// Legacy shape: '---' splits, every section unmarked.
		for _, section := range strings.Split(text, "\n---\n") {
			if s := strings.TrimSpace(section); s != "" {
				sections = append(sections, markedSection{idx: -1, body: s})
			}
		}
	}
	bound := make([]GeneratedEntry, len(events))
	taken := make([]bool, len(events))
	var extras []GeneratedEntry
	parsed := 0
	for _, s := range sections {
		if s.idx >= 0 {
			switch {
			case s.idx < len(events) && !taken[s.idx]:
				bound[s.idx] = newEntry(s.body, s.idx)
				taken[s.idx] = true
				parsed++
			default:
				// Out-of-range or a duplicate marker — keep as an
				// extra rather than positional-filling a wrong slot.
				extras = append(extras, GeneratedEntry{
					EventType: EventGeneral,
					Title:     extractTitle(s.body),
					Text:      s.body,
					Tags:      extractTags(s.body),
				})
			}
			continue
		}
		// Unmarked section (legacy '---' path): fill the first
		// still-unbound slot in order — the old positional contract.
		placed := false
		for i := range events {
			if !taken[i] {
				bound[i] = newEntry(s.body, i)
				taken[i] = true
				parsed++
				placed = true
				break
			}
		}
		if !placed {
			extras = append(extras, GeneratedEntry{
				EventType: EventGeneral,
				Title:     extractTitle(s.body),
				Text:      s.body,
				Tags:      extractTags(s.body),
			})
		}
	}
	entries := make([]GeneratedEntry, 0, len(events)+len(extras))
	for i, e := range bound {
		if !taken[i] {
			e = fallbackEntry(events[i], workDir)
		}
		entries = append(entries, e)
	}
	return append(entries, extras...), parsed
}

// extractTags finds all #tag patterns in the text. A tag is a token
// starting with a single # followed by a non-space character that is
// not itself a # (to avoid matching markdown headings like ## or ###).
// The leading # is stripped before returning so stored tags use a
// consistent format (e.g., "file:auth.go" not "#file:auth.go").
func extractTags(text string) []string {
	var tags []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		for _, f := range strings.Fields(line) {
			if len(f) < 2 || f[0] != '#' || f[1] == '#' {
				continue // Skip markdown headings (##, ###).
			}
			tag := strings.TrimPrefix(f, "#")
			if !seen[tag] {
				seen[tag] = true
				tags = append(tags, tag)
			}
		}
	}
	return tags
}

// extractTitle extracts the title from a markdown heading.
func extractTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			return strings.TrimPrefix(line, "## ")
		}
	}
	return "Entry"
}

// defaultTagsForEvent returns default tags for an event type.
func defaultTagsForEvent(event EntryInput, workDir string) []string {
	tags := []string{"phase:" + event.EventType}
	if event.ToolCall != nil {
		path := extractPathFromInput(event.ToolCall.Input)
		if path != "" {
			tags = append(tags, fileTags(path, workDir)...)
		}
	}
	return tags
}
