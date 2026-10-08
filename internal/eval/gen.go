// Parametric corpus generator for the memory ladders (#223): one
// spec produces a complete trajectory dir — Go fixture, authored
// seed_commands, dose gate, check — so depth/distractor cells scale
// to fresh instances instead of reusing the same few trajectories.
//
// The cell is a single behavioral defect in a generated package
// (Contract() returns 41, must return 42). The dimensions:
//
//	quirks       M relevant memory rows about the target — the depth
//	             dose. 0 seeds nothing; each level adds one authored
//	             row of a distinct kind (open failure → invocation
//	             command → resolved sibling → second open surface).
//	distractors  K wrong-referent rows seeded alongside.
//	plausibility how near-miss the distractors are:
//	             low    benign successes on unrelated packages
//	             mid    failures on nonexistent/typo'd packages
//	             high   build-tag-hidden failing tests — identical
//	                    failure shape to the target, invisible to the
//	                    check's `go test ./...` so they never have to
//	                    be fixed.
//	prompt       vague ("some tests fail") | explicit (names the
//	             target package and the contract value).
//	depth        package nesting under the fixture root — the
//	             discovery-cost knob (#227): flat target vs
//	             internal/core/store style paths.
//	seed         RNG seed; quirk identity and placement are sampled
//	             per instance, so a replicate never re-measures the
//	             same quirk — and a sealed held-out pool (#224) is
//	             the same generator with name pools held back.
//
// Instances are concrete dirs: the seed snapshot replays within an
// instance's attempts, and the per-replicate variance lives across
// instances — the same decoupling the snapshot is for.
package eval

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// GenSpec is one ladder cell's draw — the dimensions are
// trajectory-schema-free (the emitted spec is a plain trajectory),
// so a generated instance is indistinguishable from a hand-written
// one to the runner.
type GenSpec struct {
	Quirks       int    `json:"quirks"`
	Distractors  int    `json:"distractors"`
	Plausibility string `json:"plausibility"` // low | mid | high
	Prompt       string `json:"prompt"`       // vague | explicit
	Depth        int    `json:"depth"`        // extra nest dirs above the target package
	Seed         int64  `json:"seed"`
	Replicate    int    `json:"replicate"`
}

// genID is the corpus identity — params plus the draw's seed in
// the name so a manifest reads the cell from the trajectory id
// alone and two draws of the same cell can't share a directory.
func (s GenSpec) genID() string {
	return fmt.Sprintf("gen-m%dk%d-%s-%s-d%d-s%03d-%03d",
		s.Quirks, s.Distractors, s.Plausibility, s.Prompt, s.Depth, s.Seed, s.Replicate+1)
}

// Validate rejects dimensions the generator cannot express
// honestly — a silently-clamped cell would measure the wrong dose.
func (s GenSpec) Validate() error {
	if s.Quirks < 0 || s.Quirks > 4 {
		return fmt.Errorf("quirks %d — the authored ladder is 0..4 relevant rows", s.Quirks)
	}
	// Keep one undrawn name in reserve: a draw that collides with
	// the target (quota, ledger live in both pools) swaps the
	// colliding slot for a tail entry — an empty tail can't swap.
	if s.Distractors < 0 || s.Distractors > len(genDistractorNames)-1 {
		return fmt.Errorf("distractors %d — name pool holds %d, one reserved for collision repair",
			s.Distractors, len(genDistractorNames))
	}
	switch s.Plausibility {
	case "low", "mid", "high", "in_scope":
	default:
		return fmt.Errorf("plausibility %q is not low|mid|high|in_scope", s.Plausibility)
	}
	switch s.Prompt {
	case "vague", "explicit":
	default:
		return fmt.Errorf("prompt %q is not vague|explicit", s.Prompt)
	}
	if s.Depth < 0 || s.Depth > len(genNestNames) {
		return fmt.Errorf("depth %d — nest pool holds %d", s.Depth, len(genNestNames))
	}
	return nil
}

