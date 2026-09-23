package eval

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// fakeCacheEndpoint serves minimal openai-compat SSE responses: one
// text chunk, then a usage chunk carrying both OpenAI-style
// prompt_tokens_details.cached_tokens (what fantasy normalizes) and
// DeepSeek-style prompt_cache_hit_tokens (what only the raw capture
// sees).
func fakeCacheEndpoint(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.NotEmpty(t, r.Header.Get("x-session-affinity"))
		w.Header().Set("Content-Type", "text/event-stream")
		h := hits.Load()
		fmt.Fprintf(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`+"\n\n")
		fmt.Fprintf(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":50000,"completion_tokens":1,"total_tokens":50001,"prompt_tokens_details":{"cached_tokens":%d},"prompt_cache_hit_tokens":%d,"prompt_cache_miss_tokens":%d}}`+"\n\n", h, h, 50000-h)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

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
		recs = append(recs, r)
	}
	return recs
}

func TestProbeCache_EndToEnd(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	hits.Store(49000)
	srv := fakeCacheEndpoint(t, &hits)
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

	recs := readProbeRecords(t, out)
	// 6 conditions × 1 repeat → 6 warms + 6 measured + 1 control.
	var warms, measured, controls int
	byHash := map[string][]ProbeRecord{}
	for _, r := range recs {
		byHash[r.RequestHash] = append(byHash[r.RequestHash], r)
		switch r.Kind {
		case "warm":
			warms++
		case "measured":
			measured++
		case "control":
			controls++
		}
		require.Empty(t, r.Error)
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
	var condHashes = map[string]string{}
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
	srv := fakeCacheEndpoint(t, &hits)
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
	require.NoError(t, err)
	require.Len(t, readProbeRecords(t, out), 7)
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
