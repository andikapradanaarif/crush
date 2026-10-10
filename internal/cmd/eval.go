package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/eval"
	"github.com/spf13/cobra"
)

var evalCmd = &cobra.Command{
	Use:   "eval",
	Short: "Run the golden-trajectory eval harness",
	Long: `Golden-trajectory behavioral regression harness (docs/design/EVAL_HARNESS.md).

The corpus lives under --eval-dir (default ./eval):

  eval/corpus/<id>/trajectory.json   task spec
  eval/corpus/<id>/check.sh          scoring function
  eval/flags.json                    flag projection + defaults
  eval/experiments/<name>.json       paired comparison
  eval/results/                      append-only run records
  eval/bands.json                    characterization state

Each agent run is a 'crush run' subprocess in a fresh materialized
workdir with a pinned HOME/XDG — both arms provably run this build.
Provider credentials come from the eval environment: manifests
reference $LLM_API_KEY/$LLM_BASE_URL, so the serving endpoint is
operator env, not checked-in config.`,
}

var evalQuarantineCmd = &cobra.Command{
	Use:   "quarantine [trajectory-glob...]",
	Short: "Agent-free check validation pass over the corpus",
	Long: `Validate checks without agent runs: the check must agree with
expect_start_state on the start state, pass on start+reference.patch,
and fail on start+counterexample.patch. Failures quarantine the
trajectory until the check is fixed and re-validated.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		r, err := evalRunner(cmd)
		if err != nil {
			return err
		}
		defer r.Close()
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		sel := args
		if len(sel) == 0 {
			sel = []string{"*"}
		}
		verdicts, err := r.QuarantineCorpus(ctx, sel)
		if err != nil {
			return err
		}
		for _, id := range slices.Sorted(maps.Keys(verdicts)) {
			reason := verdicts[id]
			switch {
			case reason == "":
				fmt.Printf("%s: clean\n", id)
			case strings.HasPrefix(string(reason), "skipped:"):
				fmt.Printf("%s: SKIPPED (%s)\n", id, strings.TrimPrefix(string(reason), "skipped:"))
			default:
				fmt.Printf("%s: QUARANTINED (%s)\n", id, reason)
			}
		}
		return nil
	},
}

var evalCharacterizeCmd = &cobra.Command{
	Use:   "characterize [trajectory-glob...]",
	Short: "Genesis / re-characterization runs under the current default condition",
	RunE: func(cmd *cobra.Command, args []string) error {
		n, _ := cmd.Flags().GetInt("runs")
		model, _ := cmd.Flags().GetString("model")
		temp, _ := cmd.Flags().GetFloat64("temperature")
		r, err := evalRunner(cmd)
		if err != nil {
			return err
		}
		defer r.Close()
		providers, err := evalProvidersFlag(cmd)
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		sel := args
		if len(sel) == 0 {
			sel = []string{"*"}
		}
		return r.Characterize(ctx, model, &temp, n, sel, providers)
	},
}

// evalGenCmd emits parametric trajectory instances under
// <eval-dir>/corpus/ — the ladder cells' fixture+seed factory
// (#223). Each replicate is a distinct quirk draw; the emitted
// spec is a plain trajectory, so validation, hashing, and snapshot
// replay treat generated cells exactly like hand-written ones.
var evalGenCmd = &cobra.Command{
	Use:   "gen",
	Short: "Generate parametric corpus instances for the memory ladders",
	RunE: func(cmd *cobra.Command, _ []string) error {
		evalDir, _ := cmd.Flags().GetString("eval-dir")
		spec := eval.GenSpec{
			Plausibility: flagString(cmd, "plausibility"),
			Prompt:       flagString(cmd, "prompt"),
		}
		spec.Quirks, _ = cmd.Flags().GetInt("quirks")
		spec.Distractors, _ = cmd.Flags().GetInt("distractors")
		spec.Depth, _ = cmd.Flags().GetInt("depth")
		spec.Seed, _ = cmd.Flags().GetInt64("seed")
		count, _ := cmd.Flags().GetInt("count")
		corpus := filepath.Join(evalDir, "corpus")
		for i := range count {
			spec.Replicate = i
			dir, err := eval.Generate(corpus, spec)
			if err != nil {
				return err
			}
			fmt.Println(filepath.Base(dir))
		}
		return nil
	},
}

func flagString(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}

// evalSelectCmd runs the offline selector measurement (#223): the
// trajectory's scripted seeds materialize once under
// <eval-dir>/genwork/, then the production selector evaluates the
// seeded pools under --memory-params. Every stored-dose →
// rendered-dose question on the ladders answers here for free —
// before any paid agent run is scheduled.
var evalSelectCmd = &cobra.Command{
	Use:   "select <trajectory-id>",
	Short: "Offline selector simulation over a trajectory's seeded memory pools",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		evalDir, _ := cmd.Flags().GetString("eval-dir")
		prompt, _ := cmd.Flags().GetString("prompt")
		var overlay map[string]any
		if raw, _ := cmd.Flags().GetString("memory-params"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &overlay); err != nil {
				return fmt.Errorf("memory-params: %w", err)
			}
		}
		trajDir := filepath.Join(evalDir, "corpus", args[0])
		traj, err := eval.LoadTrajectory(trajDir)
		if err != nil {
			return err
		}
		r, err := evalRunner(cmd)
		if err != nil {
			return err
		}
		defer r.Close()
		rep, err := r.RunSelectProbe(cmd.Context(), traj, trajDir, prompt, overlay)
		if err != nil {
			return err
		}
		if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
			out, err := json.MarshalIndent(rep, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(out))
			return nil
		}
		fmt.Print(rep.String())
		return nil
	},
}

// evalCurveCmd is the offline ladder sweep: it enumerates a dose
// grid, generates each cell's instances, and probes every one —
// the stored-dose → rendered-dose curves for both ladders in one
// pass, JSONL-streamed so a long sweep's partial results survive.
var evalCurveCmd = &cobra.Command{
	Use:   "curve",
	Short: "Offline selector curves over the generated dose grid",
	RunE: func(cmd *cobra.Command, _ []string) error {
		evalDir, _ := cmd.Flags().GetString("eval-dir")
		var spec eval.SelectSweepSpec
		var err error
		if spec.Quirks, err = flagIntList(cmd, "quirks"); err != nil {
			return err
		}
		if spec.Distractors, err = flagIntList(cmd, "distractors"); err != nil {
			return err
		}
		if spec.Depth, err = flagIntList(cmd, "depth"); err != nil {
			return err
		}
		spec.Plausibility = flagStrList(cmd, "plausibility")
		spec.Prompt = flagStrList(cmd, "prompt")
		spec.Replicates, _ = cmd.Flags().GetInt("replicates")
		spec.Seed, _ = cmd.Flags().GetInt64("seed")
		var overlay map[string]any
		if raw, _ := cmd.Flags().GetString("memory-params"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &overlay); err != nil {
				return fmt.Errorf("memory-params: %w", err)
			}
		}
		outPath := flagString(cmd, "out")
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}
		out, err := os.Create(outPath)
		if err != nil {
			return err
		}
		defer out.Close()
		r, err := evalRunner(cmd)
		if err != nil {
			return err
		}
		defer r.Close()
		enc := json.NewEncoder(out)
		return r.RunSelectSweep(cmd.Context(), filepath.Join(evalDir, "corpus"), spec, overlay,
			func(row eval.SelectSweepRow) {
				if row.Error != "" {
					slog.Warn("Curve cell failed", "cell", row.Cell, "error", row.Error)
				}
				if err := enc.Encode(row); err != nil {
					slog.Warn("Curve row encode failed", "cell", row.Cell, "error", err)
				}
			})
	},
}

// flagIntList parses a comma-separated integer flag.
func flagIntList(cmd *cobra.Command, name string) ([]int, error) {
	raw := flagString(cmd, name)
	if raw == "" {
		return nil, nil
	}
	var out []int
	for _, part := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// flagStrList parses a comma-separated string flag.
func flagStrList(cmd *cobra.Command, name string) []string {
	raw := flagString(cmd, name)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

var evalRunCmd = &cobra.Command{
	Use:   "run <experiment.json>",
	Short: "Run a paired experiment and print the gate report",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("usage: crush eval run <experiment.json>")
		}
		exp, err := eval.LoadExperiment(args[0])
		if err != nil {
			return err
		}
		r, err := evalRunner(cmd)
		if err != nil {
			return err
		}
		defer r.Close()
		r.AA, _ = cmd.Flags().GetBool("aa")
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		rep, err := r.RunExperiment(ctx, exp)
		if err != nil {
			return err
		}
		alpha := r.Alpha
		if alpha <= 0 {
			alpha = 0.05
		}
		fmt.Print(rep.Summary(alpha))
		if rep.Fired(alpha) {
			return fmt.Errorf("gate fired")
		}
		if !rep.Powered() {
			return fmt.Errorf("gate inconclusive: no powered tier")
		}
		return nil
	},
}

var evalCompareCmd = &cobra.Command{
	Use:   "compare <experiment.json>",
	Short: "Paired continuous-metric estimator over one invocation's records",
	Long: `Pairs control/treatment attempts by run_index within each
trajectory (drift-matched — the scheduler's lead-arm alternation makes
same-index attempts the temporally closest samples), then reports each
pair's difference normalized by its trajectory's mean control baseline,
aggregated across trajectories with a BCa bootstrap 95% CI (raw pairs
resampled, each replicate re-normalized by its own baseline) and a
sign-flip permutation p. Zero-valued pairs are kept — a treatment zero
is often the effect itself.

Refuses on cross-invocation record sets (pass --invocation to pick
one), on invocations a structural alarm voided or that aborted
mid-run, and on too few pairs. The declared primary's CI is tested
against its MDE boundary; a CI that spans it reports inconclusive
with the required pair count when recorded noise allows.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		exp, err := eval.LoadExperiment(args[0])
		if err != nil {
			return err
		}
		r, err := evalRunner(cmd)
		if err != nil {
			return err
		}
		defer r.Close()
		inv, _ := cmd.Flags().GetString("invocation")
		rep, err := r.Compare(exp, inv)
		if err != nil {
			return err
		}
		fmt.Print(rep.Summary())
		return nil
	},
}

