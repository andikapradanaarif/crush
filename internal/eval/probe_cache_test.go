package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// fakeCacheEndpoint serves minimal openai-compat SSE responses: one
// text chunk, then a usage chunk carrying both OpenAI-style
// prompt_tokens_details.cached_tokens (what fantasy normalizes) and
// DeepSeek-style prompt_cache_hit_tokens (what only the raw capture
// sees). Header assertions are collected and checked by the caller —
// require.* in a handler goroutine is unsafe (FailNow only works on
// the test goroutine).
func fakeCacheEndpoint(t *testing.T, hits *atomic.Int64, hdrErrs *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Method != http.MethodPost {
			*hdrErrs = append(*hdrErrs, fmt.Sprintf("method %s", r.Method))
		}
		sid, sa := r.Header.Get("x-session-id"), r.Header.Get("x-session-affinity")
		if sid == "" || sa == "" {
			*hdrErrs = append(*hdrErrs, "missing affinity headers")
		} else if sid != sa {
			*hdrErrs = append(*hdrErrs, fmt.Sprintf("affinity mismatch %q != %q", sid, sa))
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		h := hits.Load()
		fmt.Fprintf(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`+"\n\n")
		fmt.Fprintf(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":50000,"completion_tokens":1,"total_tokens":50001,"prompt_tokens_details":{"cached_tokens":%d},"prompt_cache_hit_tokens":%d,"prompt_cache_miss_tokens":%d}}`+"\n\n", h, h, 50000-h)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// readProbeRecords returns the JSONL's send rows, skipping the meta
// header.
func readProbeRecords(t *testing.T, path string) []ProbeRecord {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	var recs []ProbeRecord
	dec := json.NewDecoder(f)
	for dec.More() {
		var r ProbeRecord
		require.NoError(t, dec.Decode(&r))
		if r.Kind == "meta" {
			continue
		}
		recs = append(recs, r)
	}
	return recs
}

// readProbeMeta decodes the first JSONL row as the meta record.
func readProbeMeta(t *testing.T, path string) ProbeMeta {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	var meta ProbeMeta
	require.NoError(t, json.NewDecoder(f).Decode(&meta))
	return meta
}

func TestProbeCache_EndToEnd(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	hits.Store(49000)
	var mu sync.Mutex
	var hdrErrs []string
	srv := fakeCacheEndpoint(t, &hits, &hdrErrs, &mu)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "probe.jsonl")
	err := RunProbeCache(t.Context(), ProbeCacheConfig{
		BaseURL:      srv.URL + "/v1",
		APIKey:       "test-key",
		Model:        "test-model",
		TargetTokens: 2000,
		Repeats:      1,
		MaxRequests:  100,
		OutPath:      out,
		DelayScale:   0,
		Seed:         42,
	})
	require.NoError(t, err)
	require.Empty(t, hdrErrs)

	meta := readProbeMeta(t, out)
	require.Equal(t, "meta", meta.Kind)
	require.EqualValues(t, 42, meta.Seed)
	require.Equal(t, "test-model", meta.Model)
	require.NotEmpty(t, meta.AffinityHash)
	require.Equal(t, probeConditions, meta.Conditions)

	recs := readProbeRecords(t, out)
	// 6 conditions × 1 repeat → 6 warms + 6 measured + 1 control.
	var warms, measured, controls int
	for _, r := range recs {
		switch r.Kind {
		case "warm":
			warms++
			// Warm rows carry the block's condition and no mutation.
			require.NotEqual(t, "base", r.Condition)
			require.Equal(t, -1, r.MutatedIndex)
		case "measured":
			measured++
		case "control":
			controls++
		}
		require.Empty(t, r.Error)
		require.Equal(t, 1, r.Attempts)
		require.NotEmpty(t, r.RequestHash)
		require.EqualValues(t, 49000, r.Usage.CacheReadTokens)
		// Raw capture carries the DeepSeek-style field fantasy drops.
		require.Contains(t, r.RawUsage, "prompt_cache_hit_tokens")
	}
	require.Equal(t, 6, warms)
	require.Equal(t, 6, measured)
	require.Equal(t, 1, controls)

	// Identical/fresh-process share the base request hash; mutated
	// conditions each produce a distinct one.
	condHashes := map[string]string{}
	for _, r := range recs {
		if r.Kind == "measured" {
			condHashes[r.Condition] = r.RequestHash
		}
	}
	require.Equal(t, condHashes["identical"], condHashes["fresh-process"])
	require.NotEqual(t, condHashes["identical"], condHashes["early-system"])
	require.NotEqual(t, condHashes["identical"], condHashes["notebook"])
	require.NotEqual(t, condHashes["identical"], condHashes["late-history"])
	require.NotEqual(t, condHashes["identical"], condHashes["append"])

	// Every send produced a raw body file.
	rawFiles, err := filepath.Glob(filepath.Join(filepath.Dir(out), "*-raw", "*.resp"))
	require.NoError(t, err)
	require.Len(t, rawFiles, len(recs))
}

func TestProbeCache_SpendCap(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	var mu sync.Mutex
	var hdrErrs []string
	srv := fakeCacheEndpoint(t, &hits, &hdrErrs, &mu)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "probe.jsonl")
	err := RunProbeCache(t.Context(), ProbeCacheConfig{
		BaseURL:     srv.URL + "/v1",
		APIKey:      "k",
		Model:       "m",
		Repeats:     5,
		MaxRequests: 7,
		OutPath:     out,
		DelayScale:  0,
		Seed:        1,
	})
	// Cap hit mid-schedule is an incomplete run, not a clean exit.
	require.Error(t, err)
	require.Contains(t, err.Error(), "spend cap")
	require.Len(t, readProbeRecords(t, out), 7)
}