var (
	// Target package names — domain-flavored so the vague prompt's
	// referent is findable but not free.
	genTargetNames = []string{
		"settle", "prorate", "quota", "anchor", "ledger",
		"window", "budget", "margin",
	}
	// Nest directories for the discovery-cost dimension.
	genNestNames = []string{"internal", "core", "modules", "services"}
	// Sibling packages carrying the resolved-fix evidence (M≥3).
	genSiblingNames = []string{"fees", "tax", "credit", "rate"}
	// Distractor referents — wrong names the memory can point at.
	// The pool must cover the distractor ladder's top dose (K=50):
	// each name becomes a fixture package or a stale-name command,
	// so every entry is a valid Go package identifier.
	genDistractorNames = []string{
		"auth", "cache", "queue", "codec", "sync", "batch",
		"index", "parse", "render", "watch", "relay", "store",
		"emit", "guard", "probe", "trace", "shard", "broker",
		"ingest", "export", "notify", "audit", "quota", "token",
		"cipher", "compress", "catalog", "ledger", "vector",
		"matrix", "tensor", "kernel", "driver", "socket",
		"router", "filter", "mapper", "reducer", "merger",
		"spliter", "joiner", "fetcher", "pusher", "puller",
		"tracker", "counter", "sampler", "batcher", "chainer",
		"wrapper", "adapter", "bridge", "proxy", "gateway",
		"ingress", "egress", "policy", "permit", "license",
		"voucher", "invoice", "billing", "meter",
	}
)

// genDraw is the per-instance sampling result: which names play
// which roles. Fixed by (Seed, Replicate) — two runs of the same
// spec emit byte-identical instances, so snapshot keys stay stable.
type genDraw struct {
	target     string   // target package name
	targetPath string   // slash path from fixture root
	sibling    string   // resolved-evidence package (M≥3)
	distractor []string // wrong referents, per distractor row
	hidden     []string // build-tag-hidden packages (high plausibility)
	fine       []string // healthy packages (low plausibility)
}

func (s GenSpec) draw() genDraw {
	rng := rand.New(rand.NewPCG(uint64(s.Seed), uint64(s.Replicate)))
	shuffle := func(pool []string) []string {
		out := slices.Clone(pool)
		rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	}
	d := genDraw{target: shuffle(genTargetNames)[0]}
	nest := shuffle(genNestNames)[:s.Depth]
	d.targetPath = strings.Join(append(nest, d.target), "/")
	if s.Quirks >= 3 {
		d.sibling = shuffle(genSiblingNames)[0]
	}
	shuffled := shuffle(genDistractorNames)
	d.distractor = shuffled[:s.Distractors]
	// The pools share names (quota, ledger) so a draw can put the
	// target in its own distractor set — a stale-name or healthy
	// package that binds the target and contaminates the cell.
	// Swap collisions for an undrawn pool name; the shuffle itself
	// is untouched, so clean draws — every committed cell — keep
	// their bytes and snapshot keys.
	for i, name := range d.distractor {
		if name != d.target {
			continue
		}
		for _, spare := range shuffled[s.Distractors:] {
			if spare != d.target {
				d.distractor[i] = spare
				break
			}
		}
	}
	switch s.Plausibility {
	case "high":
		// Near-miss referents: names adjacent to the target's —
		// the typo/suffix variants a real confused memory would hold.
		for _, name := range d.distractor {
			d.hidden = append(d.hidden, name+"calc")
		}
	case "mid":
		// Stale-name failures: the referent exists in memory but
		// not on disk — 'go test ./authx' for a renamed package.
		for _, name := range d.distractor {
			d.hidden = append(d.hidden, name+"x")
		}
	case "in_scope":
		// The only distractor class that binds: a wrong failure
		// inside the target's own scope — a tag-hidden decoy test
		// named after the distractor — so scope binding can't
		// reject it the way every cross-package referent is
		// rejected. Rendered-j doses come from here.
	case "low":
		d.fine = d.distractor
	}
	return d
}

