package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DefaultEvalDir is where the corpus lives relative to the repo root.
const DefaultEvalDir = "eval"

// Validation collects every trajectory spec violation instead of
// failing on the first — corpus review wants the full list.
type Validation struct {
	TrajectoryID string
	Problems     []string
}

func (v Validation) Error() string {
	return fmt.Sprintf("trajectory %s: %s", v.TrajectoryID, strings.Join(v.Problems, "; "))
}

// LoadTrajectory reads and validates corpus/<id>/trajectory.json.
// trajDir is the trajectory's corpus directory.
func LoadTrajectory(trajDir string) (*Trajectory, error) {
	path := filepath.Join(trajDir, "trajectory.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read trajectory spec: %w", err)
	}
	var t Trajectory
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if problems := ValidateTrajectory(&t, trajDir); len(problems) > 0 {
		return nil, Validation{TrajectoryID: t.ID, Problems: problems}
	}
	return &t, nil
}

// ValidateTrajectory checks the spec rules that are decidable at load
// time. trajDir is used to confirm referenced files exist.
func ValidateTrajectory(t *Trajectory, trajDir string) []string {
	var problems []string

	if t.ID == "" {
		problems = append(problems, "id is required")
	}
	if t.SchemaVersion != 1 {
		problems = append(problems, fmt.Sprintf("schema_version must be 1, got %d", t.SchemaVersion))
	}

	switch t.Origin.Kind {
	case "regression", "production", "synthetic":
		if t.Origin.Kind == "production" && (t.Origin.Scrubbed == nil || !*t.Origin.Scrubbed) {
			problems = append(problems, "origin.scrubbed must be true for production trajectories")
		}
		// A regression carrying any source reference is a real-world
		// artifact — same scrubbing requirement as production.
		if t.Origin.Kind == "regression" && t.Origin.Source != "" && (t.Origin.Scrubbed == nil || !*t.Origin.Scrubbed) {
			problems = append(problems, "origin.scrubbed must be true for regression trajectories with a real source")
		}
	case "":
		problems = append(problems, "origin.kind is required")
	default:
		problems = append(problems, fmt.Sprintf("origin.kind %q is not regression|production|synthetic", t.Origin.Kind))
	}

	switch t.StartState.Kind {
	case "fixture":
		if t.StartState.FixtureDir == "" {
			problems = append(problems, "start_state.fixture_dir is required for fixture kind")
		} else if _, err := os.Stat(filepath.Join(trajDir, t.StartState.FixtureDir)); err != nil {
			problems = append(problems, fmt.Sprintf("fixture dir %q: %v", t.StartState.FixtureDir, err))
		}
	case "git":
		if t.StartState.Repo == "" || t.StartState.Ref == "" {
			problems = append(problems, "start_state.repo and .ref are required for git kind")
		}
	case "":
		problems = append(problems, "start_state.kind is required (fixture|git)")
	default:
		problems = append(problems, fmt.Sprintf("start_state.kind %q is not fixture|git", t.StartState.Kind))
	}

	// A git start state under network:false must resolve repo to a
	// local source — the clone itself is egress.
	// Coverage must be achievable within budget — a min_steps
	// predicate above the step cap is permanently inconclusive and
	// burns attempts to the starvation cap forever. min_ over a
	// flag-gated field is worse: the counter is structurally absent
	// on the arm where the flag is off, so the pairing starves by
	// construction — those assertions belong in arm coverage.
	for key, v := range t.Coverage {
		op, field, err := ParseCoverageKey(key)
		if err != nil {
			problems = append(problems, fmt.Sprintf("coverage %q: %v", key, err))
			continue
		}
		if op == "min" && field == "steps" && t.Budget.MaxSteps > 0 && int(v) > t.Budget.MaxSteps {
			problems = append(problems, fmt.Sprintf("coverage min_steps=%v exceeds budget.max_steps=%d — permanently inconclusive", v, t.Budget.MaxSteps))
		}
		if op == "min" {
			for _, p := range flagGatedPrefixes {
				if strings.HasPrefix(field, p) {
					problems = append(problems, fmt.Sprintf("coverage %q: %s is flag-gated — a shared min_ predicate starves the arm where the flag is off; move it to arm coverage in the experiment", key, field))
				}
			}
		}
	}
	if t.Requires.Network != nil && !*t.Requires.Network && t.StartState.Kind == "git" {
		repo := t.StartState.Repo
		if strings.HasPrefix(repo, "http://") || strings.HasPrefix(repo, "https://") || strings.HasPrefix(repo, "git@") {
			problems = append(problems, "requires.network=false conflicts with remote git start_state (use a local path, file:// URL, or pre-seeded mirror)")
		}
	}

	if len(t.Task.Turns) == 0 {
		problems = append(problems, "task.turns must contain at least one prompt")
	} else {
		for i, turn := range t.Task.Turns {
			if strings.TrimSpace(turn) == "" {
				problems = append(problems, fmt.Sprintf("task.turns[%d] is empty", i))
			}
		}
	}

	if t.Check.Script == "" {
		problems = append(problems, "check.script is required")
	} else if _, err := os.Stat(filepath.Join(trajDir, t.Check.Script)); err != nil {
		problems = append(problems, fmt.Sprintf("check script %q: %v", t.Check.Script, err))
	}

	hasRef := fileExists(filepath.Join(trajDir, "reference.patch"))
	hasCounter := fileExists(filepath.Join(trajDir, "counterexample.patch"))

	switch t.Check.ExpectStartState {
	case "fail":
		// The strong form: the check must fail on the start state.
		if hasCounter {
			problems = append(problems, "counterexample.patch on a \"fail\" trajectory is a validation error")
		}
		if t.Origin.Kind == "regression" && !hasRef {
			// The fix diff is known by construction for a regression.
			problems = append(problems, "reference.patch is required for origin.kind=regression")
		}
	case "pass":
		// Required, not optional: without it a pass-guard accrues
		// p̂=1.0, joins stable, and can never fire — false coverage.
		if !hasCounter {
			problems = append(problems, "counterexample.patch is required for expect_start_state=pass")
		}
	case "":
		problems = append(problems, "check.expect_start_state is required (fail|pass)")
	default:
		problems = append(problems, fmt.Sprintf("check.expect_start_state %q is not fail|pass", t.Check.ExpectStartState))
	}

	if t.Budget.MaxSteps < 0 || t.Budget.RunTimeoutSeconds < 0 {
		problems = append(problems, "budget values must be non-negative")
	}

	return problems
}

