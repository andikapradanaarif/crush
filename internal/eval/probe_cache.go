package eval

// Cache probe: measures the serving endpoint's prompt-cache behavior
// through the same provider-level request path Crush uses —
// openaicompat provider construction, session-affinity headers, and
// streaming with include_usage. It is deliberately not the full agent
// loop: no tools array, no fantasy retry layer, a small max_tokens,
// and synthetic filler text instead of real session content. Those
// deltas can shift absolute hit fractions if the endpoint folds tools
// into cache-key material, but prefix-vs-position reuse semantics —
// what the probe exists to measure — transfer. No real session data
// ever leaves the machine. See eval issue #116 for the experiment
// spec: 6 conditions × 5 repeats over ~50K-token requests,
// randomized order, independently warmed blocks, raw provider usage
// captured alongside fantasy-normalized usage.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
)

// ProbeCacheConfig parameterizes one probe run.
type ProbeCacheConfig struct {
	BaseURL      string
	APIKey       string
	Model        string
	TargetTokens int // Approximate prompt size (~50K mirrors observed runs).
	Repeats      int // Measured requests per condition (spec: 5).
	MaxRequests  int // Hard spend cap — abort when reached.
	OutPath      string
	RawDir       string // Raw response bodies land here.
	Seed         int64
	DelayScale   float64 // Multiplier on all sleeps (1.0 = spec timing).
	DryRun       bool
}

// Probe conditions — the six request shapes the spec enumerates.
var probeConditions = []string{
	"identical",     // Byte-identical resend — reuse baseline.
	"append",        // Pure tail growth — the cache-friendly case.
	"early-system",  // Same-length mutation early in the system prompt.
	"notebook",      // Same-length mutation at the notebook-block position.
	"late-history",  // Same-length mutation ~80% into history.
	"fresh-process", // Identical content, brand-new transport/client.
}

// ProbeMeta is the first JSONL row (kind="meta") — the run's
// provenance. Without it a completed artifact can't be reproduced or
// audited: the seed drives both the filler content and the schedule
// shuffle, and the session string is hashed into the affinity header.
type ProbeMeta struct {
	Kind         string    `json:"kind"` // Always "meta".
	TS           time.Time `json:"ts"`
	Seed         int64     `json:"seed"`
	Session      string    `json:"session"` // Pre-hash affinity string.
	AffinityHash string    `json:"affinity_hash"`
	Model        string    `json:"model"`
	BaseURL      string    `json:"base_url"`
	TargetTokens int       `json:"target_tokens"`
	Repeats      int       `json:"repeats"`
	DelayScale   float64   `json:"delay_scale"`
	MaxRequests  int       `json:"max_requests"`
	Conditions   []string  `json:"conditions"`
}

// ProbeRecord is one JSONL row per logical send.
type ProbeRecord struct {
	Seq       int       `json:"seq"`
	TS        time.Time `json:"ts"`
	Kind      string    `json:"kind"` // warm | measured | control | delayed
	Condition string    `json:"condition"`
	Repeat    int       `json:"repeat"`
	DelayMS   int64     `json:"delay_ms"` // Since the previous send.
	// FreshProcess is true only when the send actually ran on a
	// rebuilt transport; FreshFallback records why it didn't (the
	// fresh client/model construction failed and the send fell back
	// to the shared one).
	FreshProcess  bool   `json:"fresh_process,omitempty"`
	FreshFallback string `json:"fresh_fallback,omitempty"`
	Attempts      int    `json:"attempts"`     // HTTP round trips observed.
	RequestHash   string `json:"request_hash"` // FNV-64a of the wire body.
	MutatedIndex  int    `json:"mutated_index"`
	// Usage is fantasy-normalized: InputTokens is the UNCACHED
	// remainder (prompt_tokens minus cached), not the full prompt
	// size — input+cache_read reconstructs it.
	Usage     fantasy.Usage  `json:"usage_normalized"`
	RawUsage  map[string]any `json:"usage_raw,omitempty"`
	LatencyMS int64          `json:"latency_ms"`
	Error     string         `json:"error,omitempty"`
}

