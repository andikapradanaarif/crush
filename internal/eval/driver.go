package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// EvalTelemetryEnvVar names the file the agent subprocess writes its
// per-run telemetry to when set. It is the eval extraction path for
// numbers that live in-process: steps, usage, stub stats, recalls.
const EvalTelemetryEnvVar = "CRUSH_EVAL_TELEMETRY"

// RunResult is what one trajectory run (all turns) produced.
type RunResult struct {
	Steps         int
	Tokens        TokenUsage
	StubStats     StubStats
	Recalls       Recalls
	SessionID     string
	ModelResolved string
	// TimedOut is set when the run hit the trajectory's
	// run_timeout_seconds budget.
	TimedOut bool
	// Err is set when the run failed in transport/agent machinery —
	// the model didn't produce the outcome.
	Err error
}

// AgentRunner executes the agent against a prepared workdir. The
// interface exists so tests drive the harness without a provider.
type AgentRunner interface {
	Run(ctx context.Context, workdir string, turns []string, budget Budget) RunResult
}

// CrushRunner drives runs through the crush binary under test —
// os.Executable(), so both arms provably run the same build. Each turn
// is a `crush run` subprocess in the materialized workdir with a pinned
// HOME/XDG: the global config layers merge into every run and would
// otherwise leak a dev laptop's options into results. Credentials come
// from the eval environment — they pass through.
type CrushRunner struct {
	Bin string // os.Executable() when empty is resolved at Run time
	// Home is the pinned HOME for subprocesses; XDG dirs are derived
	// from it.
	Home string
	// ExtraEnv entries override os.Environ for the subprocess.
	ExtraEnv []string
}

// runTelemetry is the JSON the agent subprocess drops at
// CRUSH_EVAL_TELEMETRY.
type runTelemetry struct {
	SessionID string `json:"session_id"`
	Steps     int    `json:"steps"`
	Tokens    struct {
		Input      int64 `json:"input"`
		Output     int64 `json:"output"`
		CacheRead  int64 `json:"cache_read"`
		CacheWrite int64 `json:"cache_write"`
	} `json:"tokens"`
	StubStats struct {
		Invalidations    int   `json:"invalidations"`
		Results          int   `json:"results"`
		SavedBytes       int64 `json:"saved_bytes"`
		BoundaryAdvances int   `json:"boundary_advances"`
	} `json:"stub_stats"`
	Recalls struct {
		Result int `json:"result"`
		Entry  int `json:"entry"`
		Empty  int `json:"empty"`
		Cross  int `json:"cross"`
	} `json:"recalls"`
	Model string `json:"model"`
	Error string `json:"error,omitempty"`
}

// Run executes the trajectory's turns sequentially — each turn a
// `crush run` subprocess continuing the same session. Turn i+1 starts
// only after turn i's process exits, which is after its terminal
// RunComplete; gate retries share the run's correlation and suppress
// the premature completion event, so the subprocess boundary satisfies
// the "follow-ups settle" sequencing rule.
func (c CrushRunner) Run(ctx context.Context, workdir string, turns []string, budget Budget) RunResult {
	var res RunResult
	deadline := time.Now().Add(time.Duration(budget.RunTimeoutSeconds) * time.Second)
	if budget.RunTimeoutSeconds <= 0 {
		deadline = time.Now().Add(15 * time.Minute)
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var sessionID string
	for i, turn := range turns {
		tfile := filepath.Join(workdir, fmt.Sprintf(".eval-telemetry-%d.json", i))
		args := []string{"run", "--quiet"}
		if sessionID != "" {
			args = append(args, "--session", sessionID)
		}
		args = append(args, turn)

		bin := c.Bin
		if bin == "" {
			bin, _ = os.Executable()
		}
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = workdir
		cmd.Env = c.subprocessEnv(tfile)
		out, err := cmd.CombinedOutput()

		tel, _ := readTelemetry(tfile)
		res.Steps += tel.Steps
		res.Tokens.Input += tel.Tokens.Input
		res.Tokens.Output += tel.Tokens.Output
		res.Tokens.CacheRead += tel.Tokens.CacheRead
		res.Tokens.CacheWrite += tel.Tokens.CacheWrite
		// Per-session counters are cumulative; the last turn's values
		// are the trajectory totals.
		res.StubStats = StubStats{
			Invalidations:    tel.StubStats.Invalidations,
			Results:          tel.StubStats.Results,
			SavedBytes:       tel.StubStats.SavedBytes,
			BoundaryAdvances: tel.StubStats.BoundaryAdvances,
		}
		res.Recalls = Recalls{
			Result: tel.Recalls.Result,
			Entry:  tel.Recalls.Entry,
			Empty:  tel.Recalls.Empty,
			Cross:  tel.Recalls.Cross,
		}
		if tel.SessionID != "" {
			sessionID = tel.SessionID
			res.SessionID = sessionID
		}
		if tel.Model != "" {
			res.ModelResolved = tel.Model
		}

		if ctx.Err() == context.DeadlineExceeded {
			res.TimedOut = true
			return res
		}
		if err != nil {
			res.Err = fmt.Errorf("crush run failed: %w: %s", err, out)
			return res
		}
		if tel.Error != "" {
			res.Err = fmt.Errorf("agent run failed: %s", tel.Error)
			return res
		}
	}
	return res
}

// subprocessEnv builds the run's environment: the eval environment's
// credentials pass through; HOME and the XDG dirs are pinned so the
// global config layers merge nothing in.
func (c CrushRunner) subprocessEnv(telemetryFile string) []string {
	pinned := map[string]string{
		"HOME":              c.Home,
		"XDG_CONFIG_HOME":   filepath.Join(c.Home, ".config"),
		"XDG_DATA_HOME":     filepath.Join(c.Home, ".local", "share"),
		"XDG_STATE_HOME":    filepath.Join(c.Home, ".local", "state"),
		EvalTelemetryEnvVar: telemetryFile,
	}
	// Replace rather than append: duplicated keys in environ are
	// resolved first-match by getenv, so a second HOME wouldn't pin.
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, overridden := pinned[k]; !overridden {
			env = append(env, kv)
		}
	}
	for k, v := range pinned {
		env = append(env, k+"="+v)
	}
	return append(env, c.ExtraEnv...)
}

func readTelemetry(path string) (runTelemetry, error) {
	var t runTelemetry
	data, err := os.ReadFile(path)
	if err != nil {
		return t, err
	}
	return t, json.Unmarshal(data, &t)
}