// LoadCorpus discovers trajectories under evalDir/corpus. Validation
// failures are aggregated: one bad trajectory shouldn't hide the rest.
func LoadCorpus(evalDir string) (map[string]*Trajectory, error) {
	corpusDir := filepath.Join(evalDir, "corpus")
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		return nil, fmt.Errorf("read corpus dir: %w", err)
	}
	out := make(map[string]*Trajectory)
	var problems []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := LoadTrajectory(filepath.Join(corpusDir, e.Name()))
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if t.ID != e.Name() {
			problems = append(problems, fmt.Sprintf("trajectory %s: id %q does not match directory name", e.Name(), t.ID))
			continue
		}
		out[t.ID] = t
	}
	if len(problems) > 0 {
		return out, fmt.Errorf("corpus validation failed:\n  %s", strings.Join(problems, "\n  "))
	}
	return out, nil
}

// LoadExperiment reads an experiment definition.
func LoadExperiment(path string) (*Experiment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read experiment: %w", err)
	}
	var e Experiment
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := ValidateExperiment(&e); err != nil {
		return nil, err
	}
	return &e, nil
}

// ValidateExperiment checks the experiment definition.
func ValidateExperiment(e *Experiment) error {
	if e.Name == "" {
		return fmt.Errorf("experiment name is required")
	}
	if e.Name == CharacterizeExperiment {
		return fmt.Errorf("experiment name %q is reserved for characterization runs", CharacterizeExperiment)
	}
	if e.Model == "" {
		return fmt.Errorf("experiment model pin is required (provider/model)")
	}
	if len(e.Arms) != 2 {
		return fmt.Errorf("analysis is defined for two arms, got %d", len(e.Arms))
	}
	if _, ok := e.Arms[ArmControl]; !ok {
		return fmt.Errorf("arms must include \"control\", got %v", sortedKeys(e.Arms))
	}
	if _, ok := e.Arms[ArmTreatment]; !ok {
		return fmt.Errorf("arms must include \"treatment\", got %v", sortedKeys(e.Arms))
	}
	// Temperature is a run condition hashed into baseline keys —
	// characterize/smoke always pin it, so an unpinned experiment's
	// control records land in a "default"-temp cell no baseline can
	// join, silently darkening the catastrophic tier.
	if e.Temperature == nil {
		return fmt.Errorf("temperature must be pinned — unpinned arms never join characterized baselines")
	}
	if len(e.Corpus) == 0 {
		return fmt.Errorf("corpus selector is required")
	}
	for _, sel := range e.Corpus {
		if sel == "band:"+string(BandQuarantined) {
			return fmt.Errorf("band:quarantined is never selectable — re-validating needs characterize mode, not an experiment arm")
		}
	}
	for band, n := range e.RunsPerTrajectory {
		switch band {
		case BandStable, BandMid, BandUncharacterized:
		case BandQuarantined:
			return fmt.Errorf("runs_per_trajectory cannot target quarantined band")
		default:
			return fmt.Errorf("runs_per_trajectory: unknown band %q", band)
		}
		if n <= 0 {
			return fmt.Errorf("runs_per_trajectory[%s] must be > 0", band)
		}
	}
	for name, arm := range e.Arms {
		for key := range arm.Coverage {
			op, field, err := ParseArmCoverageKey(key)
			if err != nil {
				return fmt.Errorf("arm %q coverage %q: %w", name, key, err)
			}
			// At experiment load only the arm's literal options are
			// visible — an absent key defers to flags.json defaults
			// and can't be judged here. RunExperiment re-checks
			// against resolved options once the manifest is loaded.
			resolve := armOptionResolver(arm)
			if err := checkArmStarvation(name, key, op, field, resolve); err != nil {
				return err
			}
		}
	}
	return nil
}