// Generate writes one instance of the spec under
// corpusRoot/<genID>/ and returns the dir. The emitted trajectory
// is a plain spec — fixture, authored seed_commands, dose gate,
// check — so every generator bug is gated by the same validation a
// hand-written cell gets.
func Generate(corpusRoot string, s GenSpec) (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	d := s.draw()
	dir := filepath.Join(corpusRoot, s.genID())
	w := func(rel, body string) error {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(body), 0o644)
	}

	if err := w("fixture/go.mod", "module "+genModuleName(d)+"\n\ngo 1.21\n"); err != nil {
		return "", err
	}
	if err := s.writeFixture(w, d); err != nil {
		return "", err
	}
	if err := w("check.sh", s.checkScript(d)); err != nil {
		return "", err
	}
	scripts := []string{"check.sh"}
	if len(s.seedCommands(d)) > 0 {
		if err := w("seed_check.sh", s.seedCheckScript(d)); err != nil {
			return "", err
		}
		scripts = append(scripts, "seed_check.sh")
	}
	// Executable bits on the scripts — WriteFile above wrote 0644.
	for _, sh := range scripts {
		if err := os.Chmod(filepath.Join(dir, sh), 0o755); err != nil {
			return "", err
		}
	}
	traj, err := s.trajectoryJSON(d)
	if err != nil {
		return "", err
	}
	if err := w("trajectory.json", traj); err != nil {
		return "", err
	}
	// The spec rides as a sidecar: ContentHash folds every file in
	// the dir, so a param change invalidates snapshots by itself.
	meta, _ := json.MarshalIndent(s, "", "  ")
	if err := w("gen.json", string(meta)+"\n"); err != nil {
		return "", err
	}
	return dir, nil
}

func genModuleName(d genDraw) string {
	return "genfix" + strings.ReplaceAll(d.target, "-", "")
}

// writeFixture emits the Go module: the broken target, the sibling
// (resolved-fix evidence for M≥3), and the distractor packages the
// plausibility tier needs.
func (s GenSpec) writeFixture(w func(rel, body string) error, d genDraw) error {
	// The defect is behavioral, not compile-time: `go build` and
	// `go vet` pass, `go test` fails — so M≥2/M≥4 rows of distinct
	// kinds can point at the same defect.
	target := fmt.Sprintf(`package %s

// Contract is the project's settlement constant — the integration
// contract requires 42.
func Contract() int {
	return 41
}
`, d.target)
	targetTest := fmt.Sprintf(`package %s

import "testing"

func TestContract(t *testing.T) {
	if Contract() != 42 {
		t.Fatalf("Contract() = %%d, want 42", Contract())
	}
}
`, d.target)
	if err := w("fixture/"+d.targetPath+"/contract.go", target); err != nil {
		return err
	}
	if err := w("fixture/"+d.targetPath+"/contract_test.go", targetTest); err != nil {
		return err
	}
	if d.sibling != "" {
		// The sibling ships broken too — the seed session fixes it
		// mid-run, producing the resolved-failure row whose repair
		// pattern is the M≥3 dose.
		sib := fmt.Sprintf(`package %s

// Fee returns the surcharge constant.
func Fee() int {
	return 9
}
`, d.sibling)
		sibTest := fmt.Sprintf(`package %s

import "testing"

func TestFee(t *testing.T) {
	if Fee() != 10 {
		t.Fatalf("Fee() = %%d, want 10", Fee())
	}
}
`, d.sibling)
		if err := w("fixture/"+d.sibling+"/fee.go", sib); err != nil {
			return err
		}
		if err := w("fixture/"+d.sibling+"/fee_test.go", sibTest); err != nil {
			return err
		}
	}
	for _, name := range d.fine {
		fine := fmt.Sprintf(`package %s

// Ping returns the healthcheck string.
func Ping() string {
	return "pong"
}
`, name)
		if err := w("fixture/"+name+"/ping.go", fine); err != nil {
			return err
		}
	}
	for _, name := range d.hidden {
		if s.Plausibility == "high" {
			// Build-tag-hidden failing test: the row is a real
			// `go test -tags private` failure with the target's
			// shape, but `./...` never compiles the test — the
			// distractor can't leak into the check.
			base := fmt.Sprintf(`package %s

// Stub exists so the package builds without the private tag.
func Stub() int {
	return 0
}
`, name)
			hiddenTest := fmt.Sprintf(`//go:build private

package %s

import "testing"

func TestStub(t *testing.T) {
	if Stub() != 42 {
		t.Fatalf("Stub() = %%d, want 42", Stub())
	}
}
`, name)
			if err := w("fixture/"+name+"/stub.go", base); err != nil {
				return err
			}
			if err := w("fixture/"+name+"/stub_private_test.go", hiddenTest); err != nil {
				return err
			}
		}
		// mid-tier distractors need no fixture — the whole point is
		// the referent doesn't exist on disk.
	}
	if s.Plausibility == "in_scope" {
		for _, name := range d.distractor {
			// The decoy lives in the target package behind the
			// private tag: `go test ./...` never compiles it, but
			// `go test -tags private -run Test<Name> ./<target>`
			// is a real failure in the target's own scope — a
			// stored wrong row that actually renders.
			decoy := fmt.Sprintf(`//go:build private

package %s

import "testing"

func Test%s(t *testing.T) {
	if Contract() != 99 {
		t.Fatalf("Contract() = %%d, want 99", Contract())
	}
}
`, d.target, titleName(name))
			if err := w("fixture/"+d.targetPath+"/decoy_"+name+"_private_test.go", decoy); err != nil {
				return err
			}
		}
	}
	return nil
}

