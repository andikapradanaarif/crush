package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/params"
)

// Characterization constants. The boundary positions belong to the
// characterization doc, not the schema — these are the initial values.
const (
	// GenesisRuns is the genesis characterization sample size. N=5 has
	// a uselessly wide CI — bands are provisional and revised from
	// accumulated run records.
	GenesisRuns = 5

	// BaselineWindowRuns and BaselineWindowDays bound the rolling
	// baseline window — min(K runs, D days): a count bound alone
	// retains arbitrarily old runs in quiet periods.
	BaselineWindowRuns = 30
	BaselineWindowDays = 30

	// StablePHatFloor and StableMinN bound the stable band. Stable is
	// deliberately conservative: the catastrophic tier protects only
	// near-pristine baselines at small arm N anyway.
	StablePHatFloor = 0.9
	StableMinN      = GenesisRuns

	// PromotionStreakRequired implements the asymmetric hysteresis:
	// demotion on one bad characterization, promotion on two
	// consecutive good ones — so a stable trajectory producing a
	// suspect characterization doesn't keep catastrophic eligibility
	// through the confirmation window.
	PromotionStreakRequired = 2

	// NeverPassedFails routes a trajectory to quarantined with
	// never_passed when it has zero passes in this many trailing
	// conclusive runs. Genesis 0/5 doesn't qualify — a real p=0.3
	// task would be ejected ~17% of the time at 0/5, ~3% at 0/10.
	NeverPassedFails = 10
)

// FlagsManifest is eval/flags.json — the declared projection of options
// experiments may touch (the context-management flag set) plus each
// flag's current default. Baseline keys are hashed over this
// projection; extending the list re-keys every baseline.
type FlagsManifest struct {
	Defaults map[string]any `json:"flag_defaults"`
}