// starvationRule is one requirement an arm's resolved options must
// satisfy for a coverage field's counter to be reachable. ok receives
// the option resolver; unknown options must pass — the check only
// rejects what it can prove starves.
type starvationRule struct {
	desc string
	ok   func(resolve func(string) (any, bool)) bool
}

// boolOn requires a boolean option to resolve true.
func boolOn(opt string) starvationRule {
	return starvationRule{opt, func(resolve func(string) (any, bool)) bool {
		v, known := resolve(opt)
		b, _ := v.(bool)
		return !known || b
	}}
}

// boolOff requires a boolean option to resolve false — the "would
// have fired" measurement rows only exist in the flag-off arm.
func boolOff(opt string) starvationRule {
	return starvationRule{opt + "=false", func(resolve func(string) (any, bool)) bool {
		v, known := resolve(opt)
		b, _ := v.(bool)
		return !known || !b
	}}
}

// modeIs requires a string option to resolve to one of the accepted
// values.
func modeIs(opt string, modes ...string) starvationRule {
	return starvationRule{opt + "=" + strings.Join(modes, "|"), func(resolve func(string) (any, bool)) bool {
		v, known := resolve(opt)
		if !known {
			return true
		}
		s, _ := v.(string)
		return slices.Contains(modes, s)
	}}
}

// notebookRecallTool is internal/agent/tools/notebook's
// RecallToolName — the package boundary can't import it.
const notebookRecallTool = "recall"

