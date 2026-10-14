package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/stretchr/testify/require"
)

// stubResolver is the canned-answer ScopeResolver the selector-level
// tests drive — it also records whether the gate let a call through
// at all, which is half the contract.
type stubResolver struct {
	res    *ScopeResolution
	err    error
	calls  int
	prompt string
	cands  []ResolverCandidate
}

func (s *stubResolver) ResolveScope(_ context.Context, prompt string, cands []ResolverCandidate) (*ScopeResolution, error) {
	s.calls++
	s.prompt = prompt
	s.cands = cands
	return s.res, s.err
}

// jaPrompt carries no ASCII scope signal — no path, no referent
// kind, no identifier — so the deterministic layers leave it
// unresolved and the resolver gate opens whenever open failures
// exist.
const jaPrompt = "テストが失敗している"

func TestSelectResolver_Gating(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "decoy"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "decoy", "x.go"), []byte("x"), 0o644))

	tests := []struct {
		name    string
		prompt  string
		open    []cmdlog.Failure
		workDir string
		want    int
	}{
		{
			name:    "explicit scope skips the resolver",
			prompt:  "fix decoy/x.go",
			open:    []cmdlog.Failure{mkFailure("go test .", "decoy")},
			workDir: dir,
			want:    0,
		},
		{
			name:    "bound referent kinds skip the resolver",
			prompt:  "the tests fail",
			open:    []cmdlog.Failure{mkFailure("go test .", ".")},
			workDir: dir,
			want:    0,
		},
		{
			name:    "empty open pool skips the resolver",
			prompt:  jaPrompt,
			open:    nil,
			workDir: dir,
			want:    0,
		},
		{
			// The repair-retry sentinel: a mid-chain rebuild empties
			// the prompt so nothing binds — the resolver must not see
			// a candidate list with no question attached.
			name:    "empty prompt skips the resolver",
			prompt:  "",
			open:    []cmdlog.Failure{mkFailure("go test .", ".")},
			workDir: dir,
			want:    0,
		},
		{
			name:    "whitespace-only prompt skips the resolver",
			prompt:  "   \n\t",
			open:    []cmdlog.Failure{mkFailure("go test .", ".")},
			workDir: dir,
			want:    0,
		},
		{
			name:    "unresolved prompt with open rows invokes it",
			prompt:  jaPrompt,
			open:    []cmdlog.Failure{mkFailure("go test .", ".")},
			workDir: dir,
			want:    1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubResolver{res: &ScopeResolution{About: false}}
			_, decisions := selectOpenFailuresResolver(context.Background(), tt.prompt,
				tt.open, tt.workDir, 5, stub)
			require.Equal(t, tt.want, stub.calls)
			if tt.want == 0 {
				for _, d := range decisions {
					require.NotEqual(t, failResolverNoFailure, d.Reason,
						"resolver verdict emitted without a resolver call")
				}
			}
		})
	}
}

func TestSelectResolver_AboutFalse(t *testing.T) {
	t.Parallel()
	stub := &stubResolver{res: &ScopeResolution{About: false}}
	admitted, decisions := selectOpenFailuresResolver(context.Background(), jaPrompt,
		[]cmdlog.Failure{
			mkFailure("go test .", "."),
			mkFailure("go build .", "."),
		}, "", 5, stub)
	require.Equal(t, 1, stub.calls)
	require.Empty(t, admitted)
	for _, d := range decisions {
		require.Equal(t, failResolverNoFailure, d.Reason)
		require.Equal(t, "resolver-no-failure", d.Route)
		require.Equal(t, settledResolver, d.SettledBy)
	}
}

func TestSelectResolver_IncludeScopes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "decoy"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "other"), 0o755))

	stub := &stubResolver{res: &ScopeResolution{About: true, Include: []string{"decoy"}}}
	admitted, decisions := selectOpenFailuresResolver(context.Background(), jaPrompt,
		[]cmdlog.Failure{
			mkFailure("go test .", "decoy"),
			mkFailure("go test .", "other"),
		}, dir, 5, stub)
	require.Equal(t, 1, stub.calls)
	require.Len(t, admitted, 1)
	require.Equal(t, "decoy", admitted[0].CWD)

	bySig := map[string]FailureDecision{}
	for _, d := range decisions {
		bySig[d.Signature] = d
	}
	admitDec := bySig["go test .@decoy"]
	dropDec := bySig["go test .@other"]
	require.True(t, admitDec.Admit)
	require.Equal(t, "default-admit", admitDec.Route)
	require.Equal(t, settledResolver, admitDec.SettledBy)
	require.Equal(t, failOutOfScope, dropDec.Reason)
	require.Equal(t, "explicit-out-of-scope", dropDec.Route)
	require.Equal(t, settledResolver, dropDec.SettledBy)
}