// probeCapture stores one exchange: wire-body hash + buffered SSE body.
type probeCapture struct {
	reqHash string
	buf     bytes.Buffer
}

// probeStore accumulates captures in send order (the probe is strictly
// sequential, so order correlates request → record).
type probeStore struct {
	mu   sync.Mutex
	caps []*probeCapture
}

func (s *probeStore) add(c *probeCapture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.caps = append(s.caps, c)
}

func (s *probeStore) last() *probeCapture {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.caps) == 0 {
		return nil
	}
	return s.caps[len(s.caps)-1]
}

func (s *probeStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.caps)
}

// probeTransport tees response bodies into a capture so raw provider
// usage survives fantasy's normalization. Request bodies are buffered
// (they're the canonical request identity — hashed for the record).
type probeTransport struct {
	next  http.RoundTripper
	store *probeStore
}

func (t *probeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	h := fnv.New64a()
	_, _ = h.Write(body)

	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	c := &probeCapture{reqHash: fmt.Sprintf("%016x", h.Sum64())}
	if resp.Body != nil {
		resp.Body = &teeReadCloser{rc: resp.Body, w: &c.buf}
	}
	t.store.add(c)
	return resp, nil
}

type teeReadCloser struct {
	rc io.ReadCloser
	w  *bytes.Buffer
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		_, _ = t.w.Write(p[:n])
	}
	return n, err
}

func (t *teeReadCloser) Close() error { return t.rc.Close() }

// lastSSEUsage scans the buffered stream body for the terminal usage
// chunk (openai-compatible servers emit `usage` once, on the final
// chunk, when include_usage is set — fantasy sets it). Lines beyond
// the 1MiB scanner bound silently drop the usage — oversized SSE
// chunks show up as a missing usage_raw field, not a parse error.
func lastSSEUsage(body []byte) map[string]any {
	var usage map[string]any
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) == nil && chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
	return usage
}

// probeFiller generates deterministic pseudo-source text — code-shaped
// filler tokenizes denser than prose, closer to real history.
func probeFiller(rng *rand.Rand, targetBytes int) string {
	if targetBytes <= 0 {
		return ""
	}
	var b strings.Builder
	idents := []string{"parse", "render", "buffer", "dispatch", "resolve", "commit", "token", "session", "window", "cursor", "segment", "handle"}
	for b.Len() < targetBytes {
		name := idents[rng.Intn(len(idents))]
		fmt.Fprintf(&b, "\n// %s_%d handles slot %d.\nfunc %s_%d(ctx int, n int) int {\n", name, rng.Intn(997), rng.Intn(64), name, rng.Intn(997))
		for i := 0; i < 4+rng.Intn(6); i++ {
			fmt.Fprintf(&b, "\tif ctx > %d {\n\t\tn += %d * (ctx %% %d)\n\t}\n", rng.Intn(500), rng.Intn(97), rng.Intn(89)+3)
		}
		fmt.Fprintf(&b, "\treturn n ^ 0x%x\n}\n", rng.Intn(1<<20))
	}
	return b.String()[:targetBytes]
}

// probeBundle is the base request plus all mutated variants.
type probeBundle struct {
	base     fantasy.Prompt
	variants map[string]fantasy.Prompt
	// mutatedIdx records which message index each condition changed —
	// -1 for identical/fresh-process, len(base) for append.
	mutatedIdx map[string]int
}

