package agent

import (
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/notebook"
)

// logPromptComposition logs the byte size of each static request
// component. It runs once per Run, where the system prompt, tool list,
// and MCP instructions are assembled. systemPromptBytes measures the
// rendered prompt before MCP instructions are appended.
func logPromptComposition(sessionID string, systemPromptBytes int, mcpInstructions string, agentTools []fantasy.AgentTool, sections []prompt.PromptSection) {
	builtinSchemaBytes, mcpSchemaBytes := toolSchemaBytes(agentTools)
	attrs := []any{
		"session_id", sessionID,
		"system_prompt_bytes", systemPromptBytes,
		"mcp_instructions_bytes", len(mcpInstructions),
		"builtin_tool_schema_bytes", builtinSchemaBytes,
		"mcp_tool_schema_bytes", mcpSchemaBytes,
		"tool_count", len(agentTools),
	}
	for _, s := range sections {
		attrs = append(attrs,
			"section_"+s.Name+"_bytes", s.Bytes,
			"section_"+s.Name+"_est_tokens", s.EstTokens,
		)
	}
	slog.Debug("Prompt composition", attrs...)
}

// requestStats accumulates per-session request-size telemetry: the
// prompt-token growth curve and the last rendered request's byte
// composition — the "does context stay flat" signal the eval
// benefit measurement reads. Shared across agent rebuilds via the
// coordinator's map so a rebuilt agent doesn't lose the curve.
type requestStats struct {
	// Requests counts steps that reported provider usage.
	Requests int64
	// LastPromptTokens is the normalized prompt-token count (input +
	// cache write + cache read) of the most recent request — the
	// growth-curve sample; PeakPromptTokens is the max observed.
	LastPromptTokens int64
	PeakPromptTokens int64
	// Last rendered request's content bytes by component. Tool
	// calls/results split from the rest of history so "how much of
	// the prompt is stale tool output" is a number, not an estimate.
	SystemBytes     int64
	NotebookBytes   int64
	HistoryBytes    int64
	ToolCallBytes   int64
	ToolResultBytes int64
	// Steps is the per-request table the eval harness reads — usage
	// plus the prefix-attribution forensics that name every cache
	// miss's cause. Pending holds the PrepareStep-side attribution
	// for the in-flight step; OnStepFinish folds it into Steps with
	// the request's usage, and the run-error path folds it as a
	// Failed row when the request dies mid-step. PrevHashes is the
	// previous step's per-message content hashes — the diff input.
	// Note it advances at PrepareStep, so after a failed request the
	// next diff compares against a render the provider may never
	// have accepted (see EVAL_HARNESS.md for the caveat).
	Steps      []StepRecord
	Pending    stepAttribution
	PrevHashes []uint64
}