func TestSelectResolver_KindBinds(t *testing.T) {
	t.Parallel()
	stub := &stubResolver{res: &ScopeResolution{About: true, Kind: "test"}}
	admitted, decisions := selectOpenFailuresResolver(context.Background(), jaPrompt,
		[]cmdlog.Failure{
			mkFailure("go test .", "."),
			mkFailure("go build .", "."),
		}, "", 5, stub)
	require.Equal(t, 1, stub.calls)
	require.Len(t, admitted, 1)
	require.Equal(t, "go test .", admitted[0].Cmd)

	for _, d := range decisions {
		require.Equal(t, settledResolver, d.SettledBy, "%s settled %s", d.Cmd, d.SettledBy)
		if d.Cmd == "go build ." {
			require.Equal(t, failKindMismatch, d.Reason)
			require.Equal(t, "referent-kind-mismatch", d.Route)
		}
	}
}

func TestSelectResolver_ExcludeVetoes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "decoy"), 0o755))

	// A positive identifier mention would admit the row at
	// ident-admit — the resolver-supplied exclude must still veto
	// it. negated-scope precedes the ident routes for exactly this
	// reason.
	stub := &stubResolver{res: &ScopeResolution{About: true, Exclude: []string{"decoy"}}}
	row := mkFailureHeadline("go test .", "decoy", "--- FAIL: TestFoo")
	admitted, decisions := selectOpenFailuresResolver(context.Background(),
		"fix TestFoo please", []cmdlog.Failure{row}, dir, 5, stub)
	require.Equal(t, 1, stub.calls)
	require.Empty(t, admitted)
	require.Equal(t, failNegatedScope, decisions[0].Reason)
	require.Equal(t, "negated-scope", decisions[0].Route)
	require.Equal(t, settledResolver, decisions[0].SettledBy)
}

func TestSelectResolver_Abstains(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		stub *stubResolver
	}{
		{"error", &stubResolver{err: errors.New("boom")}},
		{"nil resolution", &stubResolver{res: nil}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			admitted, decisions := selectOpenFailuresResolver(context.Background(), jaPrompt,
				[]cmdlog.Failure{mkFailure("go test .", ".")}, "", 5, tt.stub)
			require.Equal(t, 1, tt.stub.calls)
			require.Empty(t, admitted)
			require.Equal(t, failLangUnsupported, decisions[0].Reason)
			require.Equal(t, settledLexicon, decisions[0].SettledBy)
		})
	}
}

func TestSelectResolver_ShadowedKeepsPrecedence(t *testing.T) {
	t.Parallel()
	// about=false rejects every row — except a shadowed command row,
	// whose verdict is a state fact ("the open twin subsumes it")
	// and precedes the relevance verdict in table order.
	stub := &stubResolver{res: &ScopeResolution{About: false}}
	pools := memoryPools{
		open: []cmdlog.Failure{mkFailure("go test .", ".")},
		commands: []cmdlog.Command{
			{CmdNorm: "go test .", CWD: ".", Kind: "test", LastExit: 1, FailCount: 1},
		},
	}
	_, decisions := selectMemory(context.Background(), jaPrompt, pools, "",
		memoryRenderLimits{}, stub)
	require.Equal(t, 1, stub.calls)
	var openDec, cmdDec FailureDecision
	for _, d := range decisions {
		if d.Pool == poolOpen {
			openDec = d
		} else {
			cmdDec = d
		}
	}
	require.Equal(t, failResolverNoFailure, openDec.Reason)
	require.Equal(t, failShadowed, cmdDec.Reason)
	require.Equal(t, "shadowed", cmdDec.Route)
	require.Equal(t, settledState, cmdDec.SettledBy)
}