var evalInteractionCmd = &cobra.Command{
	Use:   "interaction <restart-exp.json> <persistent-exp.json>",
	Short: "Process-model 2×2 — does restart-per-turn change the measured notebook effect (#117)",
	Long: `Computes the regime interaction I = Δ_restart − Δ_persist where each Δ is
the treatment-minus-control metric mean per trajectory. The two
manifests must declare different process_model regimes over the same
corpus and arms — records self-identify via their process_model field,
so the function reads a mixed pool.

When |I| clears 10% of the restart-regime control mean the report is
"material": publish the per-regime cell estimates and never fold the
regimes into one effect estimate. Below the bound the restart regime's
estimates read as persistent-equivalent for this metric. The metric
defaults to the restart experiment's declared primary.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		restartExp, err := eval.LoadExperiment(args[0])
		if err != nil {
			return err
		}
		persistExp, err := eval.LoadExperiment(args[1])
		if err != nil {
			return err
		}
		regime := func(e *eval.Experiment) string {
			if e.ProcessModel == "" {
				return eval.ProcessModelRestart
			}
			return e.ProcessModel
		}
		if regime(restartExp) == regime(persistExp) {
			return fmt.Errorf("both experiments declare process_model %q — the 2×2 needs one restart and one persistent manifest", regime(restartExp))
		}
		metric, _ := cmd.Flags().GetString("metric")
		if metric == "" {
			if restartExp.Primary == nil {
				return fmt.Errorf("--metric required — %s declares no primary", restartExp.Name)
			}
			metric = restartExp.Primary.Metric
		}
		r, err := evalRunner(cmd)
		if err != nil {
			return err
		}
		defer r.Close()
		recsA, err := r.LoadExperimentRecords(restartExp.Name)
		if err != nil {
			return err
		}
		recsB, err := r.LoadExperimentRecords(persistExp.Name)
		if err != nil {
			return err
		}
		rep, err := eval.ProcessModelInteraction(append(recsA, recsB...), restartExp, metric)
		if err != nil {
			return err
		}
		out, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	},
}

var evalAnalyzeCmd = &cobra.Command{
	Use:   "analyze <session.db>",
	Short: "Reconstruct per-call gate metrics from a session DB",
	Long: `Run the session-DB sequence analysis standalone — the same pass
ExecuteRun runs before appending a run record. The path may be absolute,
CWD-relative, or eval-dir-relative (a RunRecord session_db value).

Useful beyond eval: it doubles as a debugging tool for real sessions,
which is why --workdir and --session exist. --trajectory supplies the
corpus turns so process-turn boundaries are authoritative instead of
repair-prompt fingerprinted.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		evalDir, _ := cmd.Flags().GetString("eval-dir")
		workdir, _ := cmd.Flags().GetString("workdir")
		sessionID, _ := cmd.Flags().GetString("session")
		trajID, _ := cmd.Flags().GetString("trajectory")

		dbPath := args[0]
		if !filepath.IsAbs(dbPath) {
			if _, err := os.Stat(dbPath); err != nil {
				// session_db paths in run records are eval-dir-relative.
				dbPath = filepath.Join(evalDir, dbPath)
			}
		}

		var turns []string
		if trajID != "" {
			traj, err := eval.LoadTrajectory(filepath.Join(evalDir, "corpus", trajID))
			if err != nil {
				return err
			}
			turns = traj.Task.Turns
		}
		if workdir == "" {
			workdir, _ = os.Getwd()
		}

		goos, _ := cmd.Flags().GetString("goos")
		metrics, err := eval.AnalyzeSessionDB(cmd.Context(), dbPath, eval.AnalyzeOptions{
			SessionID: sessionID,
			Workdir:   workdir,
			Turns:     turns,
			GOOS:      goos,
		})
		if err != nil {
			return err
		}
		out, err := json.MarshalIndent(metrics, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	},
}