// StepRecord is one provider request's usage plus prefix attribution.
// FirstChanged is the index of the first message whose content differs
// from the previous step's render: len(prev) means pure tail append
// (the cache-friendly case), inside the leading system-message run
// means a prefix rewrite, mid-history means an edit (stub promotion,
// collapse, region replacement). PrefixHash fingerprints the leading
// system-message run — the volatile prefix a provider's prompt cache
// keys on.
type StepRecord struct {
	Step             int   `json:"step"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	Estimated        bool  `json:"estimated,omitempty"`
	// Failed marks the row folded from a request that died before
	// OnStepFinish — its attribution is real but the usage fields
	// are zero (no usage report survives a failed stream).
	Failed            bool   `json:"failed,omitempty"`
	PrefixHash        string `json:"prefix_hash,omitempty"`
	FirstChanged      int    `json:"first_changed_index"`
	FirstChangedCause string `json:"first_changed_cause,omitempty"`
}

// stepAttribution is the PrepareStep-side half of a StepRecord — the
// prompt-side state captured before the request flies.
type stepAttribution struct {
	PrefixHash        string
	FirstChanged      int
	FirstChangedCause string
}

// attributeStep hashes the rendered message list and diffs it against
// the previous step's per-message hashes. prefixHash covers the
// leading system-message run (system prompt + prompt prefix +
// notebook block); firstChanged/cause name where the divergence
// starts. Returns the attribution and the new hash vector.
func attributeStep(messages []fantasy.Message, prev []uint64) (stepAttribution, []uint64) {
	hashes := make([]uint64, len(messages))
	prefixLen := 0
	for i, msg := range messages {
		hashes[i] = hashMessage(msg)
		if i == prefixLen && msg.Role == fantasy.MessageRoleSystem {
			prefixLen++
		}
	}
	ph := fnv.New64a()
	for _, h := range hashes[:prefixLen] {
		var b [8]byte
		for i := range b {
			b[i] = byte(h >> (8 * i))
		}
		_, _ = ph.Write(b[:])
	}
	fc := -1
	for i := 0; i < min(len(hashes), len(prev)); i++ {
		if hashes[i] != prev[i] {
			fc = i
			break
		}
	}
	if fc == -1 && len(hashes) != len(prev) {
		fc = min(len(hashes), len(prev))
	}
	return stepAttribution{
		PrefixHash:        fmt.Sprintf("%016x", ph.Sum64()),
		FirstChanged:      fc,
		FirstChangedCause: firstChangedCause(messages, fc, prefixLen, len(prev)),
	}, hashes
}

// firstChangedCause names the component at the first-changed index:
// cold for the run's first request, append for pure tail growth,
// shrink for truncation, system-prompt/notebook-prefix inside the
// leading system run, history for mid-conversation edits. Empty
// when nothing changed.
func firstChangedCause(messages []fantasy.Message, fc, prefixLen, prevLen int) string {
	switch {
	case prevLen == 0:
		return "cold"
	case fc < 0:
		return ""
	case fc >= len(messages):
		return "shrink"
	case fc >= prevLen:
		return "append"
	case fc < prefixLen:
		if isNotebookMessage(messages[fc]) {
			return "notebook-prefix"
		}
		return "system-prompt"
	default:
		return "history"
	}
}

// isNotebookMessage reports whether msg is a system message carrying a
// notebook-rendered block — the assembled prefix block or an
// auto-inject recall blob.
func isNotebookMessage(msg fantasy.Message) bool {
	if msg.Role != fantasy.MessageRoleSystem || len(msg.Content) == 0 {
		return false
	}
	switch tp := msg.Content[0].(type) {
	case fantasy.TextPart:
		return strings.HasPrefix(tp.Text, "<notebook")
	case *fantasy.TextPart:
		return strings.HasPrefix(tp.Text, "<notebook")
	}
	return false
}

// hashMessage fingerprints a rendered message's content — role plus
// each part's bytes. fnv-64a keeps the hash stable across processes so
// prefix_hash survives cross-run comparison.
func hashMessage(msg fantasy.Message) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(msg.Role))
	for _, part := range msg.Content {
		// A part-type separator keeps concatenated fields from
		// colliding across part boundaries.
		_, _ = h.Write([]byte{0})
		hashPart(h, part)
	}
	return h.Sum64()
}

// hashPart folds a content part into the message hash. Each part leads
// with a stable one-byte type tag so same-content parts of different
// kinds can't collide — a TextPart and a ReasoningPart holding
// identical text are different wire content. The scope is deliberately
// content-only: ProviderOptions (cache breakpoints mark lookup
// positions, not content), ProviderExecuted, and ClientMetadata are
// transport/persistence metadata that don't change what the model
// sees, so they stay unhashed.
func hashPart(h hash.Hash64, part fantasy.MessagePart) {
	write := h.Write
	switch p := part.(type) {
	case fantasy.TextPart:
		_, _ = write([]byte{1})
		_, _ = write([]byte(p.Text))
	case *fantasy.TextPart:
		_, _ = write([]byte{1})
		_, _ = write([]byte(p.Text))
	case fantasy.ReasoningPart:
		_, _ = write([]byte{2})
		_, _ = write([]byte(p.Text))
	case *fantasy.ReasoningPart:
		_, _ = write([]byte{2})
		_, _ = write([]byte(p.Text))
	case fantasy.FilePart:
		_, _ = write([]byte{3})
		_, _ = write(p.Data)
		_, _ = write([]byte{0})
		_, _ = write([]byte(p.Filename))
		_, _ = write([]byte{0})
		_, _ = write([]byte(p.MediaType))
	case *fantasy.FilePart:
		_, _ = write([]byte{3})
		_, _ = write(p.Data)
		_, _ = write([]byte{0})
		_, _ = write([]byte(p.Filename))
		_, _ = write([]byte{0})
		_, _ = write([]byte(p.MediaType))
	case fantasy.ToolCallPart:
		// ToolCallID is wire content — the provider sees it, and two
		// calls with identical tool+input but different IDs are
		// different requests.
		_, _ = write([]byte{4})
		_, _ = write([]byte(p.ToolCallID))
		_, _ = write([]byte{0})
		_, _ = write([]byte(p.ToolName))
		_, _ = write([]byte{0})
		_, _ = write([]byte(p.Input))
	case *fantasy.ToolCallPart:
		_, _ = write([]byte{4})
		_, _ = write([]byte(p.ToolCallID))
		_, _ = write([]byte{0})
		_, _ = write([]byte(p.ToolName))
		_, _ = write([]byte{0})
		_, _ = write([]byte(p.Input))
	case fantasy.ToolResultPart:
		_, _ = write([]byte{5})
		_, _ = write([]byte(p.ToolCallID))
		_, _ = write([]byte{0})
		hashToolResultOutput(h, p.Output)
	case *fantasy.ToolResultPart:
		_, _ = write([]byte{5})
		_, _ = write([]byte(p.ToolCallID))
		_, _ = write([]byte{0})
		hashToolResultOutput(h, p.Output)
	default:
		// A MessagePart kind this switch doesn't know (a future
		// fantasy addition) — hash its discriminator so two
		// different unknown kinds can't silently hash identical.
		_, _ = write([]byte{127})
		_, _ = write([]byte(p.GetType()))
	}
}

func hashToolResultOutput(h hash.Hash64, output fantasy.ToolResultOutputContent) {
	write := h.Write
	switch o := output.(type) {
	case fantasy.ToolResultOutputContentText:
		_, _ = write([]byte{1})
		_, _ = write([]byte(o.Text))
	case *fantasy.ToolResultOutputContentText:
		_, _ = write([]byte{1})
		_, _ = write([]byte(o.Text))
	case fantasy.ToolResultOutputContentError:
		_, _ = write([]byte{2})
		if o.Error != nil {
			_, _ = write([]byte(o.Error.Error()))
		}
	case *fantasy.ToolResultOutputContentError:
		_, _ = write([]byte{2})
		if o.Error != nil {
			_, _ = write([]byte(o.Error.Error()))
		}
	case fantasy.ToolResultOutputContentMedia:
		_, _ = write([]byte{3})
		_, _ = write([]byte(o.Data))
		_, _ = write([]byte{0})
		_, _ = write([]byte(o.MediaType))
		_, _ = write([]byte{0})
		_, _ = write([]byte(o.Text))
	case *fantasy.ToolResultOutputContentMedia:
		_, _ = write([]byte{3})
		_, _ = write([]byte(o.Data))
		_, _ = write([]byte{0})
		_, _ = write([]byte(o.MediaType))
		_, _ = write([]byte{0})
		_, _ = write([]byte(o.Text))
	default:
		// Same future-proofing as hashPart — a new output kind
		// still contributes its discriminator.
		_, _ = write([]byte{127})
		_, _ = write([]byte(o.GetType()))
	}
}

// logStepComposition logs the byte size of each per-step request
// component. It runs inside PrepareStep, where the message list and
// tool list for the step are finalized. System-role messages are
// bucketed separately from conversation history: the main system
// prompt (and any system prompt prefix) counts toward system_bytes,
// while notebook recall blobs count toward notebook_bytes. History
// parts split tool calls/results from other content so the "old
// tool output dominates the prompt" claim is measurable per arm.
// stubs carries cumulative per-session stubbing telemetry: stubbed
// results, saved bytes, and promotion events (each a prompt-cache
// invalidation). nb carries the notebook sufficiency counters:
// entry vs result: recalls, re-views, and per-pass selection
// contributions. The returned requestStats carries the composition
// half for SessionTelemetry export.
func logStepComposition(sessionID string, messages []fantasy.Message, agentTools []fantasy.AgentTool, stubs stubStats, nb notebook.Stats) requestStats {
	var comp requestStats
	for _, msg := range messages {
		n := messageContentBytes(msg)
		if msg.Role == fantasy.MessageRoleSystem {
			if len(msg.Content) > 0 {
				var text string
				switch tp := msg.Content[0].(type) {
				case fantasy.TextPart:
					text = tp.Text
				case *fantasy.TextPart:
					text = tp.Text
				}
				// Matches both the assembled <notebook> block and
				// maybeAutoInject's <notebook_auto_inject> recall blob.
				if strings.HasPrefix(text, "<notebook") {
					comp.NotebookBytes += int64(n)
					continue
				}
			}
			comp.SystemBytes += int64(n)
			continue
		}
		comp.HistoryBytes += int64(n)
		for _, part := range msg.Content {
			pn := int64(messagePartBytes(part))
			switch part.(type) {
			case fantasy.ToolCallPart, *fantasy.ToolCallPart:
				comp.ToolCallBytes += pn
			case fantasy.ToolResultPart, *fantasy.ToolResultPart:
				comp.ToolResultBytes += pn
			}
		}
	}
	builtinSchemaBytes, mcpSchemaBytes := toolSchemaBytes(agentTools)
	slog.Debug("Step request composition",
		"session_id", sessionID,
		"system_bytes", comp.SystemBytes,
		"raw_history_bytes", comp.HistoryBytes,
		"tool_call_bytes", comp.ToolCallBytes,
		"tool_result_bytes", comp.ToolResultBytes,
		"notebook_bytes", comp.NotebookBytes,
		"stubbed_tool_results_total", stubs.Results,
		"stubbed_saved_bytes_total", stubs.SavedBytes,
		"stub_invalidations", stubs.Invalidations,
		"boundary_advances", stubs.BoundaryAdvances,
		"nb_entry_recalls", nb.EntryRecalls,
		"nb_result_recalls", nb.ResultRecalls,
		"nb_cross_recalls", nb.CrossRecalls,
		"nb_empty_recalls", nb.EmptyRecalls,
		"nb_stub_reviews", nb.StubReViews,
		"nb_covered_reviews", nb.CoveredReViews,
		"nb_checkpoints_written", nb.CheckpointsWritten,
		"nb_checkpoint_renders", nb.CheckpointRenders,
		"nb_digests_written", nb.DigestsWritten,
		"nb_digest_renders", nb.DigestRenders,
		"nb_hydration_seeds", nb.HydrationSeeds,
		"nb_hydration_plan_seeds", nb.HydrationPlanSeeds,
		"nb_hydration_renders", nb.HydrationRenders,
		"nb_sel_recency", nb.SelPassRecency,
		"nb_sel_pinned", nb.SelPassPinned,
		"nb_sel_refs", nb.SelPassRefs,
		"nb_sel_working", nb.SelPassWorking,
		"nb_sel_fill", nb.SelPassFill,
		"builtin_tool_schema_bytes", builtinSchemaBytes,
		"mcp_tool_schema_bytes", mcpSchemaBytes,
		"tool_count", len(agentTools),
		"message_count", len(messages),
	)
	return comp
}

// isMCPTool reports whether t is an MCP tool, looking through the hook,
// verification, and scope-gate decorators. Origin is decided at call
// time; do not infer it elsewhere from the flattened tool list without
// unwrapping first. verifyingTool only ever wraps built-in write tools,
// but scopeGateTool wraps the whole list — unwrap anyway so the
// invariant survives a future change in wrap order.
func isMCPTool(t fantasy.AgentTool) bool {
	for {
		switch d := t.(type) {
		case *hookedTool:
			t = d.Unwrap()
		case *verifyingTool:
			t = d.Unwrap()
		case *scopeGateTool:
			t = d.Unwrap()
		default:
			_, ok := t.(*tools.Tool)
			return ok
		}
	}
}

// toolSchemaBytes returns the serialized schema size of the tool list,
// split into built-in and MCP partitions.
func toolSchemaBytes(agentTools []fantasy.AgentTool) (builtin, mcp int64) {
	for _, t := range agentTools {
		data, err := json.Marshal(t.Info())
		if err != nil {
			continue
		}
		if isMCPTool(t) {
			mcp += int64(len(data))
		} else {
			builtin += int64(len(data))
		}
	}
	return builtin, mcp
}

// messageContentBytes returns the content-byte size of a fantasy
// message: role plus the size of each content part. It measures logical
// content, not transport bytes — see the transport wrapper for the
// wire-level measurement.
func messageContentBytes(msg fantasy.Message) int {
	n := len(msg.Role)
	for _, part := range msg.Content {
		n += messagePartBytes(part)
	}
	return n
}

func messagePartBytes(part fantasy.MessagePart) int {
	switch p := part.(type) {
	case fantasy.TextPart:
		return len(p.Text)
	case *fantasy.TextPart:
		return len(p.Text)
	case fantasy.ReasoningPart:
		return len(p.Text)
	case *fantasy.ReasoningPart:
		return len(p.Text)
	case fantasy.FilePart:
		return len(p.Data) + len(p.Filename) + len(p.MediaType)
	case *fantasy.FilePart:
		return len(p.Data) + len(p.Filename) + len(p.MediaType)
	case fantasy.ToolCallPart:
		return len(p.ToolName) + len(p.Input)
	case *fantasy.ToolCallPart:
		return len(p.ToolName) + len(p.Input)
	case fantasy.ToolResultPart:
		return toolResultOutputBytes(p.Output)
	case *fantasy.ToolResultPart:
		return toolResultOutputBytes(p.Output)
	default:
		return 0
	}
}

func toolResultOutputBytes(output fantasy.ToolResultOutputContent) int {
	switch o := output.(type) {
	case fantasy.ToolResultOutputContentText:
		return len(o.Text)
	case *fantasy.ToolResultOutputContentText:
		return len(o.Text)
	case fantasy.ToolResultOutputContentError:
		if o.Error != nil {
			return len(o.Error.Error())
		}
	case *fantasy.ToolResultOutputContentError:
		if o.Error != nil {
			return len(o.Error.Error())
		}
	case fantasy.ToolResultOutputContentMedia:
		return len(o.Data) + len(o.MediaType) + len(o.Text)
	case *fantasy.ToolResultOutputContentMedia:
		return len(o.Data) + len(o.MediaType) + len(o.Text)
	}
	return 0
}

// normalizedPromptTokens is the canonical prompt-token count for a
// provider usage report. The session counters and the title/summarize
// path previously disagreed on which cache tokens count toward prompt
// tokens; this normalizes both to input + cache write + cache read.
func normalizedPromptTokens(usage fantasy.Usage) int64 {
	return usage.InputTokens + usage.CacheCreationTokens + usage.CacheReadTokens
}

// logStepUsage logs the raw provider token counters alongside the
// normalized prompt-token count so the two can be reconciled.
func logStepUsage(sessionID string, usage fantasy.Usage, estimated bool) {
	slog.Debug("Step token usage",
		"session_id", sessionID,
		"input_tokens", usage.InputTokens,
		"output_tokens", usage.OutputTokens,
		"cache_creation_tokens", usage.CacheCreationTokens,
		"cache_read_tokens", usage.CacheReadTokens,
		"prompt_tokens_normalized", normalizedPromptTokens(usage),
		"estimated", estimated,
	)
}

// byteCountingTransport is an http.RoundTripper that measures the
// serialized request and response bodies crossing the transport. It
// reports per-request totals tagged with the x-session-id header when
// present, giving transport-level payload telemetry that content
// measurements cannot capture (JSON field names, escaping, tool
// schemas, provider options, base64 attachments, headers).
//
// Streaming (SSE) responses are counted incrementally and reported when
// the body reaches EOF or is closed. The request-byte count is read at
// report time rather than when RoundTrip returns: with HTTP/2 or
// "Expect: 100-continue", response headers can arrive while the request
// body is still streaming, so an early sample can under-report. For
// early error responses the count may still be partial — provider
// payloads are buffered JSON, so this is a known but minor limitation.
type byteCountingTransport struct {
	next http.RoundTripper
}

func (t *byteCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var reqCounter *countingReadCloser
	if req.Body != nil && req.Body != http.NoBody {
		reqCounter = &countingReadCloser{inner: req.Body}
		req.Body = reqCounter
	}
	resp, err := t.next.RoundTrip(req)
	sessionHash := req.Header.Get("x-session-id")
	if err != nil {
		var reqBytes int64
		if reqCounter != nil {
			reqBytes = reqCounter.n.Load()
		}
		slog.Debug("LLM transport",
			"session_hash", sessionHash,
			"host", req.URL.Host,
			"request_bytes", reqBytes,
			"error", err,
		)
		return nil, err
	}
	if resp.Body != nil && resp.Body != http.NoBody {
		resp.Body = &reportingReadCloser{
			inner:      resp.Body,
			sessionID:  sessionHash,
			host:       req.URL.Host,
			statusCode: resp.StatusCode,
			reqCounter: reqCounter,
		}
	} else {
		var reqBytes int64
		if reqCounter != nil {
			reqBytes = reqCounter.n.Load()
		}
		slog.Debug("LLM transport",
			"session_hash", sessionHash,
			"host", req.URL.Host,
			"status", resp.StatusCode,
			"request_bytes", reqBytes,
			"response_bytes", 0,
		)
	}
	return resp, nil
}

// countingReadCloser counts bytes read from an underlying body.
type countingReadCloser struct {
	inner io.ReadCloser
	n     atomic.Int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *countingReadCloser) Close() error { return c.inner.Close() }

// reportingReadCloser counts response-body bytes and logs the
// request/response pair once the body is fully consumed or closed. The
// request counter is read at report time so the request_bytes figure
// reflects as much of the streamed upload as completed.
type reportingReadCloser struct {
	inner      io.ReadCloser
	sessionID  string
	host       string
	statusCode int
	reqCounter *countingReadCloser
	respBytes  int64
	reported   bool
}

func (r *reportingReadCloser) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	r.respBytes += int64(n)
	if err == io.EOF {
		r.report()
	}
	return n, err
}

func (r *reportingReadCloser) Close() error {
	err := r.inner.Close()
	r.report()
	return err
}

func (r *reportingReadCloser) report() {
	if r.reported {
		return
	}
	r.reported = true
	var reqBytes int64
	if r.reqCounter != nil {
		reqBytes = r.reqCounter.n.Load()
	}
	slog.Debug("LLM transport",
		"session_hash", r.sessionID,
		"host", r.host,
		"status", r.statusCode,
		"request_bytes", reqBytes,
		"response_bytes", r.respBytes,
	)
}

// instrumentedHTTPClient wraps client's transport with the
// byte-counting transport, preserving any existing transport (e.g. the
// debug round-trip logger) underneath. A nil client yields a client
// measuring http.DefaultTransport.
//
// Coverage is asymmetric by necessity: REST providers (Anthropic,
// OpenAI, OpenRouter, Vercel, Azure, OpenAI-compatible) always get an
// instrumented client since they accept WithHTTPClient, while
// Bedrock/Vertex/Google SDK-managed transports are measured only when
// the caller injects a client (debug mode); otherwise transport
// measurement is unavailable for them.
func instrumentedHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	instrumented := *client
	instrumented.Transport = &byteCountingTransport{next: base}
	return &instrumented
}