// A positively-named identifier is direct evidence — it outranks the
// resolver's about=false verdict. resolver-no-failure sits after the
// ident block so model judgment fills gaps rather than overturning
// L1 bindings.
func TestSelectResolver_IdentBeatsAboutFalse(t *testing.T) {
	t.Parallel()
	stub := &stubResolver{res: &ScopeResolution{About: false}}
	row := mkFailureHeadline("go test .", ".", "--- FAIL: TestFoo")
	admitted, decisions := selectOpenFailuresResolver(context.Background(),
		"fix TestFoo please", []cmdlog.Failure{row}, "", 5, stub)
	require.Equal(t, 1, stub.calls)
	require.Len(t, admitted, 1)
	require.Equal(t, failAdmit, decisions[0].Reason)
	require.Equal(t, "ident-admit", decisions[0].Route)
	require.Equal(t, settledIdentifier, decisions[0].SettledBy)
}

func TestSelectResolver_CandidateProjectionIsReadOnly(t *testing.T) {
	t.Parallel()
	stub := &stubResolver{res: &ScopeResolution{About: false}}
	var open []cmdlog.Failure
	for range 20 {
		open = append(open, mkFailure("go test .", "."))
	}
	selectOpenFailuresResolver(context.Background(), jaPrompt, open, "", 25, stub)
	require.Len(t, stub.cands, 20)
	for _, c := range stub.cands {
		require.Equal(t, poolOpen, c.Pool)
		require.Equal(t, "go test .", c.Cmd)
	}
	// The rendered prompt caps the listing — the projection carries
	// every row, the model sees the bounded head.
	rendered := scopeResolverUserPrompt(jaPrompt, stub.cands)
	require.Equal(t, scopeResolverMaxCandidates,
		strings.Count(rendered, "- pool="), "resolver prompt must cap the candidate listing")
}

// --- llmScopeResolver: the model-backed layer end to end ---

// textModelStreams builds a fakeLanguageModel streaming the canned
// text as a single delta, counting how many times the model body
// actually ran — the cache tests read the counter.
func textModelStreams(text string, calls *atomic.Int32) *fakeLanguageModel {
	return &fakeLanguageModel{stream: func(yield func(fantasy.StreamPart) bool) {
		if calls != nil {
			calls.Add(1)
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t", Delta: text}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "t"}) {
			return
		}
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			Usage:        fantasy.Usage{OutputTokens: 10, TotalTokens: 10},
			FinishReason: fantasy.FinishReasonStop,
		})
	}}
}

func resolverTestAgent(lm fantasy.LanguageModel) *sessionAgent {
	return &sessionAgent{
		smallModel:     csync.NewValue(Model{Model: lm}),
		largeModel:     csync.NewValue(Model{Model: lm}),
		resolverStates: csync.NewMap[string, *resolverState](),
	}
}

// resolverForSession mirrors scopeResolverFor's state wiring for the
// unit tests — the per-session cache/budget live on resolverStates.
func resolverForSession(a *sessionAgent, workDir string, budget int) *llmScopeResolver {
	state := a.resolverStates.GetOrSet("s1", func() *resolverState {
		return &resolverState{cache: csync.NewMap[string, *ScopeResolution]()}
	})
	return &llmScopeResolver{a: a, state: state, sessionID: "s1", workDir: workDir, budget: budget}
}

func TestLLMScopeResolver_ValidResolution(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "decoy"), 0o755))

	a := resolverTestAgent(textModelStreams(
		`{"about_failure":true,"include_paths":["decoy"],"exclude_paths":[],"kind":"test"}`, nil))
	r := resolverForSession(a, dir, 8)
	res, err := r.ResolveScope(context.Background(), jaPrompt,
		[]ResolverCandidate{{Pool: poolOpen, Cmd: "go test .", CWD: "decoy"}})
	require.NoError(t, err)
	require.True(t, res.About)
	require.Equal(t, []string{"decoy"}, res.Include)
	require.Equal(t, "test", res.Kind)
}

