package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
)

// TB21Seed is the frozen draw seed for the Terminal-Bench 2.x dev/sealed
// split (#439). The value is arbitrary — it derives from the issue number —
// and is recorded here and in the generated manifests so the split can be
// re-derived byte-for-byte. Never change it: doing so re-deals the split.
const TB21Seed uint64 = 439

// TB21DevCount is the number of tasks dealt into the development split. The
// remainder (89 − 59 = 30) forms the sealed split.
const TB21DevCount = 59

// TB21ManifestsDir is where the frozen manifests live inside the eval dir.
const TB21ManifestsDir = "manifests"

// TB21Source pins the upstream the task list was vendored from: the harbor
// dataset registry entry and the exact git commit of the task repository.
// The commit is what freezes the corpus — registry versions can drift.
type TB21Source struct {
	RegistryURL string `json:"registry_url"`
	GitURL      string `json:"git_url"`
	Commit      string `json:"commit"`
}

// TB21TaskSource is eval/manifests/tb21-tasks.json: the vendored,
// commit-pinned list of all tasks in the corpus. Regenerating this file is
// a deliberate act — it is the input the split is frozen against.
type TB21TaskSource struct {
	Dataset string     `json:"dataset"`
	Version string     `json:"version"`
	Source  TB21Source `json:"source"`
	Tasks   []string   `json:"tasks"`
}

// TB21Manifest is one frozen split list (dev or sealed). The task_sha256
// covers the canonical tasks array so a manifest verifies itself without
// re-deriving the draw.
type TB21Manifest struct {
	Dataset    string     `json:"dataset"`
	Version    string     `json:"version"`
	Source     TB21Source `json:"source"`
	Split      string     `json:"split"`
	Seed       uint64     `json:"seed"`
	TaskSHA256 string     `json:"task_sha256"`
	// AccessProtocol names who may read this list and when. The dev list is
	// unrestricted — lever development iterates against it. The sealed list
	// is read only at phase exits (R1.5 lever acceptance, R2 promotion):
	// enumerating it during development leaks held-out information into
	// arm selection and invalidates the split.
	AccessProtocol string   `json:"access_protocol"`
	Tasks          []string `json:"tasks"`
}

// TB21 access-protocol strings stamped into the manifests.
const (
	TB21AccessDev    = "unrestricted"
	TB21AccessSealed = "phase-exit only: do not enumerate during lever development"
)

// TB21SplitTasks performs the seeded draw: the task list is sorted (so the
// draw is independent of registry ordering), shuffled with a PCG stream
// keyed by seed, and cut at devCount. Both outputs are returned sorted —
// the manifest's reviewability matters more than draw order, and run order
// is a harness concern.
func TB21SplitTasks(tasks []string, seed uint64, devCount int) (dev, sealed []string) {
	shuffled := slices.Clone(tasks)
	slices.Sort(shuffled)
	rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)).Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})
	dev = slices.Clone(shuffled[:devCount])
	sealed = slices.Clone(shuffled[devCount:])
	slices.Sort(dev)
	slices.Sort(sealed)
	return dev, sealed
}

// TB21TaskHash is the self-verifying digest of a manifest's task list: the
// SHA-256 of the compact JSON encoding of the sorted names. Sorting makes
// the hash order-independent — it attests membership, not file layout.
func TB21TaskHash(tasks []string) string {
	sorted := slices.Clone(tasks)
	slices.Sort(sorted)
	data, _ := json.Marshal(sorted)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TB21DevManifest builds the frozen dev manifest for a vendored source.
func TB21DevManifest(src *TB21TaskSource, seed uint64, devCount int) *TB21Manifest {
	dev, _ := TB21SplitTasks(src.Tasks, seed, devCount)
	return &TB21Manifest{
		Dataset:        src.Dataset,
		Version:        src.Version,
		Source:         src.Source,
		Split:          "dev",
		Seed:           seed,
		TaskSHA256:     TB21TaskHash(dev),
		AccessProtocol: TB21AccessDev,
		Tasks:          dev,
	}
}

// TB21SealedManifest builds the frozen sealed manifest. Callers should
// treat the returned task list per AccessProtocol — it exists so the
// freeze can be verified, not for development-time iteration.
func TB21SealedManifest(src *TB21TaskSource, seed uint64, devCount int) *TB21Manifest {
	_, sealed := TB21SplitTasks(src.Tasks, seed, devCount)
	return &TB21Manifest{
		Dataset:        src.Dataset,
		Version:        src.Version,
		Source:         src.Source,
		Split:          "sealed",
		Seed:           seed,
		TaskSHA256:     TB21TaskHash(sealed),
		AccessProtocol: TB21AccessSealed,
		Tasks:          sealed,
	}
}

// LoadTB21TaskSource reads the vendored corpus list from
// <evalDir>/manifests/tb21-tasks.json and checks its shape.
func LoadTB21TaskSource(evalDir string) (*TB21TaskSource, error) {
	path := filepath.Join(evalDir, TB21ManifestsDir, "tb21-tasks.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tb21 task source: %w", err)
	}
	var src TB21TaskSource
	if err := json.Unmarshal(data, &src); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(src.Tasks) == 0 {
		return nil, fmt.Errorf("%s: tasks is empty", path)
	}
	return &src, nil
}

// LoadTB21Manifest reads <evalDir>/manifests/tb21-<split>.json and verifies
// the embedded task hash — a manifest that fails its own digest is treated
// as corrupt rather than silently consumed. For the sealed split, callers
// must honor AccessProtocol: the list exists for phase-exit reads, not for
// development-time enumeration.
func LoadTB21Manifest(evalDir, split string) (*TB21Manifest, error) {
	path := filepath.Join(evalDir, TB21ManifestsDir, "tb21-"+split+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tb21 %s manifest: %w", split, err)
	}
	var m TB21Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if m.Split != split {
		return nil, fmt.Errorf("%s: split field is %q", path, m.Split)
	}
	if got := TB21TaskHash(m.Tasks); got != m.TaskSHA256 {
		return nil, fmt.Errorf("%s: task hash mismatch (manifest %s, computed %s)", path, m.TaskSHA256, got)
	}
	return &m, nil
}

// WriteTB21Manifest serializes a split manifest deterministically (tab
// indent, trailing newline, tasks already sorted by construction).
func WriteTB21Manifest(m *TB21Manifest, path string) error {
	data, err := json.MarshalIndent(m, "", "\t")
	if err != nil {
		return fmt.Errorf("marshal tb21 manifest: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