// disablesTool reports whether a resolved disabled_tools value lists
// name. JSON unmarshals the option as []any; test fixtures may build
// it as []string.
func disablesTool(v any, name string) bool {
	switch l := v.(type) {
	case []string:
		return slices.Contains(l, name)
	case []any:
		return slices.Contains(l, any(name))
	}
	return false
}

// recallToolLive requires the recall tool to stay registered —
// disabled_tools:[recall] removes every pointer-resolving counter's
// only writer.
var recallToolLive = starvationRule{
	"recall tool enabled (disabled_tools must not list it)",
	func(resolve func(string) (any, bool)) bool {
		v, known := resolve("disabled_tools")
		return !known || !disablesTool(v, notebookRecallTool)
	},
}

// priorTurnsRecallLive is the conditional form for prior_turns.* and
// digests.*: stub/digest print recall pointers and die without the
// tool, while summarize renders entries inline and doesn't need it.
// The mode constants are internal/agent's priorTurns* strings — kept
// in sync with internal/agent/prior_turns.go; the package boundary
// can't import them.
var priorTurnsRecallLive = starvationRule{
	"notebook_prior_turns=stub|digest need the recall tool enabled",
	func(resolve func(string) (any, bool)) bool {
		v, known := resolve("notebook_prior_turns")
		if !known {
			return true
		}
		s, _ := v.(string)
		if s != "stub" && s != "digest" {
			return true
		}
		dt, known := resolve("disabled_tools")
		return !known || !disablesTool(dt, notebookRecallTool)
	},
}

// armStarvationRules maps a coverage field to the option requirements
// that must hold for its counter to be reachable. map_calls is
// absent deliberately: tool-not-found attempts still count, so a
// flag-off arm can measure unprompted map reach — only the *_ok /
// *_index_unavailable / result_bytes fields are truly unreachable.
func armStarvationRules(field string) []starvationRule {
	switch field {
	case "call_metrics.map_calls_ok",
		"call_metrics.map_calls_index_unavailable",
		"call_metrics.map_result_bytes",
		// The wrong-pointer detector requires a successful map call
		// (analyze.go skips is_error records) — structurally zero
		// wherever map isn't registered.
		"call_metrics.wrong_pointer_events":
		return []starvationRule{boolOn("project_index")}
	}
	if strings.HasPrefix(field, "stub_stats.") {
		if field == "stub_stats.boundary_advances" {
			// Boundary moves count whenever the notebook prefix
			// renders — supersession and collapse are just two of
			// its writers; verbatim arms churn too.
			return []starvationRule{boolOn("notebook_enabled")}
		}
		return []starvationRule{boolOn("notebook_stub_superseded"), boolOn("notebook_enabled"), recallToolLive}
	}
	if edge, ok := strings.CutPrefix(field, "edge_firings."); ok {
		// Stall and burn-watch ride ambiguity_clarification; the
		// outcome suffix picks the direction.
		if strings.HasPrefix(edge, "stall.") || strings.HasPrefix(edge, "burn-watch.") {
			switch edge[strings.LastIndexByte(edge, '.')+1:] {
			case "gated":
				return []starvationRule{boolOff("ambiguity_clarification")}
			case "cancelled":
				// Mid-scan ctx kills are flag-independent.
				return nil
			default:
				// Flag-off triggers take the gated short-circuit
				// before resolve/contention — every other outcome
				// needs the flag on.
				return []starvationRule{boolOn("ambiguity_clarification")}
			}
		}
		return nil
	}
	if strings.HasPrefix(field, "checkpoints.") {
		return []starvationRule{boolOn("notebook_checkpoint"), boolOn("notebook_enabled")}
	}
	if strings.HasPrefix(field, "hydration.") {
		return []starvationRule{boolOn("notebook_hydration"), boolOn("notebook_enabled")}
	}
	if strings.HasPrefix(field, "prior_turns.") {
		return []starvationRule{boolOn("notebook_enabled"), modeIs("notebook_prior_turns", "stub", "digest", "summarize"), priorTurnsRecallLive}
	}
	if strings.HasPrefix(field, "digests.") {
		return []starvationRule{boolOn("notebook_enabled"), modeIs("notebook_prior_turns", "digest"), priorTurnsRecallLive}
	}
	if strings.HasPrefix(field, "recalls.") {
		return []starvationRule{boolOn("notebook_enabled"), recallToolLive}
	}
	return nil
}

