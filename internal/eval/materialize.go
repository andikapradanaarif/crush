package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Materialize builds a fresh workdir for a run inside parentDir and
// returns its path. Every run gets a new tempdir — never the corpus or
// the real repo.
// env supplies the setup commands' environment — pass the runner's
// pinned env so e.g. `go mod download` populates the pinned module
// cache, not the operator's real HOME.
func Materialize(ctx context.Context, traj *Trajectory, trajDir, parentDir string, env []string) (string, error) {
	workdir, err := os.MkdirTemp(parentDir, "eval-run-*")
	if err != nil {
		return "", fmt.Errorf("create workdir: %w", err)
	}

	switch traj.StartState.Kind {
	case "fixture":
		src := filepath.Join(trajDir, traj.StartState.FixtureDir)
		if err := copyTree(src, workdir); err != nil {
			return "", fmt.Errorf("copy fixture: %w", err)
		}
	case "git":
		if err := gitCheckout(ctx, traj.StartState.Repo, traj.StartState.Ref, workdir); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unknown start_state kind %q", traj.StartState.Kind)
	}

	for _, cmdLine := range traj.StartState.Setup {
		if err := runShell(ctx, workdir, cmdLine, env); err != nil {
			return "", fmt.Errorf("setup %q: %w", cmdLine, err)
		}
	}
	return workdir, nil
}

// EvalRepoCacheEnvVar overrides the git start_state mirror cache
// location. Default is <user cache dir>/crush/eval-repos.
const EvalRepoCacheEnvVar = "CRUSH_EVAL_REPO_CACHE"

// gitCheckout clones repo and checks out ref. Remote repos materialize
// through a shared mirror cache — realrepo-* trajectories clone the
// same large repos every run, so the first run pays the network cost
// and later runs clone locally via --shared alternates. A local source
// clones directly; --shared is meaningless — and wrong to emit — for
// remote URLs.
// gitCheckout deliberately runs under the ambient environment, not
// the pinned eval env: git credentials (ssh keys, credential helpers,
// .gitconfig) live in the real HOME, and cloning is materialization —
// not part of the measured run. GIT_TERMINAL_PROMPT=0 keeps a repo
// that wants interactive auth (private URL, expired credential) a
// fast failure instead of a hang nobody can answer.
func gitCheckout(ctx context.Context, repo, ref, dest string) error {
	src := repo
	if !isLocalRepo(repo) {
		// The cache is an optimization, never a new failure mode: a
		// mirror setup error falls back to the direct remote clone.
		if mirror, err := ensureRepoMirror(ctx, repo); err == nil {
			src = mirror
		}
	}
	args := []string{"clone", "--quiet"}
	if isLocalRepo(src) {
		args = append(args, "--shared")
	}
	args = append(args, src, dest)
	if out, err := gitCmd(ctx, "", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("git clone %s: %w: %s", displayRepoURL(repo), err, out)
	}
	if src != repo {
		// A mirror clone leaves origin pointing at the local cache —
		// restore the declared URL so in-run git operations see the
		// repo the trajectory names.
		if out, err := gitCmd(ctx, dest, "remote", "set-url", "origin", repo).CombinedOutput(); err != nil {
			return fmt.Errorf("git remote set-url: %w: %s", err, out)
		}
	}
	if out, err := gitCmd(ctx, dest, "checkout", "--quiet", ref).CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout %s: %w: %s", ref, err, out)
	}
	return nil
}

// gitCmd builds a git command with terminal prompts disabled — the
// harness never has an interactive stdin, so a credential prompt must
// fail rather than hang. dir empty runs in the ambient cwd; otherwise
// it maps to git -C.
func gitCmd(ctx context.Context, dir string, args ...string) *exec.Cmd {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// ensureRepoMirror returns the path of a bare mirror of repo under the
// eval repo cache, cloning it on first use and fetching on later ones
// (a failed fetch keeps the stale mirror — offline resilience for an
// already-pinned ref). Parallel runs race on clone-to-tmp + rename:
// the loser discards its tmp dir and uses the winner's mirror.
func ensureRepoMirror(ctx context.Context, repo string) (string, error) {
	dir := os.Getenv(EvalRepoCacheEnvVar)
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(cache, "crush", "eval-repos")
	}
	key := sha256.Sum256([]byte(repo))
	mirror := filepath.Join(dir, hex.EncodeToString(key[:8])+".git")

	if _, err := os.Stat(mirror); err == nil {
		if err := gitCmd(ctx, mirror, "rev-parse", "--is-bare-repository").Run(); err == nil {
			_ = gitCmd(ctx, mirror, "fetch", "--quiet", "--prune", "origin").Run()
			return mirror, nil
		}
		if ctx.Err() != nil {
			// The check ran on a dead context — the failure says
			// nothing about the mirror. Don't rebuild on it.
			return "", ctx.Err()
		}
		// A corrupt or partially-written cache entry would serve a
		// bad path forever — remove it and rebuild below.
		_ = os.RemoveAll(mirror)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", mirror, os.Getpid())
	if out, err := gitCmd(ctx, "", "clone", "--mirror", "--quiet", repo, tmp).CombinedOutput(); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("git clone --mirror %s: %w: %s", displayRepoURL(repo), err, out)
	}
	if err := os.Rename(tmp, mirror); err != nil {
		_ = os.RemoveAll(tmp)
		if _, statErr := os.Stat(mirror); statErr == nil {
			return mirror, nil
		}
		return "", err
	}
	return mirror, nil
}