func TestLLMScopeResolver_FencedJSON(t *testing.T) {
	t.Parallel()
	a := resolverTestAgent(textModelStreams(
		"Sure! Here you go:\n```json\n{\"about_failure\":false}\n```", nil))
	r := resolverForSession(a, "", 8)
	res, err := r.ResolveScope(context.Background(), jaPrompt, nil)
	require.NoError(t, err)
	require.False(t, res.About)
}

func TestLLMScopeResolver_InvalidOutputAbstains(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "real"), 0o755))

	tests := []struct {
		name string
		text string
	}{
		{"not json", "I cannot determine that."},
		{"missing about_failure", `{"include_paths":["real"]}`},
		{"invalid kind", `{"about_failure":true,"kind":"deploy"}`},
		{"nonexistent path", `{"about_failure":true,"include_paths":["ghost"]}`},
		{"traversal path", `{"about_failure":true,"include_paths":["../outside"]}`},
		{"absolute path", `{"about_failure":true,"include_paths":["/etc"]}`},
		{"about false with scope", `{"about_failure":false,"kind":"test"}`},
		{"unknown field", `{"about_failure":true,"row":3}`},
		{"truncated json", `{"about_failure":true,"include_paths":["real"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := resolverTestAgent(textModelStreams(tt.text, nil))
			r := resolverForSession(a, dir, 8)
			res, err := r.ResolveScope(context.Background(), jaPrompt,
				[]ResolverCandidate{{Pool: poolOpen, Cmd: "go test .", CWD: "."}})
			require.Error(t, err)
			require.Nil(t, res)
		})
	}
}

func TestLLMScopeResolver_Budget(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	a := resolverTestAgent(textModelStreams(`{"about_failure":false}`, &calls))
	r := resolverForSession(a, "", 1)
	cands := []ResolverCandidate{{Pool: poolOpen, Cmd: "go test .", CWD: "."}}

	_, err := r.ResolveScope(context.Background(), "prompt one", cands)
	require.NoError(t, err)
	_, err = r.ResolveScope(context.Background(), "prompt two", cands)
	require.Error(t, err)
	require.Equal(t, int32(1), calls.Load(), "the second call spent a model call past budget")

	// A cached verdict is not a model call — it still serves after
	// the budget trips.
	res, err := r.ResolveScope(context.Background(), "prompt one", cands)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, int32(1), calls.Load())
}

func TestLLMScopeResolver_Cache(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	a := resolverTestAgent(textModelStreams(`{"about_failure":false}`, &calls))
	r := resolverForSession(a, "", 8)
	cands := []ResolverCandidate{{Pool: poolOpen, Cmd: "go test .", CWD: "."}}

	res1, err := r.ResolveScope(context.Background(), "Fix  THE thing", cands)
	require.NoError(t, err)
	// Normalization folds case and whitespace into the same key.
	res2, err := r.ResolveScope(context.Background(), "fix the   thing", cands)
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load())
	require.Same(t, res1, res2)

	// A changed candidate set is a different question.
	_, err = r.ResolveScope(context.Background(), "fix the thing",
		append(cands, ResolverCandidate{Pool: poolOpen, Cmd: "go build .", CWD: "."}))
	require.NoError(t, err)
	require.Equal(t, int32(2), calls.Load())
}

func TestLLMScopeResolver_Timeout(t *testing.T) {
	t.Parallel()
	// The stream body parks until its context dies — exactly the
	// hung-model case the resolver timeout exists for.
	lm := &fakeLanguageModel{stream: func(yield func(fantasy.StreamPart) bool) {
		<-time.After(200 * time.Millisecond)
	}}
	a := resolverTestAgent(lm)
	r := resolverForSession(a, "", 8)
	r.timeout = 30 * time.Millisecond
	start := time.Now()
	res, err := r.ResolveScope(context.Background(), jaPrompt,
		[]ResolverCandidate{{Pool: poolOpen, Cmd: "go test .", CWD: "."}})
	require.Error(t, err)
	require.Nil(t, res)
	require.Less(t, time.Since(start), 2*time.Second)
}

func TestLLMScopeResolver_FallsBackToLarge(t *testing.T) {
	t.Parallel()
	// A configured-but-broken small provider (bad key, zero credit
	// balance — the #333 eval abstain) must not pin the layer off:
	// the consult retries on the large model, matching the title
	// generator's "trying next" order.
	small := &fakeLanguageModel{stream: func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{
			Type:  fantasy.StreamPartTypeError,
			Error: errors.New("400 credit balance too low"),
		})
	}}
	var largeCalls atomic.Int32
	large := textModelStreams(`{"about_failure":false}`, &largeCalls)
	a := &sessionAgent{
		smallModel: csync.NewValue(Model{Model: small,
			ModelCfg: config.SelectedModel{Provider: "anthropic", Model: "haiku"}}),
		largeModel: csync.NewValue(Model{Model: large,
			ModelCfg: config.SelectedModel{Provider: "opencode-go", Model: "muse"}}),
		resolverStates: csync.NewMap[string, *resolverState](),
	}
	r := resolverForSession(a, "", 8)
	res, err := r.ResolveScope(context.Background(), jaPrompt,
		[]ResolverCandidate{{Pool: poolOpen, Cmd: "go test .", CWD: "."}})
	require.NoError(t, err)
	require.False(t, res.About)
	require.Equal(t, int32(1), largeCalls.Load())
}

func TestLLMScopeResolver_NoModel(t *testing.T) {
	t.Parallel()
	a := resolverTestAgent(nil)
	r := resolverForSession(a, "", 8)
	res, err := r.ResolveScope(context.Background(), jaPrompt, nil)
	require.Error(t, err)
	require.Nil(t, res)
}

// The audit outcome must reflect what the layer actually produced —
// "resolved" only when a verdict (including the no-failure reject)
// came back, "abstain" for every failed or empty consult, and empty
// when the gate never opened so a dead resolver is visible in eval
// records instead of reading as never-engaged.
func TestOutcomeResolver(t *testing.T) {
	t.Parallel()

	verdict := &ScopeResolution{About: true}
	for _, tc := range []struct {
		name   string
		inner  *stubResolver
		invoke bool
		want   string
	}{
		{"verdict stamps resolved", &stubResolver{res: verdict}, true, resolverResolved},
		{"no-failure reject stamps resolved", &stubResolver{res: &ScopeResolution{About: false}}, true, resolverResolved},
		{"error stamps abstain", &stubResolver{err: errors.New("boom")}, true, resolverAbstain},
		{"nil verdict stamps abstain", &stubResolver{}, true, resolverAbstain},
		{"never invoked stays empty", &stubResolver{res: verdict}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := ""
			r := outcomeResolver{inner: tc.inner, out: &out}
			if tc.invoke {
				res, err := r.ResolveScope(context.Background(), "p", nil)
				require.Equal(t, tc.inner.res, res)
				require.Equal(t, tc.inner.err, err)
			}
			require.Equal(t, tc.want, out)
		})
	}
}

// The output budget must survive a reasoning preamble: endpoints may
// spend output tokens on reasoning_content before the JSON verdict
// even when catwalk says CanReason=false and /no_think is attached,
// so the floor is 1024 for every model and the configured default
// when it is larger.
func TestLLMScopeResolver_TokenFloor(t *testing.T) {
	t.Parallel()

	jaCands := []ResolverCandidate{{Pool: poolOpen, Cmd: "go test .", CWD: "."}}
	run := func(t *testing.T, cfg catwalk.Model) int64 {
		t.Helper()
		lm := textModelStreams(`{"about_failure":true}`, nil)
		a := resolverTestAgent(lm)
		a.smallModel = csync.NewValue(Model{Model: lm, CatwalkCfg: cfg})
		r := resolverForSession(a, "", 8)
		_, err := r.ResolveScope(context.Background(), jaPrompt, jaCands)
		require.NoError(t, err)
		require.NotNil(t, lm.streamCall)
		require.NotNil(t, lm.streamCall.MaxOutputTokens)
		return *lm.streamCall.MaxOutputTokens
	}

	t.Run("non-reasoning model gets the floor", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, int64(1024), run(t, catwalk.Model{}))
	})
	t.Run("reasoning model keeps its larger default", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, int64(8192), run(t, catwalk.Model{
			CanReason:        true,
			DefaultMaxTokens: 8192,
		}))
	})
	t.Run("reasoning model with a small default gets the floor", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, int64(1024), run(t, catwalk.Model{
			CanReason:        true,
			DefaultMaxTokens: 64,
		}))
	})
}