// armOptionResolver resolves only the arm's literal options — an
// absent key reports unknown so load-time validation can't flag what
// flags.json might enable.
func armOptionResolver(arm Arm) func(string) (any, bool) {
	return func(opt string) (any, bool) {
		v, ok := arm.Config.Options[opt]
		return v, ok
	}
}

// checkArmStarvation rejects min_ predicates that can't measure what
// they claim — flag-gated fields whose gating option resolves off,
// and question_* counters: the question tool is interactive-only, so
// headless eval calls only ever register as is_error tool-not-found
// attempts — a min_ asserts hallucination, not firing. resolve
// reports (value, known); rules pass unknown options so the check
// only rejects what it can prove starves.
func checkArmStarvation(armName, key, op, field string, resolve func(string) (any, bool)) error {
	if op != "min" {
		return nil
	}
	if strings.HasPrefix(field, "call_metrics.question_") {
		return fmt.Errorf("arm %q coverage %q: the question tool is interactive-only — headless calls only register as is_error, so a min_ asserts a hallucination", armName, key)
	}
	for _, req := range armStarvationRules(field) {
		if !req.ok(resolve) {
			return fmt.Errorf("arm %q coverage %q: %s needs %s — every run starves", armName, key, field, req.desc)
		}
	}
	return nil
}

// flagCodeDefaults mirror the Options helper defaults for the flag-
// gated coverage counters — the last resolution step when neither the
// arm nor the flags manifest names the key.
var flagCodeDefaults = map[string]bool{
	"notebook_enabled":         true,
	"notebook_stub_superseded": false,
	"notebook_checkpoint":      true,
	"notebook_hydration":       true,
	"project_index":            false,
	"ambiguity_clarification":  false,
}

// ValidateArmCoverageResolved re-runs the starvation check against
// fully resolved options — arm option, then flags.json default, then
// the code default. This catches the likelier footgun the load-time
// check can't see: a firing assertion on an arm that omits the flag
// entirely (notebook_stub_superseded defaults false).
func ValidateArmCoverageResolved(e *Experiment, manifest *FlagsManifest) error {
	for name, arm := range e.Arms {
		for key := range arm.Coverage {
			op, field, err := ParseArmCoverageKey(key)
			if err != nil {
				return fmt.Errorf("arm %q coverage %q: %w", name, key, err)
			}
			resolve := func(opt string) (any, bool) {
				if v, ok := arm.Config.Options[opt]; ok {
					return v, true
				}
				if manifest != nil {
					if v, ok := manifest.Defaults[opt]; ok {
						return v, true
					}
				}
				if d, ok := flagCodeDefaults[opt]; ok {
					return d, true
				}
				// An unnamed flag resolves to its code default — off
				// for bools, verbatim for notebook_prior_turns — and
				// either way the gated counter is unreachable.
				return nil, true
			}
			if err := checkArmStarvation(name, key, op, field, resolve); err != nil {
				return err
			}
		}
	}
	return nil
}