func isLocalRepo(repo string) bool {
	return strings.HasPrefix(repo, "/") || strings.HasPrefix(repo, ".") ||
		strings.HasPrefix(repo, "file://")
}

// displayRepoURL strips embedded credentials from a repo URL for error
// text — a https://user:token@host remote would otherwise print its
// token into run records.
func displayRepoURL(repo string) string {
	u, err := url.Parse(repo)
	if err != nil || u.User == nil {
		return repo
	}
	u.User = nil
	return u.String()
}

// ApplyPatch applies a patch file to a workdir (git apply; works in
// non-git dirs with --unsafe-paths off since our patches are relative).
func ApplyPatch(ctx context.Context, workdir, patchPath string) error {
	data, err := os.ReadFile(patchPath)
	if err != nil {
		return fmt.Errorf("read patch: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "apply", "--whitespace=nowarn")
	cmd.Dir = workdir
	cmd.Stdin = strings.NewReader(string(data))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git apply: %w: %s", err, out)
	}
	return nil
}

// WriteArmConfig drops the arm's generated config into the workdir.
//
// The model pin goes to .crushrc via the `model` builtin (top of the
// directory precedence order, so it overrides anything a fixture or
// cloned repo carries). Options go to .crush.json: the `option`
// builtin's key set is closed and flags without a builtin path
// (notebook_*) can only be expressed in JSON. Collision is per-key:
// a start-state shell config (.crushrc/crushrc) would shadow the JSON
// arm's same-named keys — an error. A start-state .crush.json merges
// with the arm options winning; crush.json is lower precedence than
// .crush.json outright, so it never shadows.
func WriteArmConfig(workdir string, exp *Experiment, arm Arm, manifest *FlagsManifest) error {
	for _, name := range []string{".crushrc", "crushrc"} {
		if fileExists(filepath.Join(workdir, name)) {
			return fmt.Errorf("workdir already carries %s — a shell config shadows the .crush.json arm; fixtures must not ship crush shell config", name)
		}
	}

	// .crushrc: model pin identical for every arm — options-only
	// constrains what may *differ* between arms, not what may be set.
	var rc strings.Builder
	if exp.Model != "" {
		fmt.Fprintf(&rc, "model large %s", exp.Model)
		if exp.Temperature != nil {
			fmt.Fprintf(&rc, " --temperature %v", *exp.Temperature)
		}
		rc.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(workdir, ".crushrc"), []byte(rc.String()), 0o644); err != nil {
		return fmt.Errorf("write .crushrc: %w", err)
	}

	// .crush.json: the options delta plus harness invariants (metrics
	// and provider auto-update off — identical for both arms).
	options := map[string]any{
		"disable_metrics":              true,
		"disable_provider_auto_update": true,
		// Harness state (crush.db, logs) beside the workdir, not in
		// it: check.sh sees the tree exactly as the agent left it.
		"data_directory": DataDirFor(workdir),
	}
	for k, v := range arm.Config.Options {
		// Harness invariants an arm must not override — data dir
		// relocation breaks telemetry/session-DB paths and litters
		// the tree checks observe; metrics/auto-update re-enable
		// nondeterministic side effects in measured runs.
		switch k {
		case "data_directory", "disable_metrics", "disable_provider_auto_update":
			return fmt.Errorf("arm sets %q which the harness manages — remove it from the experiment", k)
		}
		options[k] = v
	}
	doc := map[string]any{"options": options}
	// A start-state .crush.json merges under the arm: the fixture's
	// keys survive, the arm's win — same per-key rule the loader
	// applies across config layers.
	jsonPath := filepath.Join(workdir, ".crush.json")
	if fileExists(jsonPath) {
		raw, err := os.ReadFile(jsonPath)
		if err != nil {
			return fmt.Errorf("read existing .crush.json: %w", err)
		}
		var existing map[string]any
		if err := json.Unmarshal(raw, &existing); err != nil {
			// A corrupt fixture config must not be silently clobbered.
			return fmt.Errorf("existing .crush.json does not parse: %w", err)
		}
		if err := rejectFixtureProviderKeys(existing, ".crush.json"); err != nil {
			return err
		}
		if opts, ok := existing["options"].(map[string]any); ok {
			// A fixture pinning a manifest flag keys this trajectory's
			// runs under a foreign condition — every characterization
			// lands in a cell the gate never reads.
			for k := range opts {
				if _, declared := manifest.Defaults[k]; declared {
					return fmt.Errorf("existing .crush.json sets manifest flag %q — the fixture would pin a flag under test; remove it or drop the flag from flags.json", k)
				}
			}
			for k, v := range options {
				opts[k] = v
			}
			existing["options"] = opts
		} else {
			existing["options"] = options
		}
		doc = existing
	}
	// Experiment-declared providers: the child's pinned HOME hides
	// the operator's config, so a custom provider (e.g. an
	// OpenAI-compatible endpoint for a non-builtin model) must ride
	// the generated config. api_key is an env ref — credentials come
	// from the eval environment. Applied after the fixture merge so
	// a start-state .crush.json can't shadow it.
	if len(exp.Providers) > 0 {
		doc["providers"] = exp.Providers
	}
	// crush.json merges at lower precedence — still an error when it
	// pins a manifest flag (same foreign-condition trap).
	lowPath := filepath.Join(workdir, "crush.json")
	if fileExists(lowPath) {
		raw, err := os.ReadFile(lowPath)
		if err != nil {
			return fmt.Errorf("read existing crush.json: %w", err)
		}
		var existing map[string]any
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("existing crush.json does not parse: %w", err)
		}
		if err := rejectFixtureProviderKeys(existing, "crush.json"); err != nil {
			return err
		}
		if opts, ok := existing["options"].(map[string]any); ok {
			for k := range opts {
				if _, declared := manifest.Defaults[k]; declared {
					return fmt.Errorf("existing crush.json sets manifest flag %q — the fixture would pin a flag under test; remove it or drop the flag from flags.json", k)
				}
			}
		}
	}
	data, err := json.MarshalIndent(doc, "", "\t")
	if err != nil {
		return fmt.Errorf("marshal arm config: %w", err)
	}
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		return fmt.Errorf("write .crush.json: %w", err)
	}
	return nil
}