// titleName upper-cases a package-name word into a Go test
// identifier — "auth" → "Auth".
func titleName(name string) string {
	return strings.ToUpper(name[:1]) + name[1:]
}

// seedCommands authors the memory rows. Ages stagger so the
// distractor noise sits days back while the target's debugging arc
// is recent (hours) — the same shape a real returning user leaves.
// Relevant rows are ordered oldest→newest; distractors interleave
// older, and the resolved sibling sits between them in age.
func (s GenSpec) seedCommands(d genDraw) []ScriptedSeed {
	var seeds []ScriptedSeed
	ago := func(hours float64) float64 { return hours * 3600 }

	// Distractors first — the oldest ambient rows. The stagger must
	// keep every row inside the open-failure TTL: a distractor
	// backdated past the read bound isn't a stored dose at all —
	// the fetch drops it before the selector ever sees it. Six-hour
	// steps hold K≤50 inside a fortnight.
	for i, name := range d.distractor {
		var cmd string
		switch s.Plausibility {
		case "low":
			// Benign success on a healthy package — a command row.
			cmd = "go test ./" + name
		case "mid":
			// Failure on a nonexistent referent — the stale name a
			// renamed package leaves in memory.
			cmd = "go test ./" + d.hidden[i]
		case "high":
			// Near-miss failure, tag-hidden from the check.
			cmd = "go test -tags private ./" + d.hidden[i]
		case "in_scope":
			// Wrong failure in the target's own scope — a hidden
			// decoy test. Binds where every cross-package row
			// rejects, so stored K becomes rendered j.
			cmd = "go test -tags private -run Test" + titleName(name) + " ./" + d.targetPath
		}
		seeds = append(seeds, ScriptedSeed{
			AgoSeconds: ago(48 + float64(i)*6),
			Commands:   []string{cmd},
		})
	}

	// The resolved sibling (M≥3) — a real mid-session fix so the
	// row resolves through RecordRun, not fixture SQL.
	if d.sibling != "" {
		fix := fmt.Sprintf("printf 'package %s\\n\\n// Fee returns the surcharge constant.\\nfunc Fee() int {\\n\\treturn 10\\n}\\n' > %s/fee.go",
			d.sibling, d.sibling)
		seeds = append(seeds, ScriptedSeed{
			AgoSeconds: ago(30),
			Commands: []string{
				"go test ./" + d.sibling,
				fix,
				"go test ./" + d.sibling,
			},
		})
	}

	// The target's debugging arc — each level of M adds one row of
	// a distinct kind, oldest→newest.
	if s.Quirks >= 4 {
		// A second failure surface on the target — same defect
		// observed through a different invocation.
		seeds = append(seeds, ScriptedSeed{
			AgoSeconds: ago(8),
			Commands:   []string{"go test -run TestContract -v ./" + d.targetPath},
		})
	}
	if s.Quirks >= 2 {
		// The healthy-invocation row: build passes where test
		// fails — the defect is behavioral.
		seeds = append(seeds, ScriptedSeed{
			AgoSeconds: ago(5),
			Commands:   []string{"go build ./" + d.targetPath},
		})
	}
	if s.Quirks >= 1 {
		seeds = append(seeds, ScriptedSeed{
			AgoSeconds: ago(2),
			Commands:   []string{"go test ./" + d.targetPath},
		})
	}
	return seeds
}