// LoadFlagsManifest reads eval/flags.json. A missing manifest yields an
// empty projection — legal until the first experiment needs it.
func LoadFlagsManifest(evalDir string) (*FlagsManifest, error) {
	data, err := os.ReadFile(filepath.Join(evalDir, "flags.json"))
	if os.IsNotExist(err) {
		return &FlagsManifest{Defaults: map[string]any{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read flags manifest: %w", err)
	}
	var m FlagsManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse flags.json: %w", err)
	}
	if m.Defaults == nil {
		m.Defaults = map[string]any{}
	}
	if problems := validateFlagSet(sortedKeys(m.Defaults), "flags.json key"); len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	return &m, nil
}

// EffectiveConfig resolves the flag projection's value for a run:
// manifest defaults overlaid with the arm's options delta.
func (m *FlagsManifest) EffectiveConfig(armOptions map[string]any) map[string]any {
	out := make(map[string]any, len(m.Defaults))
	for k, v := range m.Defaults {
		out[k] = v
	}
	for k := range m.Defaults {
		if v, ok := armOptions[k]; ok {
			out[k] = v
		}
	}
	return out
}

// BaselineKey is the config hash a run belongs under for baseline
// purposes — the projection of its effective config.
func (m *FlagsManifest) BaselineKey(armOptions map[string]any) string {
	return m.keyWith(armOptions, nil)
}

// keyWith is BaselineKey plus extra dimensions folded into the hash
// (e.g. "$temperature") — run conditions that aren't config.Options.
func (m *FlagsManifest) keyWith(armOptions map[string]any, extra map[string]any) string {
	cfg := m.EffectiveConfig(armOptions)
	for k, v := range extra {
		cfg[k] = v
	}
	return BaselineConfigHash(append(sortedKeys(m.Defaults), sortedKeys(extra)...), cfg)
}

// ValidateArmFlags enforces the declared-manifest rule: an option an
// arm varies must be a manifest flag, otherwise flips would rotate
// baselines without re-keying. Structured flags get resolved here too
// — a memory_params typo that passed name-checking would otherwise
// fail inside every run's app.New, turning a manifest error into an
// inconclusive loop.
func (m *FlagsManifest) ValidateArmFlags(e *Experiment) error {
	for armName, arm := range e.Arms {
		for k, v := range arm.Config.Options {
			if _, ok := m.Defaults[k]; !ok {
				return fmt.Errorf("arm %q sets option %q which is not in eval/flags.json — flags under test must be declared so baseline keys rotate correctly", armName, k)
			}
			if k == "memory_params" {
				ov, ok := v.(map[string]any)
				if !ok {
					return fmt.Errorf("arm %q memory_params must be an object of param-name → value, got %T", armName, v)
				}
				if _, err := params.ResolveMemory(ov); err != nil {
					return fmt.Errorf("arm %q memory_params: %w", armName, err)
				}
			}
			if k == "edge_params" {
				ov, ok := v.(map[string]any)
				if !ok {
					return fmt.Errorf("arm %q edge_params must be an object of param-name → value, got %T", armName, v)
				}
				if _, err := params.ResolveEdges(ov); err != nil {
					return fmt.Errorf("arm %q edge_params: %w", armName, err)
				}
			}
		}
		// The arm's .crush.json blocks get the same validation contract:
		// a malformed hook or tools key must fail here, at manifest
		// validation, rather than inside every run's app.New.
		if err := validateArmHooks(armName, arm.Config.Hooks); err != nil {
			return err
		}
		if err := validateArmTools(armName, arm.Config.Tools); err != nil {
			return err
		}
	}
	return nil
}

// validateArmHooks schema-checks an arm's hooks block the way the
// config loader reads it — strict decode into config.HookConfig, then
// config.ValidateHooks runs the production checks (known event,
// command required, matcher only on tool events, matcher compiles), so
// a bad hook fails at manifest validation instead of inside every
// run's app.New. Event names are normalized in place — mergeArmBlock
// and the baseline hash then see canonical keys, not arm spelling.
// Commands are eval-scoped: they may only reference paths inside the
// run workdir, so absolute host paths and ~/../ escapes are rejected
// — a hook keyed on host files would measure behavior the harness
// can't reproduce (#391 allow-list).
func validateArmHooks(armName string, block map[string]any) error {
	if len(block) == 0 {
		return nil
	}
	// Two spellings of one event ("PreToolUse" + "pre_tool_use") would
	// normalize onto a single slot — the concatenation order is
	// map-iteration random, so reject the ambiguity rather than let the
	// merge (and hash) land on a nondeterministic hook order.
	seen := map[string]string{}
	for event := range block {
		canonical, ok := hooks.NormalizeEvent(event)
		if !ok {
			return fmt.Errorf("arm %q hooks: unknown event %q", armName, event)
		}
		if prev, dup := seen[canonical]; dup {
			return fmt.Errorf("arm %q hooks: event %q declared under two spellings (%q and %q) — pick one", armName, canonical, prev, event)
		}
		seen[canonical] = event
	}
	data, err := json.Marshal(block)
	if err != nil {
		return fmt.Errorf("arm %q hooks: %w", armName, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var hookMap map[string][]config.HookConfig
	if err := dec.Decode(&hookMap); err != nil {
		return fmt.Errorf("arm %q hooks: decode: %w", armName, err)
	}
	// config.ValidateHooks normalizes event keys on the decoded map in
	// place; write it back so materialize/merge and the baseline hash
	// see the canonical names (a fixture's "PreToolUse" and an arm's
	// "pre_tool_use" are the same slot — without this the arm's list
	// would concatenate with the fixture's instead of replacing it).
	cfg := &config.Config{Hooks: hookMap}
	if err := cfg.ValidateHooks(); err != nil {
		return fmt.Errorf("arm %q hooks: %w", armName, err)
	}
	normed := map[string]any{}
	for event, list := range cfg.Hooks {
		raw, err := json.Marshal(list)
		if err != nil {
			return fmt.Errorf("arm %q hooks[%q]: %w", armName, event, err)
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("arm %q hooks[%q]: %w", armName, event, err)
		}
		normed[event] = v
	}
	for k := range block {
		delete(block, k)
	}
	for k, v := range normed {
		block[k] = v
	}
	for event, list := range cfg.Hooks {
		for i, hc := range list {
			if hc.Timeout < 0 {
				return fmt.Errorf("arm %q hooks[%q][%d]: timeout must be >= 0 seconds, got %d", armName, event, i, hc.Timeout)
			}
			if err := checkHookCommandScope(armName, event, i, hc.Command); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkHookCommandScope enforces the workdir allow-list on a hook
// command: every token (and the value side of VAR=/flag= assignments)
// that looks like a path must resolve inside the workdir — filepath.
// Clean resolves ./../ and embedded .. segments, so only commands
// whose paths never escape pass. It is a lint, not a boundary: shell
// indirection ($VAR, $(...) substitution, concatenated -I../flag
// forms, interpreter -c payloads) isn't scanned — the #391 runtime
// allow-list, not this check, is the containment guarantee.
func checkHookCommandScope(armName, event string, i int, command string) error {
	reject := func(tok string) error {
		return fmt.Errorf("arm %q hooks[%q][%d]: command references %q — hook commands may only touch paths inside the run workdir (absolute host paths and ~/../ escapes are forbidden)", armName, event, i, tok)
	}
	for _, tok := range strings.Fields(command) {
		for _, part := range strings.Split(tok, "=") {
			part = strings.Trim(part, `"'`)
			if part == "" {
				continue
			}
			if strings.HasPrefix(part, "~") || filepath.IsAbs(part) {
				return reject(part)
			}
			clean := filepath.ToSlash(filepath.Clean(part))
			for _, seg := range strings.Split(clean, "/") {
				if seg == ".." {
					return reject(part)
				}
			}
		}
	}
	return nil
}

// validateArmTools schema-checks an arm's tools block strictly against
// config.Tools — an unknown sub-tool or field fails here instead of
// silently no-oping inside the run's config load. The config types
// carry no bounds of their own; negatives are the only values that
// could turn a typo into a run-time failure, so they're rejected.
func validateArmTools(armName string, block map[string]any) error {
	if len(block) == 0 {
		return nil
	}
	data, err := json.Marshal(block)
	if err != nil {
		return fmt.Errorf("arm %q tools: %w", armName, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var tools config.Tools
	if err := dec.Decode(&tools); err != nil {
		return fmt.Errorf("arm %q tools: %w", armName, err)
	}
	negErr := func(field string) error {
		return fmt.Errorf("arm %q tools: %s must be >= 0", armName, field)
	}
	if tools.Ls.MaxDepth != nil && *tools.Ls.MaxDepth < 0 {
		return negErr("ls.max_depth")
	}
	if tools.Ls.MaxItems != nil && *tools.Ls.MaxItems < 0 {
		return negErr("ls.max_items")
	}
	if tools.Grep.Timeout != nil && *tools.Grep.Timeout < 0 {
		return negErr("grep.timeout")
	}
	if tools.Glob.Timeout != nil && *tools.Glob.Timeout < 0 {
		return negErr("glob.timeout")
	}
	return nil
}

// LoadBands reads bands.json; a missing file yields empty state.
func LoadBands(evalDir string) (*Bands, error) {
	path := filepath.Join(evalDir, "bands.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Bands{SchemaVersion: 1, Entries: map[string]BandEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read bands.json: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse bands.json: %w", err)
	}
	b := &Bands{SchemaVersion: 1, Entries: map[string]BandEntry{}}
	for k, v := range raw {
		if k == "_schema_version" {
			var ver int
			if err := json.Unmarshal(v, &ver); err == nil && ver != 1 {
				return nil, fmt.Errorf("bands.json schema_version %d unsupported (want 1)", ver)
			}
			continue
		}
		var e BandEntry
		if err := json.Unmarshal(v, &e); err != nil {
			return nil, fmt.Errorf("parse bands entry %q: %w", k, err)
		}
		b.Entries[k] = e
	}
	return b, nil
}

// Save writes bands.json.
func (b *Bands) Save(evalDir string) error {
	raw := map[string]any{"_schema_version": b.SchemaVersion}
	for k, v := range b.Entries {
		raw[k] = v
	}
	data, err := json.MarshalIndent(raw, "", "\t")
	if err != nil {
		return fmt.Errorf("marshal bands.json: %w", err)
	}
	// Write-then-rename: a crash mid-write must not corrupt the file
	// every gate reads.
	path := filepath.Join(evalDir, "bands.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Band returns the trajectory's band, defaulting to uncharacterized.
func (b *Bands) Band(id string) Band {
	if e, ok := b.Entries[id]; ok {
		return e.Band
	}
	return BandUncharacterized
}

// Entry returns the band entry, creating an empty one if absent.
// The pointer addresses a COPY — mutations evaporate unless followed
// by Put(id, *e).
func (b *Bands) Entry(id string) *BandEntry {
	if b.Entries == nil {
		b.Entries = map[string]BandEntry{}
	}
	e, ok := b.Entries[id]
	if !ok {
		e = BandEntry{Band: BandUncharacterized}
	}
	return &e
}

// Put stores the entry back.
func (b *Bands) Put(id string, e BandEntry) {
	if b.Entries == nil {
		b.Entries = map[string]BandEntry{}
	}
	b.Entries[id] = e
}

// Baseline returns the windowed counts for a (model, baselineKey) pair.
func (b *Bands) Baseline(id, model, baselineKey string) BaselineCounts {
	e, ok := b.Entries[id]
	if !ok {
		return BaselineCounts{}
	}
	byCfg, ok := e.Baselines[model]
	if !ok {
		return BaselineCounts{}
	}
	return byCfg[baselineKey]
}

// Recompute rebuilds a trajectory's characterization state from the
// accumulated run records. It is the byproduct pass: every paired
// comparison adds baseline samples for the keys it ran under.
//
// records are all known run records for this trajectory (any
// experiment, including _characterize). contentHash is the trajectory's
// current corpus hash — samples scored under a different revision are
// discarded (a check fix invalidates the old scoring function).
// baselineKey selects which effective-config condition the run counted
// toward; each record carries its own baseline_key.
//
// curModel/curKey name the current default condition — the model pin
// plus the manifest-default baseline key. Band assignment and the
// diagnostic scans read only that key: a stale model's deep baseline
// must not keep a trajectory stable after a re-pin, and a fixed check
// must not be re-quarantined by its own pre-fix failures.
func (b *Bands) Recompute(id string, records []RunRecord, contentHash string, now time.Time, curModel, curKey string) {
	e := b.Entry(id)
	// Put via closure: defer evaluates arguments immediately, so
	// defer b.Put(id, *e) would snapshot the pre-Recompute entry.
	defer func() { b.Put(id, *e) }()

	if e.Band == BandQuarantined {
		// Stays until re-validated by the quarantine pass — even
		// across content changes, since a new revision still needs
		// explicit re-validation, not silent release.
		e.ContentHash = contentHash
		return
	}

	if e.ContentHash != contentHash {
		// Corpus revision changed — samples scored by the old
		// function are a different trajectory's data.
		e.Baselines = nil
		e.Band = BandUncharacterized
		e.QuarantineReason = ""
		e.PromotionStreak = 0
		e.ContentHash = contentHash
	}

	// Group conclusive records by (model, baseline_key), windowed.
	type key struct{ model, cfg string }
	grouped := map[key][]RunRecord{}
	for _, r := range records {
		if !r.Outcome.Conclusive() {
			continue
		}
		if r.Env.ContentHash != contentHash {
			// Empty-hash records (pre-field) are stale too — they
			// can't be proven current.
			continue
		}
		if r.Env.ModelResolved == "" {
			// Telemetry-less records (pre-field, crashed pre-emission)
			// keep intent keys under an empty model — accumulating
			// them litters bands.json with Baselines[""] cells that
			// nothing joins. Still valid outcomes elsewhere.
			continue
		}
		k := key{r.Env.ModelResolved, r.BaselineKey}
		grouped[k] = append(grouped[k], r)
	}

	e.Baselines = map[string]map[string]BaselineCounts{}
	for k, recs := range grouped {
		sort.Slice(recs, func(i, j int) bool { return recs[i].StartedAt.After(recs[j].StartedAt) })
		windowed := recs
		if len(windowed) > BaselineWindowRuns {
			windowed = windowed[:BaselineWindowRuns]
		}
		cutoff := now.AddDate(0, 0, -BaselineWindowDays)
		n := 0
		passes := 0
		for _, r := range windowed {
			if r.StartedAt.Before(cutoff) {
				break
			}
			n++
			if r.Outcome == OutcomePass {
				passes++
			}
		}
		if n == 0 {
			continue
		}
		if e.Baselines[k.model] == nil {
			e.Baselines[k.model] = map[string]BaselineCounts{}
		}
		e.Baselines[k.model][k.cfg] = BaselineCounts{
			Passes:  passes,
			N:       n,
			LastRun: windowed[0].StartedAt.Format("2006-01-02"),
		}
	}

	b.assignBand(e, records, contentHash, curModel, curKey)
	// "Last recompute" rather than "last new data" — callers can't
	// distinguish a no-sample pass without diffing record counts.
	e.LastCharacterized = now.Format("2006-01-02")
}

// recordMatchesPin reports whether a record was produced under the
// given pin: stamped pins compare exactly, so a re-pinned model's
// records never count toward the new pin's condition. Unstamped
// (pre-field) records fall back to resolved-model equality.
func recordMatchesPin(r RunRecord, model string) bool {
	if r.Env.ModelPin != "" {
		return r.Env.ModelPin == model
	}
	return r.Env.ModelResolved == model
}

// currentConditionKey returns the resolved baseline key stamped on
// the newest record under the given arms and current pin — the join
// key for bands and the gate. Records hash what actually ran;
// resolved values can diverge from manifest intent on options
// NormalizeOptions doesn't materialize (nil *bool, 0 int), so intent
// is only the bootstrap fallback for record-less calls.
//
// Only records carrying ResolvedOptions may be join sources: a run
// that died before telemetry (config-load failure, hard kill) keeps
// the intent key stamped at ExecuteRun start — accepting it would
// flip curKey back to the intent namespace and orphan every
// resolved-keyed baseline cell. Returns "" when no qualifying
// record exists.
func currentConditionKey(records []RunRecord, model string, arms ...string) string {
	key := ""
	var latest time.Time
	for _, r := range records {
		if r.BaselineKey == "" || len(r.ResolvedOptions) == 0 {
			continue
		}
		if !slices.Contains(arms, r.Arm) {
			continue
		}
		if model != "" && !recordMatchesPin(r, model) {
			continue
		}
		if r.StartedAt.After(latest) {
			latest, key = r.StartedAt, r.BaselineKey
		}
	}
	return key
}

// assignBand classifies the trajectory from its recomputed state,
// applying hysteresis: demote on one bad characterization, promote on
// two consecutive good ones.
func (b *Bands) assignBand(e *BandEntry, records []RunRecord, contentHash, curModel, curKey string) {
	matchesPin := func(r RunRecord) bool { return recordMatchesPin(r, curModel) }

	// revision = current-pin records scored under the current corpus
	// hash — suspect_check scans this whole set (a flaky check is a
	// script property, not a per-condition one), but it must still be
	// pin-filtered: characterize/smoke records share the "baseline"
	// arm name across pins, so a re-pin boundary would interleave
	// pass/fail into a false alternation.
	var revision []RunRecord
	for _, r := range records {
		if r.Env.ContentHash != "" && r.Env.ContentHash != contentHash {
			continue
		}
		if !matchesPin(r) {
			continue
		}
		revision = append(revision, r)
	}
	sort.Slice(revision, func(i, j int) bool { return revision[i].StartedAt.After(revision[j].StartedAt) })

	// effModel is the resolved spelling of the current pin — the
	// baseline storage key — taken from the newest record produced
	// under it. When the pin has produced nothing yet (fresh re-pin),
	// curModel itself is the lookup and the scans legitimately empty.
	effModel := curModel
	var latest time.Time
	for _, r := range revision {
		if r.BaselineKey == curKey && r.Outcome.Conclusive() &&
			r.Env.ModelResolved != "" && matchesPin(r) && r.StartedAt.After(latest) {
			latest = r.StartedAt
			effModel = r.Env.ModelResolved
		}
	}

	// current = records under this (pin, baseline key) condition —
	// never_passed's trailing count is per baseline key.
	var current []RunRecord
	for _, r := range revision {
		if r.BaselineKey == curKey && matchesPin(r) {
			current = append(current, r)
		}
	}

	// never_passed: zero passes in the trailing NeverPassedFails
	// conclusive runs under this condition. A trajectory with
	// lifetime passes hitting the same streak is rot, not
	// beyond-model — that routes through suspect_check/environment
	// alarms, not here.
	consecFails := 0
	for _, r := range current {
		if !r.Outcome.Conclusive() {
			continue
		}
		if r.Outcome == OutcomePass {
			break
		}
		consecFails++
	}
	// everPassed is lifetime: passes under a different baseline key
	// (treatment arm, pre-flip default) still mean "can pass" — a
	// 0/N streak after that is rot, not beyond-model.
	everPassed := false
	for _, r := range revision {
		if r.Outcome == OutcomePass {
			everPassed = true
			break
		}
	}
	if !everPassed && consecFails >= NeverPassedFails {
		e.Band = BandQuarantined
		e.QuarantineReason = ReasonNeverPassed
		return
	}

	// suspect_check: quarantine exercises agent-free states, so a
	// check flaky only on agent-produced states (port binding, races)
	// passes quarantine then masquerades as mid band. Consecutive
	// within-arm pass/fail alternation is the empirical net — arms
	// run interleaved, so the scan must split by arm or a real
	// treatment effect (P,F,P,F…) reads as flakiness. The scan covers
	// every current-revision record, not just the current key: a
	// check flaky only under the flag-on config alternates inside
	// the treatment arm and must not surface as an apparent
	// regression.
	if suspectCheck(revision) {
		e.Band = BandQuarantined
		e.QuarantineReason = ReasonSuspectCheck
		return
	}

	// Band reads the baseline under the current default condition.
	best := BaselineCounts{}
	if byCfg, ok := e.Baselines[effModel]; ok {
		best = byCfg[curKey]
	}

	good := best.N >= StableMinN && best.PHat() >= StablePHatFloor
	switch e.Band {
	case BandStable:
		if !good {
			// One bad characterization demotes immediately.
			e.Band = bandFor(best)
			e.PromotionStreak = 0
		}
	default:
		if good {
			e.PromotionStreak++
			if e.PromotionStreak >= PromotionStreakRequired {
				e.Band = BandStable
				e.PromotionStreak = 0
			} else {
				e.Band = bandFor(best)
			}
		} else {
			e.PromotionStreak = 0
			e.Band = bandFor(best)
		}
	}
}

// bandFor classifies counts without hysteresis.
func bandFor(c BaselineCounts) Band {
	if c.N < GenesisRuns {
		return BandUncharacterized
	}
	return BandMid // Stable additionally requires the promotion streak.
}

// suspectCheck reports whether any arm's conclusive sequence
// alternates pass/fail more than a real check should — a runs-style
// heuristic: ≥8 conclusive samples within one arm with alternation
// rate above 0.7 flags the check rather than letting the noise absorb
// into p̂. Splitting by arm is load-bearing: arms interleave in time,
// so a genuine treatment effect (control passes, treatment fails every
// round) alternates perfectly in the pooled stream while each arm is
// individually constant.
func suspectCheck(orderedDesc []RunRecord) bool {
	// Group by (arm, baseline key): two experiments' same-named arms
	// under different flag configs must not pool into one stream.
	byArm := map[string][]Outcome{}
	for _, r := range orderedDesc {
		if r.Outcome == OutcomePass || r.Outcome == OutcomeFail {
			k := r.Arm + "|" + r.BaselineKey
			byArm[k] = append(byArm[k], r.Outcome)
		}
	}
	for _, outcomes := range byArm {
		if len(outcomes) < 8 {
			continue
		}
		alternations := 0
		for i := 1; i < len(outcomes); i++ {
			if outcomes[i] != outcomes[i-1] {
				alternations++
			}
		}
		if float64(alternations)/float64(len(outcomes)-1) > 0.7 {
			return true
		}
	}
	return false
}