// rejectFixtureProviderKeys enforces the ownership invariant the eval
// preflight depends on: providers and credentials live on the
// experiment, never the fixture. Preflight can't see fixture config, so
// a fixture-declared provider would resolve in the child but be
// invisible to the parent check — and fixture options like
// disable_default_providers diverge the same way.
func rejectFixtureProviderKeys(existing map[string]any, name string) error {
	for _, k := range []string{"providers", "env"} {
		if _, ok := existing[k]; ok {
			return fmt.Errorf("existing %s sets %q — providers and env belong on the experiment, not the fixture", name, k)
		}
	}
	if opts, ok := existing["options"].(map[string]any); ok {
		if _, ok := opts["disable_default_providers"]; ok {
			return fmt.Errorf("existing %s sets \"disable_default_providers\" — provider visibility belongs to the experiment", name)
		}
	}
	return nil
}

// CheckRequires reports which declared environment preconditions this
// machine can't honor — the pre-flight half of `requires`. A missing
// `go` or `jq` must surface as a skip-report, not as check `error`
// outcomes masquerading as flakiness. `network` has no cheap probe;
// its constraint is enforced at load (local-path git rules), not here.
func CheckRequires(t *Trajectory) []string {
	var missing []string
	for _, tool := range append(t.Requires.Tools, t.Requires.LSP...) {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, "tool:"+tool)
		}
	}
	if len(t.Requires.OS) > 0 {
		ok := false
		for _, osname := range t.Requires.OS {
			if osname == runtime.GOOS {
				ok = true
				break
			}
		}
		if !ok {
			missing = append(missing, "os:"+runtime.GOOS)
		}
	}
	return missing
}

// BaselineConfigHash keys a baseline by the run's effective config over
// the declared flag projection — the context-management flag set
// experiments may touch. Unrelated option churn must not rotate
// baselines, and extending the projection re-keys every baseline (a
// third darkness trigger alongside re-pins and flips).
func BaselineConfigHash(projection []string, effective map[string]any) string {
	keys := make([]string, 0, len(projection))
	for _, k := range projection {
		if _, ok := effective[k]; ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		v, _ := json.Marshal(effective[k])
		fmt.Fprintf(&b, "%s=%s;", k, v)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// runShell executes a setup command in the workdir via bash under
// the pinned eval environment (nil env inherits os.Environ).
func runShell(ctx context.Context, dir, cmdLine string, env []string) error {
	cmd := exec.CommandContext(ctx, "bash", "-c", cmdLine)
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// copyTree recursively copies src into dst.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}

// DataDirFor locates the run's .crush state beside the workdir so the
// checked tree carries no harness litter.
func DataDirFor(workdir string) string {
	return filepath.Join(filepath.Dir(workdir), filepath.Base(workdir)+".crush-data")
}