var evalProbeCmd = &cobra.Command{
	Use:   "probe <session.db> <probe-name|all>",
	Short: "Offline mechanism probes over a preserved session DB (issue #109)",
	Long: `Runs a named probe — a zero-API-cost mechanism check — over a
preserved or live session DB. Probes consume the analyzer's labeled
call sequence plus per-call inputs and result metadata, so ordering,
turn segmentation, and normalization stay single-sourced.

Known probes:

  post-edit-window    for every view call, was its range inside the
                      ±10-line window of the prior mutation on the
                      same path? (the #98 served-class floor)
  view-edit-same-file per-path mutations and post-edit view counts —
                      the window-free servable superset
  turn-start-reread   first view per (path,turn) of a file mutated in
                      an earlier turn — the collapse re-open signature

'all' runs every probe. Flags match 'eval analyze': --workdir,
--session, --trajectory, --goos.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		evalDir, _ := cmd.Flags().GetString("eval-dir")
		workdir, _ := cmd.Flags().GetString("workdir")
		sessionID, _ := cmd.Flags().GetString("session")
		trajID, _ := cmd.Flags().GetString("trajectory")

		dbPath := args[0]
		if !filepath.IsAbs(dbPath) {
			if _, err := os.Stat(dbPath); err != nil {
				// session_db paths in run records are eval-dir-relative.
				dbPath = filepath.Join(evalDir, dbPath)
			}
		}

		var turns []string
		if trajID != "" {
			traj, err := eval.LoadTrajectory(filepath.Join(evalDir, "corpus", trajID))
			if err != nil {
				return err
			}
			turns = traj.Task.Turns
		}
		if workdir == "" {
			workdir, _ = os.Getwd()
		}

		goos, _ := cmd.Flags().GetString("goos")
		opts := eval.AnalyzeOptions{
			SessionID: sessionID,
			Workdir:   workdir,
			Turns:     turns,
			GOOS:      goos,
		}
		names := []string{args[1]}
		if args[1] == "all" {
			names = eval.ProbeNames()
		}
		reps, err := eval.RunProbes(cmd.Context(), dbPath, names, opts)
		if err != nil {
			return err
		}
		for i, rep := range reps {
			if i > 0 {
				fmt.Println()
			}
			fmt.Print(rep.String())
		}
		return nil
	},
}

var evalProbeCacheCmd = &cobra.Command{
	Use:   "probe-cache",
	Short: "Measure the serving endpoint's prompt-cache behavior (issue #116)",
	Long: `Sends synthetic ~50K-token requests through the same openai-compat
provider construction and session-affinity headers Crush uses, under six
conditions (identical / append / early-system / notebook-position /
late-history / fresh-process), 5 repeats each, independently warmed and
order-randomized. Writes one JSONL record per request plus raw response
bodies; prints the per-condition cache-hit table at the end.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		keyEnv, _ := cmd.Flags().GetString("api-key-env")
		cfg := eval.ProbeCacheConfig{
			BaseURL:      cmp.Or(flagStr(cmd, "base-url"), os.Getenv("LLM_BASE_URL")),
			APIKey:       os.Getenv(keyEnv),
			Model:        cmp.Or(flagStr(cmd, "model"), os.Getenv("LLM_MODEL")),
			TargetTokens: flagInt(cmd, "target-tokens"),
			Repeats:      flagInt(cmd, "repeats"),
			MaxRequests:  flagInt(cmd, "max-requests"),
			OutPath:      flagStr(cmd, "out"),
			RawDir:       flagStr(cmd, "raw-dir"),
			Seed:         flagInt64(cmd, "seed"),
			DelayScale:   flagFloat(cmd, "delay-scale"),
			DryRun:       flagBool(cmd, "dry-run"),
		}
		if cfg.BaseURL == "" {
			return errors.New("--base-url or LLM_BASE_URL is required")
		}
		if cfg.APIKey == "" {
			return fmt.Errorf("env var %q is empty or unset (--api-key-env)", keyEnv)
		}
		if cfg.Model == "" {
			return errors.New("--model or LLM_MODEL is required")
		}
		if cfg.OutPath == "" {
			cfg.OutPath = fmt.Sprintf("probe-cache-%d.jsonl", time.Now().Unix())
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return eval.RunProbeCache(ctx, cfg)
	},
}

