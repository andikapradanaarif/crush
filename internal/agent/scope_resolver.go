package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/params"
)

// scopeResolverTimeout bounds one L3 round-trip — the call is
// synchronous inside selectMemory's prompt analysis, so a hung model
// must not stall the turn. The wall must cover reasoning latency:
// the resolver falls back to the large model when no small model is
// configured, and a reasoning consult runs ~20s (observed 19.9s /
// 2143 reasoning tokens on muse-spark, #333) — a 10s wall abstained
// on 100% of consults. Cached verdicts mean the headroom is paid
// once per (prompt, candidates), not per turn.
const scopeResolverTimeout = 60 * time.Second

// scopeResolverMaxCandidates bounds how many rows the resolver prompt
// lists. The candidate set is prompt context, not an inventory — a
// bounded head keeps the call cheap and the parse surface small.
const scopeResolverMaxCandidates = 12

// scopeResolverFieldRunes bounds any single candidate field in the
// resolver prompt — attacker-influenced text (a seeded headline) can
// be long; the model needs a pointer, not the row.
const scopeResolverFieldRunes = 120

// scopeResolverPromptRunes bounds the user prompt as rendered into
// the resolver's context — the same hygiene the candidate fields get.
const scopeResolverPromptRunes = 2048

// scopeResolverSystemPrompt is the L3 layer's closed-output contract
// (#216): the model answers a prompt-level scope question — never
// names a row — and the deterministic validator below decides whether
// the answer may steer selection at all.
const scopeResolverSystemPrompt = `You decide whether a user's request refers to a past test, build, lint, or run failure, and which repo paths it scopes to.

Reply with exactly one JSON object and nothing else — no prose, no code fence:
{"about_failure":true,"include_paths":["src/"],"exclude_paths":[],"kind":"test"}

Rules:
- about_failure=false means the request is not about a listed failure. Leave every other field empty.
- about_failure=true means the request asks to fix, investigate, or verify a listed failure.
- include_paths: repo-relative paths the request positively scopes to. Only paths that exist. Empty means unscoped.
- exclude_paths: repo-relative paths the request rules out.
- kind: one of "test", "build", "lint", "run" when the request names a failure kind; "" when any kind fits.
- When unsure, answer about_failure=false. Never reference a failure by number or repeat its command.`

// scopeResolutionWire is the on-the-wire form — AboutFailure is a
// pointer so a missing key is distinguishable from an explicit false
// (the closed contract requires the key present).
type scopeResolutionWire struct {
	AboutFailure *bool    `json:"about_failure"`
	IncludePaths []string `json:"include_paths"`
	ExcludePaths []string `json:"exclude_paths"`
	Kind         string   `json:"kind"`
}

// resolverState is one session's L3 layer state — the resolution
// cache and the call-budget counter, shared across agent rebuilds via
// the coordinator-owned map (the same lifetime as usageLedger).
type resolverState struct {
	cache *csync.Map[string, *ScopeResolution]
	// callsUsed counts model round-trips against params.ResolverCalls
	// — claimed calls count even when the response fails validation,
	// since the spend happened either way.
	callsUsed atomic.Int32
}

// Resolver engagement outcomes — the turn-level closed vocabulary
// stamped on TailAudit.Resolver so a silent layer is measurable:
// a verdict (about true OR false) settles rows with settled_resolver
// in the decision stream, but an abstention leaves no trace there —
// without this stamp "the gate never opened" and "the layer was
// consulted and produced nothing" are indistinguishable on records.
const (
	// resolverResolved: the call produced a valid verdict — the
	// decision stream carries the stamps.
	resolverResolved = "resolved"
	// resolverAbstain: the gate fired and the call produced no usable
	// verdict — error, invalid, empty, truncated, or timed out.
	resolverAbstain = "abstain"
	// resolverBudget: the gate fired but the per-session call budget
	// was already spent — the layer is fine, the calls ran out
	// (#332). Splitting it from abstain keeps "resolver dead" and
	// "budget spent early" distinguishable on eval records.
	resolverBudget = "budget"
)

// errResolverBudget is the typed reason ResolveScope returns when
// the consult is skipped because the call budget is spent —
// outcomeResolver maps it to resolverBudget rather than lumping it
// into abstain.
var errResolverBudget = errors.New("scope resolver: call budget exhausted")

// outcomeResolver wraps a ScopeResolver to record whether the call
// produced a verdict — telemetry only, it changes no behavior. The
// gate never firing reads as "" (the zero value), which stays
// distinguishable from abstain: consulted-and-failed vs never-asked.
type outcomeResolver struct {
	inner ScopeResolver
	out   *string
}