// buildProbeBundle constructs a ~TargetTokens request shaped like a
// real Crush render: system prompt, then a <notebook> system block,
// then alternating user/assistant history.
func buildProbeBundle(rng *rand.Rand, targetTokens int) probeBundle {
	// ~3.8 bytes/token for code-ish content; sizes are starting points —
	// the first warm's reported prompt_tokens is the ground truth.
	total := targetTokens * 38 / 10
	sysBytes := total / 6
	nbBytes := total / 8
	histBytes := total - sysBytes - nbBytes

	header := "You are a coding agent. Follow the rules below for every request.\n"
	rules := "Rules: read before editing; keep changes minimal; prefer existing helpers; never invent APIs; run tests after edits; report blockers early. "
	sys := fantasy.NewSystemMessage(header + strings.Repeat(rules, sysBytes/len(rules)+1)[:sysBytes-len(header)])

	nbContent := "<notebook>\n" + probeFiller(rng, nbBytes-64) + "\n</notebook>"
	nb := fantasy.NewSystemMessage(nbContent)

	// History: ~24 alternating user/assistant messages.
	var hist []fantasy.Message
	nMsg := 24
	per := histBytes / nMsg
	for i := 0; i < nMsg; i++ {
		text := probeFiller(rng, per)
		if i%2 == 0 {
			hist = append(hist, fantasy.NewUserMessage(text))
		} else {
			hist = append(hist, fantasy.Message{
				Role:    fantasy.MessageRoleAssistant,
				Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}},
			})
		}
	}

	base := append(fantasy.Prompt{sys, nb}, hist...)

	// sameLen swaps a substring at offset with different content of
	// identical byte length — token count preserved approximately.
	// Offsets/spans clamp to the message so small test bundles work.
	sameLen := func(s string, offset, span int) string {
		if offset < 0 || offset >= len(s) {
			offset = len(s) / 2
		}
		if span <= 0 || offset+span > len(s) {
			span = len(s) - offset
		}
		if span == 0 {
			return s
		}
		repl := probeFiller(rng, span)
		return s[:offset] + repl + s[offset+span:]
	}

	// copyPrompt deep-enough-copies a prompt: mutating a message's
	// Content[0] must not write through into base's shared slice.
	copyPrompt := func(p fantasy.Prompt, i int, fn func(fantasy.TextPart) fantasy.TextPart) fantasy.Prompt {
		v := append(fantasy.Prompt{}, p...)
		v[i].Content = append([]fantasy.MessagePart{}, v[i].Content...)
		v[i].Content[0] = fn(v[i].Content[0].(fantasy.TextPart))
		return v
	}
	variants := map[string]fantasy.Prompt{
		"identical":     base,
		"fresh-process": base,
		"append": append(append(fantasy.Prompt{}, base...),
			fantasy.NewUserMessage("One more thing: "+probeFiller(rng, per/2)),
			fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: probeFiller(rng, per/2)}}}),
		"early-system": copyPrompt(base, 0, func(tp fantasy.TextPart) fantasy.TextPart {
			return fantasy.TextPart{Text: sameLen(tp.Text, len(header)+16, 512)}
		}),
		"notebook": copyPrompt(base, 1, func(tp fantasy.TextPart) fantasy.TextPart {
			// Replace the inner span with same-length content.
			return fantasy.TextPart{Text: "<notebook>\n" + probeFiller(rng, len(tp.Text)-23) + "\n</notebook>"}
		}),
		"late-history": copyPrompt(base, 2+int(float64(len(hist))*0.8), func(tp fantasy.TextPart) fantasy.TextPart {
			return fantasy.TextPart{Text: sameLen(tp.Text, len(tp.Text)/2, 512)}
		}),
	}
	mutatedIdx := map[string]int{
		"identical": -1, "fresh-process": -1, "append": len(base),
		"early-system": 0, "notebook": 1,
		"late-history": 2 + int(float64(len(hist))*0.8),
	}
	return probeBundle{base: base, variants: variants, mutatedIdx: mutatedIdx}
}

// minProbeTargetTokens is the smallest prompt size whose bundle keeps
// every condition's mutation meaningful — below it the system-prompt
// and notebook builders hit negative slice bounds or degenerate to
// byte-identical variants.
const minProbeTargetTokens = 1000