func flagStr(cmd *cobra.Command, n string) string { v, _ := cmd.Flags().GetString(n); return v }
func flagInt(cmd *cobra.Command, n string) int    { v, _ := cmd.Flags().GetInt(n); return v }
func flagInt64(cmd *cobra.Command, n string) int64 {
	v, _ := cmd.Flags().GetInt64(n)
	return v
}

func flagFloat(cmd *cobra.Command, n string) float64 {
	v, _ := cmd.Flags().GetFloat64(n)
	return v
}
func flagBool(cmd *cobra.Command, n string) bool { v, _ := cmd.Flags().GetBool(n); return v }

var evalSmokeCmd = &cobra.Command{
	Use:   "smoke",
	Short: "Smoke tier: strict 0/N collapse check over the stable band",
	RunE: func(cmd *cobra.Command, args []string) error {
		n, _ := cmd.Flags().GetInt("runs")
		model, _ := cmd.Flags().GetString("model")
		temp, _ := cmd.Flags().GetFloat64("temperature")
		r, err := evalRunner(cmd)
		if err != nil {
			return err
		}
		defer r.Close()
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		providers, err := evalProvidersFlag(cmd)
		if err != nil {
			return err
		}
		alarms, err := r.Smoke(ctx, model, &temp, n, providers)
		if err != nil {
			return err
		}
		if len(alarms) > 0 {
			return fmt.Errorf("smoke collapse: %v", alarms)
		}
		fmt.Println("smoke: clean")
		return nil
	},
}