// coverageTurnCeiling gives the structural maximum a coverage field
// can reach on a trajectory of n turns — (ceil, true, why) when a
// bound is expressible in turn count, false when it isn't.
func coverageTurnCeiling(field string, turns int) (ceil int, bounded bool, why string) {
	switch field {
	case "prior_turns.turns_collapsed":
		// A turn collapses only at a LATER run's frozen render — the
		// last turn has no later run. Summarize mode lowers the
		// practical ceiling further (a covered turn with no entries
		// renders verbatim), but the count can't see entry-emptiness.
		return max(turns-1, 0), true, "the last turn can never collapse — no later run renders it"
	case "digests.written":
		// The run-end pass digests the just-finished turn too, so
		// every turn is digestible. The HasFinishedToolCall floor —
		// a tool-free turn never digests — is invisible to the count.
		return turns, true, "one digest per turn"
	case "prior_turns.events_collapsed", "digests.rendered", "recalls.prior_turn_result":
		// Not turn-bounded above zero, but with fewer than two turns
		// no prior turn exists — any positive min_ starves.
		if turns < 2 {
			return 0, true, "no prior turn exists below 2 turns"
		}
	}
	return 0, false, ""
}

// ValidateArmCoverageVsCorpus rejects min_ predicates that exceed what
// a selected trajectory can ever produce — the starvation class
// checkArmStarvation can't see because trajectory turn counts only
// exist after corpus selection. A ceiling violation starves every run
// of that trajectory into inconclusive, forever.
func ValidateArmCoverageVsCorpus(e *Experiment, trajs []*Trajectory) error {
	var problems []string
	for _, t := range trajs {
		n := len(t.Task.Turns)
		for name, arm := range e.Arms {
			for key, v := range arm.Coverage {
				op, field, err := ParseArmCoverageKey(key)
				if err != nil {
					continue // Load-time validation reports the bad key.
				}
				if op != "min" {
					continue
				}
				ceil, bounded, why := coverageTurnCeiling(field, n)
				if !bounded || v <= float64(ceil) {
					continue
				}
				problems = append(problems, fmt.Sprintf("arm %q coverage %s=%v exceeds trajectory %q ceiling %d (%d turns; %s)", name, key, v, t.ID, ceil, n, why))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("coverage unachievable: %s", strings.Join(problems, "; "))
	}
	return nil
}

// SelectCorpus resolves the corpus selector against loaded trajectories
// and band state. Quarantined trajectories are excluded even under "*".
func SelectCorpus(corpus map[string]*Trajectory, bands *Bands, selectors []string) ([]*Trajectory, error) {
	return selectCorpus(corpus, bands, selectors, false)
}

// selectCorpus with includeQuarantined=true is used by the quarantine
// pass itself — a quarantined trajectory must be selectable for
// re-validation; it just can't be gated on.
func selectCorpus(corpus map[string]*Trajectory, bands *Bands, selectors []string, includeQuarantined bool) ([]*Trajectory, error) {
	want := make(map[string]bool)
	for _, sel := range selectors {
		if rest, ok := strings.CutPrefix(sel, "band:"); ok {
			band := Band(rest)
			for id := range corpus {
				if bands.Band(id) == band && (band != BandQuarantined || includeQuarantined) {
					want[id] = true
				}
			}
			continue
		}
		// Glob over trajectory ids.
		matched := false
		for id := range corpus {
			ok, err := filepath.Match(sel, id)
			if err != nil {
				return nil, fmt.Errorf("corpus selector %q: %w", sel, err)
			}
			if ok {
				matched = true
				want[id] = true
			}
		}
		if !matched && sel != "*" {
			return nil, fmt.Errorf("corpus selector %q matched no trajectories", sel)
		}
	}
	var out []*Trajectory
	for id, t := range corpus {
		if bands.Band(id) == BandQuarantined && !includeQuarantined {
			continue
		}
		if want[id] {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b *Trajectory) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// ContentHash scopes samples to the corpus revision: p̂ is conditional
// on check.sh, trajectory.json, the fixture, and the patches as much as
// on the model. Any change re-keys the baseline like a re-pin.
func ContentHash(trajDir string) (string, error) {
	h := sha256.New()
	var files []string
	err := filepath.WalkDir(trajDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk trajectory dir: %w", err)
	}
	slices.Sort(files)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("hash %s: %w", f, err)
		}
		rel, _ := filepath.Rel(trajDir, f)
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