// RunProbeCache executes the probe. Writes a kind="meta" provenance
// record then one JSONL record per send to cfg.OutPath, raw SSE bodies
// under cfg.RawDir, and prints the per-condition cache-hit table — the
// decision-gate readout — at the end. Returns nil only when the full
// schedule ran; an early cap hit, three consecutive send errors, or a
// cancelled context come back as errors so automation can tell a
// complete run from an aborted one.
func RunProbeCache(ctx context.Context, cfg ProbeCacheConfig) error {
	if cfg.Repeats <= 0 {
		cfg.Repeats = 5
	}
	if cfg.MaxRequests <= 0 {
		cfg.MaxRequests = 120
	}
	if cfg.TargetTokens <= 0 {
		cfg.TargetTokens = 50000
	}
	if cfg.TargetTokens < minProbeTargetTokens {
		return fmt.Errorf("target tokens %d below minimum %d", cfg.TargetTokens, minProbeTargetTokens)
	}
	if cfg.DelayScale < 0 {
		cfg.DelayScale = 1
	}
	// DelayScale 0 is legitimate — tests use it for instant sleeps.
	if cfg.RawDir == "" {
		cfg.RawDir = strings.TrimSuffix(cfg.OutPath, filepath.Ext(cfg.OutPath)) + "-raw"
	}
	rng := rand.New(rand.NewSource(cfg.Seed))
	bundle := buildProbeBundle(rng, cfg.TargetTokens)

	// Schedule: 6 conditions × repeats, shuffled into independently
	// warmed blocks. Controls (measured identical resends) interleave
	// every ~5 blocks; repeats 2 and 4 of each condition get a delayed
	// re-send to test seconds-scale persistence.
	type block struct {
		cond string
		rep  int
	}
	var blocks []block
	for _, c := range probeConditions {
		for r := 1; r <= cfg.Repeats; r++ {
			blocks = append(blocks, block{c, r})
		}
	}
	rng.Shuffle(len(blocks), func(i, j int) { blocks[i], blocks[j] = blocks[j], blocks[i] })

	nDelayed := 0
	for _, r := range []int{2, 4} {
		if r <= cfg.Repeats {
			nDelayed += len(probeConditions)
		}
	}
	estSends := len(blocks)*2 + nDelayed + len(blocks)/5
	fmt.Printf("probe: %d blocks, ~%d sends, cap %d, seed %d, out %s\n",
		len(blocks), estSends, cfg.MaxRequests, cfg.Seed, cfg.OutPath)
	if cfg.DryRun {
		for i, b := range blocks {
			fmt.Printf("  %2d: %-13s rep=%d\n", i, b.cond, b.rep)
		}
		return nil
	}
	if cfg.BaseURL == "" {
		// openaicompat's default is api.openai.com — falling through
		// to it would send the configured key to the wrong provider.
		return fmt.Errorf("base URL required")
	}
	if cfg.APIKey == "" {
		return fmt.Errorf("API key required")
	}
	if err := os.MkdirAll(cfg.RawDir, 0o755); err != nil {
		return err
	}

	// Session affinity is constant for the whole run — including the
	// fresh-process condition, which rebuilds the transport, not the
	// header.
	probeSession := fmt.Sprintf("probe-%d-%d", time.Now().Unix(), cfg.Seed)
	affinity := session.HashID(probeSession)
	headers := map[string]string{"x-session-id": affinity, "x-session-affinity": affinity}
	fmt.Printf("probe: session %s affinity %s\n", probeSession, affinity)

	newClient := func() (*http.Client, *probeStore, *http.Transport) {
		store := &probeStore{}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		return &http.Client{Transport: &probeTransport{next: tr, store: store}}, store, tr
	}
	newModel := func(client *http.Client) (fantasy.LanguageModel, error) {
		prov, err := openaicompat.New(
			openaicompat.WithBaseURL(cfg.BaseURL),
			openaicompat.WithAPIKey(cfg.APIKey),
			openaicompat.WithHTTPClient(client),
		)
		if err != nil {
			return nil, err
		}
		return prov.LanguageModel(ctx, cfg.Model)
	}

	client, store, _ := newClient()
	model, err := newModel(client)
	if err != nil {
		return err
	}

	out, err := os.Create(cfg.OutPath)
	if err != nil {
		return err
	}
	defer out.Close()
	enc := json.NewEncoder(out)
	// Provenance first — the artifact must be self-describing.
	if err := enc.Encode(ProbeMeta{
		Kind: "meta", TS: time.Now(), Seed: cfg.Seed,
		Session: probeSession, AffinityHash: affinity,
		Model: cfg.Model, BaseURL: cfg.BaseURL,
		TargetTokens: cfg.TargetTokens, Repeats: cfg.Repeats,
		DelayScale: cfg.DelayScale, MaxRequests: cfg.MaxRequests,
		Conditions: probeConditions,
	}); err != nil {
		return err
	}

	seq := 0
	lastSend := time.Now()
	consecErr := 0
	var recs []ProbeRecord
	send := func(kind, cond string, rep int, msgs fantasy.Prompt, mutIdx int, fresh bool) {
		if seq >= cfg.MaxRequests || consecErr >= 3 || ctx.Err() != nil {
			return
		}
		seq++
		rec := ProbeRecord{
			Seq: seq, TS: time.Now(), Kind: kind, Condition: cond, Repeat: rep,
			MutatedIndex: mutIdx,
		}
		m, st := model, store
		var freshTr *http.Transport
		if fresh {
			fc, fs, ftr := newClient()
			fm, ferr := newModel(fc)
			if ferr == nil {
				m, st = fm, fs
				rec.FreshProcess = true
				freshTr = ftr
			} else {
				// The send still runs on the shared transport —
				// record the fallback so the row doesn't claim a
				// fresh connection it didn't get.
				rec.FreshFallback = ferr.Error()
				ftr.CloseIdleConnections()
			}
		}
		delay := time.Since(lastSend)
		lastSend = time.Now()
		rec.DelayMS = delay.Milliseconds()
		prevCaps := st.len()
		start := time.Now()
		// Per-request timeout mirrors production's request-timeout
		// model — a stalled stream must not hang the run.
		sendCtx, cancelSend := context.WithTimeout(ctx, config.DefaultRequestTimeout)
		stream, serr := m.Stream(sendCtx, fantasy.Call{
			Prompt:          msgs,
			Headers:         headers,
			MaxOutputTokens: ptrOf(int64(32)),
			Temperature:     ptrOf(0.0),
		})
		if serr == nil {
			for part := range stream {
				if part.Error != nil {
					serr = part.Error
				}
				if part.Usage.TotalTokens > 0 || part.Usage.InputTokens > 0 {
					rec.Usage = part.Usage
				}
			}
		}
		cancelSend()
		if freshTr != nil {
			freshTr.CloseIdleConnections()
		}
		rec.LatencyMS = time.Since(start).Milliseconds()
		rec.Attempts = st.len() - prevCaps
		// Only a capture from THIS send is attributable — a transport-
		// level failure appends nothing, and reading last() anyway
		// would stamp the previous request's hash, usage, and body
		// onto the failure record.
		if rec.Attempts > 0 {
			if capt := st.last(); capt != nil {
				rec.RequestHash = capt.reqHash
				rec.RawUsage = lastSSEUsage(capt.buf.Bytes())
				fname := filepath.Join(cfg.RawDir, fmt.Sprintf("seq-%04d-%s-%s.resp", seq, kind, cond))
				_ = os.WriteFile(fname, capt.buf.Bytes(), 0o644)
			}
		}
		if serr != nil {
			rec.Error = serr.Error()
			consecErr++
		} else {
			consecErr = 0
		}
		if err := enc.Encode(rec); err != nil {
			fmt.Fprintf(os.Stderr, "probe: encode: %v\n", err)
		}
		recs = append(recs, rec)
		status := ""
		if serr != nil {
			status = " ERR " + serr.Error()
		}
		// InputTokens is the uncached remainder (fantasy normalizes
		// prompt_tokens minus cached_tokens) — label it as such.
		fmt.Printf("  %3d %-8s %-13s uncached=%6d cache_read=%6d%s\n",
			seq, kind, cond, rec.Usage.InputTokens, rec.Usage.CacheReadTokens, status)
	}

	sleep := func(lo, hi time.Duration) {
		if ctx.Err() != nil || seq >= cfg.MaxRequests || consecErr >= 3 {
			return
		}
		d := lo + time.Duration(rng.Int63n(int64(hi-lo)))
		t := time.NewTimer(time.Duration(float64(d) * cfg.DelayScale))
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}

	doneBlocks := 0
	for i, b := range blocks {
		if seq >= cfg.MaxRequests || consecErr >= 3 || ctx.Err() != nil {
			break
		}
		// The warm carries the block's condition so it's attributable
		// without seq-adjacency inference; kind=warm distinguishes it.
		send("warm", b.cond, 0, bundle.base, -1, false)
		sleep(8*time.Second, 22*time.Second)
		fresh := b.cond == "fresh-process"
		send("measured", b.cond, b.rep, bundle.variants[b.cond], bundle.mutatedIdx[b.cond], fresh)
		if b.rep == 2 || b.rep == 4 {
			sleep(45*time.Second, 75*time.Second)
			send("delayed", b.cond, b.rep, bundle.variants[b.cond], bundle.mutatedIdx[b.cond], fresh)
		}
		if i%5 == 4 {
			send("control", "identical", 0, bundle.variants["identical"], -1, false)
		}
		doneBlocks++
	}
	fmt.Printf("probe done: %d sends → %s\n", seq, cfg.OutPath)
	ProbeSummarize(os.Stdout, recs)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case consecErr >= 3:
		return fmt.Errorf("probe aborted: %d consecutive send errors", consecErr)
	case doneBlocks < len(blocks):
		return fmt.Errorf("probe incomplete: spend cap reached after %d sends (%d/%d blocks ran)",
			seq, doneBlocks, len(blocks))
	}
	return nil
}