func (o outcomeResolver) ResolveScope(ctx context.Context, prompt string, cands []ResolverCandidate) (*ScopeResolution, error) {
	res, err := o.inner.ResolveScope(ctx, prompt, cands)
	switch {
	case err == nil && res != nil:
		*o.out = resolverResolved
	case errors.Is(err, errResolverBudget):
		*o.out = resolverBudget
	default:
		*o.out = resolverAbstain
	}
	return res, err
}

// llmScopeResolver is the production ScopeResolver — one bounded
// small-model call per (prompt, candidates) content hash, validated
// against the closed contract before any of it steers selection.
// Anything that is not a clean valid resolution abstains.
type llmScopeResolver struct {
	a         *sessionAgent
	state     *resolverState
	sessionID string
	workDir   string
	budget    int
	// timeout bounds the model round-trip — zero uses
	// scopeResolverTimeout.
	timeout time.Duration
}

// scopeResolverFor builds the turn's resolver — nil when the layer is
// disabled (resolver_calls=0), unconfigured (no model at all), or in
// a sub-agent, whose prompts are delegated scope already.
func (a *sessionAgent) scopeResolverFor(workDir, sessionID string, mp params.Memory) ScopeResolver {
	if mp.ResolverCalls <= 0 || a.isSubAgent || sessionID == "" || a.resolverStates == nil {
		return nil
	}
	if a.smallModel.Get().Model == nil && a.largeModel.Get().Model == nil {
		return nil
	}
	state := a.resolverStates.GetOrSet(sessionID, func() *resolverState {
		return &resolverState{cache: csync.NewMap[string, *ScopeResolution]()}
	})
	return &llmScopeResolver{a: a, state: state, sessionID: sessionID, workDir: workDir, budget: mp.ResolverCalls}
}

func (r *llmScopeResolver) ResolveScope(ctx context.Context, prompt string, cands []ResolverCandidate) (*ScopeResolution, error) {
	// Cache first: a warm verdict costs no round-trip, so it serves
	// even after the budget trips — the budget bounds model calls,
	// not reads of what they already resolved.
	key := scopeResolverCacheKey(prompt, cands)
	if res, ok := r.state.cache.Get(key); ok {
		return res, nil
	}
	if r.budget <= 0 || int(r.state.callsUsed.Load()) >= r.budget {
		return nil, errResolverBudget
	}
	// Claim the call before spending it — a parse failure still
	// burned the model round-trip, so it counts against the budget.
	r.state.callsUsed.Add(1)
	res, err := r.call(ctx, prompt, cands)
	if err != nil {
		slog.Debug("Scope resolver abstained", "err", err)
		return nil, err
	}
	// Valid resolutions cache — including about=false, which is a
	// verdict. Abstentions (invalid output, errors) do not: a
	// transient glitch should not pin the layer off for a prompt.
	r.state.cache.Set(key, res)
	return res, nil
}

