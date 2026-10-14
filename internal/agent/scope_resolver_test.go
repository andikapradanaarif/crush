package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/cmdlog"
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

func TestSelectResolver_CandidateProjectionIsBounded(t *testing.T) {
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

func TestLLMScopeResolver_NoModel(t *testing.T) {
	t.Parallel()
	a := resolverTestAgent(nil)
	r := resolverForSession(a, "", 8)
	res, err := r.ResolveScope(context.Background(), jaPrompt, nil)
	require.Error(t, err)
	require.Nil(t, res)
}