func ptrOf[T any](v T) *T { return &v }

// ProbeSummarize prints the per-condition cache-hit table — the
// decision-gate readout. Delayed resends get their own rows so
// seconds-scale persistence is visible next to the immediate repeats;
// warm rows are excluded (they establish the cache, they don't test
// it).
func ProbeSummarize(w io.Writer, recs []ProbeRecord) {
	type acc struct{ prompt, hits, rawHit []int64 }
	by := map[string]*acc{}
	var order []string
	for _, r := range recs {
		if r.Kind != "measured" && r.Kind != "delayed" && r.Kind != "control" {
			continue
		}
		key := r.Condition
		if r.Kind == "delayed" {
			key += " (delayed)"
		}
		a := by[key]
		if a == nil {
			a = &acc{}
			by[key] = a
			order = append(order, key)
		}
		// Fantasy normalizes InputTokens to the uncached remainder
		// (prompt_tokens − cached_tokens), so input+cached
		// reconstructs the true prompt size — the correct hit%
		// denominator.
		a.prompt = append(a.prompt, r.Usage.InputTokens+r.Usage.CacheReadTokens)
		a.hits = append(a.hits, r.Usage.CacheReadTokens)
		// Provider-native cache fields (DeepSeek-style) survive in
		// the raw usage — prefer them for the hit column when present.
		if v, ok := r.RawUsage["prompt_cache_hit_tokens"].(float64); ok {
			a.rawHit = append(a.rawHit, int64(v))
		} else {
			a.rawHit = append(a.rawHit, -1)
		}
	}
	sort.Strings(order)
	fmt.Fprintf(w, "\n%-22s %4s %10s %10s %10s %8s\n", "condition", "n", "prompt", "cache_read", "raw_hit", "hit%")
	for _, c := range order {
		a := by[c]
		var hitFrac float64
		for i := range a.hits {
			if a.prompt[i] > 0 {
				hitFrac += float64(a.hits[i]) / float64(a.prompt[i])
			}
		}
		hitFrac /= float64(len(a.hits))
		raw := "-"
		var rawVals []int64
		for _, v := range a.rawHit {
			if v >= 0 {
				rawVals = append(rawVals, v)
			}
		}
		if len(rawVals) > 0 {
			raw = fmt.Sprintf("%d", median(rawVals))
		}
		fmt.Fprintf(w, "%-22s %4d %10d %10d %10s %7.1f%%\n",
			c, len(a.hits), median(a.prompt), median(a.hits), raw, hitFrac*100)
	}
	fmt.Fprintln(w, "hit% = mean(cache_read / (uncached + cache_read)) per row; 'identical' includes control resends; warm rows excluded.")
}

func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64{}, v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}