var evalCalibrateCmd = &cobra.Command{
	Use:   "calibrate",
	Short: "Learn-then-Test offline certification of memory params",
	Long: `Offline certification for learned memory parameters (#295).

Reads the settled referent episodes in a project crush.db, estimates
the harm rate of each candidate parameter value, and tests the fixed
sequence most-conservative → least with Hoeffding–Bentkus p-values.
The emitted certificate is reproducible from the episode set plus
the stated bound; a run that certifies nothing keeps the default.

The guarantee holds on the calibration distribution only — episodes
are stratified to one per session because within-session draws are
correlated. ~60+ clean labeled episodes are needed before α=5% can
certify at all.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		param, _ := cmd.Flags().GetString("param")
		dbPath, _ := cmd.Flags().GetString("db")
		workdir, _ := cmd.Flags().GetString("workdir")
		alpha, _ := cmd.Flags().GetFloat64("alpha")
		delta, _ := cmd.Flags().GetFloat64("delta")
		gridStr, _ := cmd.Flags().GetString("grid")
		out, _ := cmd.Flags().GetString("out")

		if workdir == "" {
			var err error
			workdir, err = os.Getwd()
			if err != nil {
				return err
			}
		}
		var grid []int
		for _, s := range strings.Split(gridStr, ",") {
			v, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil || v <= 0 {
				return fmt.Errorf("bad --grid value %q", s)
			}
			grid = append(grid, v)
		}

		conn, err := db.ConnectReadOnly(ctx, dbPath)
		if err != nil {
			return fmt.Errorf("open %s: %w", dbPath, err)
		}
		defer conn.Close()
		rows, err := db.New(conn).ListReferentCalibrationEpisodes(ctx, cmdlog.ProjectKeyForDir(workdir))
		if err != nil {
			// A DB written before the referent schema landed has no
			// episodes — that is an empty calibration set ("no
			// certification"), not an error worth dying on.
			if !strings.Contains(err.Error(), "no such table: referent_episodes") {
				return fmt.Errorf("list calibration episodes: %w", err)
			}
			rows = nil
		}
		eps := make([]eval.CalibrationEpisode, 0, len(rows))
		for _, r := range rows {
			eps = append(eps, eval.CalibrationEpisode{
				ID:          r.ID,
				Phrase:      r.Phrase,
				Target:      r.Target,
				SessionID:   r.SessionID,
				Verdict:     r.Verdict,
				Committed:   r.LabelCommitted,
				HashChanged: r.LabelHashChanged,
				Suggested:   r.MemorySuggested != 0,
			})
		}

		var cert eval.Certificate
		switch param {
		case eval.CalibrateParamPromoteHits:
			cert, err = eval.CalibratePromoteHits(eps, grid, alpha, delta)
		default:
			err = fmt.Errorf("unknown --param %q (have %q)", param, eval.CalibrateParamPromoteHits)
		}
		if err != nil {
			return err
		}

		data, err := json.MarshalIndent(cert, "", "  ")
		if err != nil {
			return err
		}
		if out != "" {
			if err := os.WriteFile(out, append(data, '\n'), 0o644); err != nil {
				return err
			}
		}
		fmt.Println(string(data))
		if cert.CertifiedValue == nil {
			fmt.Println("calibrate: no certification — keeping the default")
		}
		return nil
	},
}

func evalRunner(cmd *cobra.Command) (*eval.Runner, error) {
	dir, _ := cmd.Flags().GetString("eval-dir")
	return &eval.Runner{EvalDir: dir}, nil
}

// evalProvidersFlag resolves --providers: an experiment file whose
// providers block is borrowed for the child's pinned-HOME config —
// providers are infrastructure, not part of the measured condition.
func evalProvidersFlag(cmd *cobra.Command) (map[string]any, error) {
	path, _ := cmd.Flags().GetString("providers")
	if path == "" {
		return nil, nil
	}
	exp, err := eval.LoadExperiment(path)
	if err != nil {
		return nil, fmt.Errorf("providers %s: %w", path, err)
	}
	return exp.Providers, nil
}

func init() {
	evalCmd.PersistentFlags().String("eval-dir", eval.DefaultEvalDir, "eval root directory")
	evalRunCmd.Flags().Bool("aa", false, "add a calibration arm (control clone) measuring the harness's false-effect magnitude; refreshes noise.json")
	evalCharacterizeCmd.Flags().IntP("runs", "n", eval.GenesisRuns, "runs per trajectory")
	evalCharacterizeCmd.Flags().StringP("model", "m", "", "model pin (provider/model)")
	evalCharacterizeCmd.Flags().Float64("temperature", 0, "sampling temperature")
	evalCharacterizeCmd.Flags().String("providers", "", "experiment file whose providers block supplies the child's provider config")
	evalSmokeCmd.Flags().IntP("runs", "n", 5, "runs per stable trajectory")
	evalSmokeCmd.Flags().StringP("model", "m", "", "model pin (provider/model)")
	evalSmokeCmd.Flags().Float64("temperature", 0, "sampling temperature")
	evalSmokeCmd.Flags().String("providers", "", "experiment file whose providers block supplies the child's provider config")
	_ = evalCharacterizeCmd.MarkFlagRequired("model")
	_ = evalSmokeCmd.MarkFlagRequired("model")
	evalProbeCacheCmd.Flags().String("base-url", "", "serving endpoint (default $LLM_BASE_URL)")
	evalProbeCacheCmd.Flags().String("api-key-env", "LLM_API_KEY", "env var holding the API key")
	evalProbeCacheCmd.Flags().String("model", "", "model ID (default $LLM_MODEL)")
	evalProbeCacheCmd.Flags().Int("target-tokens", 50000, "approximate prompt size in tokens")
	evalProbeCacheCmd.Flags().Int("repeats", 5, "measured repeats per condition")
	evalProbeCacheCmd.Flags().Int("max-requests", 120, "hard spend cap on sends (one HTTP request each)")
	evalProbeCacheCmd.Flags().String("out", "", "JSONL output path (default probe-cache-<ts>.jsonl)")
	evalProbeCacheCmd.Flags().String("raw-dir", "", "raw response bodies dir (default <out>-raw)")
	evalProbeCacheCmd.Flags().Int64("seed", time.Now().UnixNano(), "filler/schedule RNG seed")
	evalProbeCacheCmd.Flags().Float64("delay-scale", 1.0, "multiplier on inter-request sleeps (spec timing = 1.0)")
	evalProbeCacheCmd.Flags().Bool("dry-run", false, "print schedule and send estimate without sending")
	for _, c := range []*cobra.Command{evalAnalyzeCmd, evalProbeCmd} {
		c.Flags().String("workdir", "", "run working dir for normalizing relative call paths (default: CWD)")
		c.Flags().String("session", "", "session ID to analyze (default: latest parent session)")
		c.Flags().String("trajectory", "", "corpus trajectory ID — supplies turns for process-turn segmentation")
		c.Flags().String("goos", "", "OS whose path conventions produced the artifact (default: this machine)")
	}
	evalCompareCmd.Flags().String("invocation", "", "invocation ID to compare (required when records span several)")
	evalInteractionCmd.Flags().String("metric", "", "record metric for the interaction (default: the restart experiment's primary.metric)")
	evalGenCmd.Flags().Int("quirks", 4, "relevant memory rows about the target defect (depth dose M, 0-4)")
	evalGenCmd.Flags().Int("distractors", 0, "wrong-referent memory rows (dose K)")
	evalGenCmd.Flags().String("plausibility", "mid", "distractor nearness: low|mid|high")
	evalGenCmd.Flags().String("prompt", "vague", "task prompt style: vague|explicit")
	evalGenCmd.Flags().Int("depth", 0, "package nesting depth above the target (discovery cost)")
	evalGenCmd.Flags().Int64("seed", 1, "RNG seed — quirk identity sampling")
	evalGenCmd.Flags().Int("count", 1, "replicate instances to emit (each a distinct draw)")
	evalSelectCmd.Flags().String("prompt", "", "prompt to bind against (default: the trajectory's first task turn)")
	evalSelectCmd.Flags().String("memory-params", "", "JSON overlay for params.Memory (e.g. '{\"open_render_limit\":0}')")
	evalSelectCmd.Flags().Bool("json", false, "print the full report as JSON")
	evalCurveCmd.Flags().String("quirks", "4", "comma-separated depth doses M (relevant rows)")
	evalCurveCmd.Flags().String("distractors", "0", "comma-separated distractor doses K")
	evalCurveCmd.Flags().String("plausibility", "mid", "comma-separated plausibility tiers")
	evalCurveCmd.Flags().String("prompt", "vague", "comma-separated prompt styles")
	evalCurveCmd.Flags().String("depth", "0", "comma-separated discovery-cost depths")
	evalCurveCmd.Flags().Int("replicates", 1, "instances per cell")
	evalCurveCmd.Flags().Int64("seed", 1, "base RNG seed for the grid")
	evalCurveCmd.Flags().String("memory-params", "", "JSON overlay for params.Memory")
	evalCurveCmd.Flags().String("out", "curve.jsonl", "JSONL output path")
	evalCalibrateCmd.Flags().String("param", eval.CalibrateParamPromoteHits, "parameter to certify")
	evalCalibrateCmd.Flags().String("db", ".crush/crush.db", "project crush.db to read episodes from")
	evalCalibrateCmd.Flags().String("workdir", "", "project dir for project_key derivation (default: CWD)")
	evalCalibrateCmd.Flags().Float64("alpha", 0.05, "certified harm-rate bound")
	evalCalibrateCmd.Flags().Float64("delta", 0.05, "family-wise error level")
	evalCalibrateCmd.Flags().String("grid", "5,4,3,2,1", "candidate values, any order — tested most-conservative first")
	evalCalibrateCmd.Flags().String("out", "", "write the certificate JSON here (also printed)")
	evalCmd.AddCommand(evalQuarantineCmd, evalCharacterizeCmd, evalRunCmd, evalSmokeCmd, evalAnalyzeCmd, evalProbeCmd, evalProbeCacheCmd, evalCompareCmd, evalGenCmd, evalSelectCmd, evalCurveCmd, evalInteractionCmd, evalCalibrateCmd)
}