func (r *llmScopeResolver) call(ctx context.Context, prompt string, cands []ResolverCandidate) (*ScopeResolution, error) {
	// Attempt order mirrors GenerateTitle: small first, large as the
	// fallback. A configured-but-broken small provider (bad key,
	// zero credits) must not pin the layer off — a single abstain
	// outcome per consult, but the consult should still be tried on
	// the model that demonstrably works in this session.
	models := []Model{r.a.smallModel.Get()}
	if large := r.a.largeModel.Get(); large.Model != nil &&
		(models[0].Model == nil ||
			large.ModelCfg.Provider != models[0].ModelCfg.Provider ||
			large.ModelCfg.Model != models[0].ModelCfg.Model) {
		models = append(models, large)
	}
	timeout := r.timeout
	if timeout <= 0 {
		timeout = scopeResolverTimeout
	}
	var lastErr error
	for i, model := range models {
		if model.Model == nil {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		res, err := r.consult(callCtx, model, prompt, cands)
		cancel()
		if err == nil {
			return res, nil
		}
		lastErr = err
		if i+1 < len(models) {
			slog.Debug("Scope resolver consult failed; trying next model", "err", err)
		}
	}
	if lastErr == nil {
		lastErr = errors.New("scope resolver: no model configured")
	}
	return nil, lastErr
}

func (r *llmScopeResolver) consult(ctx context.Context, model Model, prompt string, cands []ResolverCandidate) (*ScopeResolution, error) {
	// 1024 is a floor, not a target: reasoning models spend output
	// tokens on reasoning_content before any content text — a 120
	// cap truncated every call with an empty body (observed on
	// deepseek-flash: reasoning_tokens=120, finish_reason=length).
	// /no_think is best-effort — endpoints may not honor it — so the
	// budget must survive a reasoning preamble plus the small JSON
	// verdict.
	tok := int64(1024)
	if model.CatwalkCfg.CanReason {
		tok = max(tok, model.CatwalkCfg.DefaultMaxTokens)
	}
	resAgent := fantasy.NewAgent(
		model.Model,
		fantasy.WithSystemPrompt(scopeResolverSystemPrompt+"\n /no_think"),
		fantasy.WithMaxOutputTokens(tok),
		fantasy.WithUserAgent(userAgent),
	)
	resp, err := resAgent.Stream(ctx, fantasy.AgentStreamCall{
		Prompt:  scopeResolverUserPrompt(prompt, cands),
		Headers: sessionHeaders(r.sessionID),
	})
	r.a.recordUsage(r.sessionID, resp)
	if err != nil {
		return nil, fmt.Errorf("scope resolver: %w", err)
	}
	if resp == nil || resp.Response.FinishReason == fantasy.FinishReasonLength {
		return nil, errors.New("scope resolver: empty or truncated response")
	}
	return parseScopeResolution(resp.Response.Content.Text(), r.workDir)
}

// scopeResolverUserPrompt renders the bounded candidate projection —
// pool, normalized command, cwd, headline, file hints, each field
// rune-capped so no row dominates the context.
func scopeResolverUserPrompt(prompt string, cands []ResolverCandidate) string {
	var b strings.Builder
	b.WriteString("Open and recent failures (context only — do not name them):\n")
	for i, c := range cands {
		if i >= scopeResolverMaxCandidates {
			break
		}
		fmt.Fprintf(&b, "- pool=%s cmd=%q cwd=%q headline=%q files=%q\n",
			c.Pool,
			truncateRunes(c.Cmd, scopeResolverFieldRunes),
			truncateRunes(c.CWD, scopeResolverFieldRunes),
			truncateRunes(c.Headline, scopeResolverFieldRunes),
			truncateRunes(strings.Join(c.Files, ","), scopeResolverFieldRunes))
	}
	b.WriteString("\nUser request:\n")
	b.WriteString(truncateRunes(prompt, scopeResolverPromptRunes))
	return b.String()
}

// parseScopeResolution validates the wire output against the closed
// contract — this is the deterministic gate between model text and
// selector scope. Every check fails the whole resolution (abstain),
// never partially.
func parseScopeResolution(text, workDir string) (*ScopeResolution, error) {
	obj := extractJSONObject(text)
	if obj == "" {
		return nil, errors.New("scope resolver: no JSON object in output")
	}
	var w scopeResolutionWire
	dec := json.NewDecoder(strings.NewReader(obj))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("scope resolver: decode: %w", err)
	}
	if w.AboutFailure == nil {
		return nil, errors.New("scope resolver: about_failure missing")
	}
	res := &ScopeResolution{About: *w.AboutFailure}
	if !res.About {
		if len(w.IncludePaths) > 0 || len(w.ExcludePaths) > 0 || w.Kind != "" {
			return nil, errors.New("scope resolver: about_failure=false must leave scope fields empty")
		}
		return res, nil
	}
	var err error
	if res.Include, err = resolverPaths(w.IncludePaths, workDir); err != nil {
		return nil, err
	}
	if res.Exclude, err = resolverPaths(w.ExcludePaths, workDir); err != nil {
		return nil, err
	}
	if w.Kind != "" {
		if !slices.Contains(verificationKinds, w.Kind) {
			return nil, fmt.Errorf("scope resolver: kind %q not in %v", w.Kind, verificationKinds)
		}
		res.Kind = w.Kind
	}
	return res, nil
}

// resolverPaths normalizes each entry through the same normScopePath
// the deterministic layer uses, then holds resolver output to the
// stricter contract untrusted text earns: repo-relative, inside the
// working directory, and real on disk.
func resolverPaths(paths []string, workDir string) ([]string, error) {
	var out []string
	for _, p := range paths {
		norm := normScopePath(p)
		if norm == "" || workDir == "" {
			return nil, fmt.Errorf("scope resolver: path %q has no expressible repo scope", p)
		}
		full := filepath.Join(workDir, filepath.FromSlash(norm))
		rel, err := filepath.Rel(workDir, full)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("scope resolver: path %q escapes the working directory", p)
		}
		if _, err := os.Stat(full); err != nil {
			return nil, fmt.Errorf("scope resolver: path %q does not exist", p)
		}
		out = append(out, norm)
	}
	return out, nil
}

// extractJSONObject returns the first balanced {...} span in the
// output, tolerating leading prose and code fences. Braces inside
// strings do not count.
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case c == '{' && !inStr:
			depth++
		case c == '}' && !inStr:
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// scopeResolverCacheKey hashes the normalized prompt plus each
// candidate's content — a resolution is only as fresh as the set it
// reasoned over.
func scopeResolverCacheKey(prompt string, cands []ResolverCandidate) string {
	h := sha256.New()
	h.Write([]byte(strings.Join(strings.Fields(strings.ToLower(prompt)), " ")))
	for _, c := range cands {
		fmt.Fprintf(h, "\x00%s\x00%s\x00%s\x00%s\x00%s",
			c.Pool, c.Cmd, c.CWD, c.Headline, strings.Join(c.Files, "\x01"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