// trajectoryJSON emits the spec — the same document a hand-written
// cell would carry, so validation, hashing, and snapshot keys are
// uniform across corpus tiers.
func (s GenSpec) trajectoryJSON(d genDraw) (string, error) {
	turn := "Some tests in this repository are failing. Find and fix them so `go test ./...` passes. Make the smallest changes."
	if s.Prompt == "explicit" {
		turn = fmt.Sprintf("Fix the failing test in ./%s so `go test ./...` passes — Contract() must return 42. Make the smallest change.", d.targetPath)
	}
	spec := map[string]any{
		"id":             s.genID(),
		"schema_version": 1,
		"origin":         map[string]any{"kind": "synthetic"},
		"start_state":    map[string]any{"kind": "fixture", "fixture_dir": "fixture"},
		"task":           map[string]any{"turns": []string{turn}},
		"check": map[string]any{
			"script":             "check.sh",
			"expect_start_state": "fail",
		},
	}
	// An unseeded cell (M=0, K=0) has no authored state to gate —
	// the loader rightly rejects seed_script without seeding.
	if seeds := s.seedCommands(d); len(seeds) > 0 {
		spec["seed_commands"] = seeds
		spec["check"].(map[string]any)["seed_script"] = "seed_check.sh"
	}
	raw, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw) + "\n", nil
}

// checkScript scores the fix the same way regardless of dose: the
// suite passes and the contract constant is the real value — a
// stubbed test or a build-only patch fails the check.
func (s GenSpec) checkScript(d genDraw) string {
	return fmt.Sprintf(`#!/bin/bash
# The measured fix: the whole suite passes and the target's
# Contract is the contract value — not a stub that compiles past
# the prompt, not a deleted test.
cd "$EVAL_WORKDIR"
go test ./... || exit 1
grep -q 'return 42' %s/contract.go || exit 1
`, d.targetPath)
}

// seedCheckScript is the dose gate: the authored rows must exist in
// exactly the pools the cell's LOO arms ablate over — open failures,
// resolved failures, command rows — and the session count must match
// the authored seed count. A diverged seed rejects the run before
// the measured session is spent.
func (s GenSpec) seedCheckScript(d genDraw) string {
	wantOpen, wantResolved, wantCmdMin := s.wantPoolCounts()
	wantSessions := len(s.seedCommands(d))
	return fmt.Sprintf(`#!/bin/bash
# Dose gate: the scripted seeds must have produced exactly the
# three-pool state this cell was authored around.
cd "$EVAL_WORKDIR"
db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"
open_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in = '';")
resolved_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in != '';")
cmd_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM command_memory;")
seed_sessions=$(sqlite3 "$db" "SELECT COUNT(*) FROM sessions WHERE title LIKE 'seed %%';")
open_rows=${open_rows:-0}
resolved_rows=${resolved_rows:-0}
cmd_rows=${cmd_rows:-0}
seed_sessions=${seed_sessions:-0}
echo "EVAL_JSON {\"open_rows\":$open_rows,\"resolved_rows\":$resolved_rows,\"cmd_rows\":$cmd_rows,\"seed_sessions\":$seed_sessions}"
[ "$open_rows" -eq %d ] && [ "$resolved_rows" -eq %d ] && [ "$cmd_rows" -ge %d ] && [ "$seed_sessions" -eq %d ]
`, wantOpen, wantResolved, wantCmdMin, wantSessions)
}

// wantPoolCounts predicts what RecordRun produces for the authored
// commands — the gate asserts these exactly for the failure pools
// and as a floor for command rows. Derived from the authored spec,
// not re-simulated: open = target test surfaces + mid/high
// distractors; resolved = the sibling fix (M≥3); commands = every
// authored invocation lands in the ledger.
func (s GenSpec) wantPoolCounts() (open, resolved, cmdMin int) {
	if s.Quirks >= 1 {
		open++ // go test ./<target>
	}
	if s.Quirks >= 4 {
		open++ // go test -run TestContract -v ./<target>
	}
	if s.Plausibility != "low" {
		open += s.Distractors // mid/high/in_scope distractors are failures
	}
	if s.Quirks >= 3 {
		resolved++ // sibling: fail → printf fix → pass
	}
	// command_memory dedupes normalized (cmd, cwd) pairs — the
	// sibling's repeated `go test` is one row, so the floor counts
	// distinct authored commands, not invocations.
	seen := map[string]bool{}
	for _, seed := range s.seedCommands(s.draw()) {
		for _, cmd := range seed.Commands {
			if !seen[cmd] {
				seen[cmd] = true
				cmdMin++
			}
		}
	}
	return open, resolved, cmdMin
}