// TestProbeCache_TransportFailure exercises the path where a send dies
// before RoundTrip produces a response — the record must not inherit
// the previous send's capture (hash, usage, body).
func TestProbeCache_TransportFailure(t *testing.T) {
	t.Parallel()
	// A listener that accepts then immediately closes gives a fast
	// transport-level failure (no HTTP response ever arrives).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, ln.Close())

	out := filepath.Join(t.TempDir(), "probe.jsonl")
	err = RunProbeCache(t.Context(), ProbeCacheConfig{
		BaseURL:     "http://" + ln.Addr().String() + "/v1",
		APIKey:      "k",
		Model:       "m",
		Repeats:     5,
		MaxRequests: 100,
		OutPath:     out,
		DelayScale:  0,
		Seed:        1,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "consecutive send errors")
	recs := readProbeRecords(t, out)
	require.NotEmpty(t, recs)
	for _, r := range recs {
		require.NotEmpty(t, r.Error)
		require.Zero(t, r.Attempts)
		// The stale-capture bug stamped these with the prior send's
		// data — they must stay empty on transport failure.
		require.Empty(t, r.RequestHash)
		require.Empty(t, r.RawUsage)
	}
}

// TestProbeCache_HTTPError covers the deterministic provider-error
// path: RoundTrip ran (attempts=1), so the capture IS this send's —
// the record must carry its request hash and the error body lands in
// the raw file, while the error is still recorded.
func TestProbeCache_HTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"probe-400-marker","type":"invalid_request_error"}}`)
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "probe.jsonl")
	err := RunProbeCache(t.Context(), ProbeCacheConfig{
		BaseURL:      srv.URL + "/v1",
		APIKey:       "k",
		Model:        "m",
		TargetTokens: 2000,
		Repeats:      5,
		MaxRequests:  100,
		OutPath:      out,
		DelayScale:   0,
		Seed:         1,
	})
	// Every send 400s → three consecutive errors abort the run.
	require.Error(t, err)
	require.Contains(t, err.Error(), "consecutive send errors")
	recs := readProbeRecords(t, out)
	require.Len(t, recs, 3)
	for _, r := range recs {
		require.NotEmpty(t, r.Error)
		require.Equal(t, 1, r.Attempts)
		require.NotEmpty(t, r.RequestHash)
		require.NotEmpty(t, r.RawFile)
		body, rerr := os.ReadFile(r.RawFile)
		require.NoError(t, rerr)
		require.Contains(t, string(body), "probe-400-marker")
	}
}

// TestProbeCache_DryRun verifies the schedule path sends nothing and
// writes no artifact.
func TestProbeCache_DryRun(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "probe.jsonl")
	err := RunProbeCache(t.Context(), ProbeCacheConfig{
		DryRun:       true,
		TargetTokens: 2000,
		OutPath:      out,
		Seed:         1,
	})
	require.NoError(t, err)
	_, statErr := os.Stat(out)
	require.True(t, os.IsNotExist(statErr))
}

func TestProbeSummarize(t *testing.T) {
	t.Parallel()
	mk := func(kind, cond string, input, cached int64) ProbeRecord {
		return ProbeRecord{
			Kind: kind, Condition: cond,
			Usage: fantasy.Usage{InputTokens: input, CacheReadTokens: cached},
		}
	}
	retried := mk("measured", "notebook", 25000, 25000)
	retried.Attempts = 2 // Counted but flagged — may inherit a warm cache.
	recs := []ProbeRecord{
		mk("warm", "identical", 50000, 0), // Excluded — establishes cache.
		mk("measured", "identical", 1000, 49000),
		mk("control", "identical", 2000, 48000),
		mk("delayed", "identical", 5000, 45000),
		mk("measured", "notebook", 25000, 25000),
		retried,
		{Kind: "measured", Condition: "notebook", Error: "conn reset"}, // n_err — must not dilute hit%.
	}
	var buf bytes.Buffer
	ProbeSummarize(&buf, recs)
	s := buf.String()
	// hit% divides by the true prompt (uncached+cached), not the
	// uncached remainder — 49K/50K must print ~98%, not 4900%.
	require.Contains(t, s, "identical")
	require.Contains(t, s, "97.0%") // (49000/50000 + 48000/50000)/2
	require.Contains(t, s, "identical (delayed)")
	require.Contains(t, s, "90.0%") // 45000/50000
	// The errored row counts in n_err but stays out of n and hit% —
	// with it included notebook would read 33.3% instead of 50.0%.
	require.Regexp(t, `notebook\s+2\s+1\s+1`, s)
	require.Contains(t, s, "50.0%")
}

func TestBuildProbeBundle_MutationLocality(t *testing.T) {
	t.Parallel()
	b := buildProbeBundle(rand.New(rand.NewSource(7)), 2000)
	require.Len(t, b.base, 26) // sys + notebook + 24 history.

	for cond, wantIdx := range b.mutatedIdx {
		v := b.variants[cond]
		switch cond {
		case "identical", "fresh-process":
			require.Equal(t, b.base, v)
		case "append":
			require.Greater(t, len(v), len(b.base))
			require.Equal(t, []fantasy.Message(b.base), []fantasy.Message(v[:len(b.base)]))
		default:
			require.Equal(t, len(b.base), len(v))
			for i := range b.base {
				if i == wantIdx {
					require.NotEqual(t, b.base[i], v[i], cond)
				} else {
					require.Equal(t, b.base[i], v[i], cond)
				}
			}
			// Mutations preserve byte length (approx token count).
			require.Len(t, v[wantIdx].Content[0].(fantasy.TextPart).Text,
				len(b.base[wantIdx].Content[0].(fantasy.TextPart).Text), cond)
		}
	}
}

func TestLastSSEUsage(t *testing.T) {
	t.Parallel()
	body := `data: {"usage":null}

data: {"usage":{"prompt_tokens":100,"prompt_cache_hit_tokens":80}}

data: [DONE]
`
	u := lastSSEUsage([]byte(body))
	require.EqualValues(t, 100, u["prompt_tokens"])
	require.EqualValues(t, 80, u["prompt_cache_hit_tokens"])
	require.Nil(t, lastSSEUsage([]byte("data: [DONE]\n\n")))
}
