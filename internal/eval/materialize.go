package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Materialize builds a fresh workdir for a run inside parentDir and
// returns its path. Every run gets a new tempdir — never the corpus or
// the real repo.
func Materialize(ctx context.Context, traj *Trajectory, trajDir, parentDir string) (string, error) {
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
		if err := runShell(ctx, workdir, cmdLine); err != nil {
			return "", fmt.Errorf("setup %q: %w", cmdLine, err)
		}
	}
	return workdir, nil
}

// gitCheckout clones repo and checks out ref. A local source may use
// --shared (objects shared via alternates); --shared is meaningless —
// and wrong to emit — for remote URLs.
func gitCheckout(ctx context.Context, repo, ref, dest string) error {
	args := []string{"clone", "--quiet"}
	if isLocalRepo(repo) {
		args = append(args, "--shared")
	}
	args = append(args, repo, dest)
	if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("git clone %s: %w: %s", repo, err, out)
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", dest, "checkout", "--quiet", ref).CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout %s: %w: %s", ref, err, out)
	}
	return nil
}

func isLocalRepo(repo string) bool {
	return strings.HasPrefix(repo, "/") || strings.HasPrefix(repo, ".") ||
		strings.HasPrefix(repo, "file://")
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
// (notebook_*) can only be expressed in JSON. Per-key merge means a
// start-state shell config would shadow same-named JSON keys — so a
// materialized workdir that already carries crush config is a
// validation error, not a silent override.
func WriteArmConfig(workdir string, exp *Experiment, arm Arm) error {
	for _, name := range []string{".crushrc", "crushrc", ".crush.json", "crush.json"} {
		if fileExists(filepath.Join(workdir, name)) {
			return fmt.Errorf("workdir already carries %s — arm config would collide; fixtures must not ship crush config", name)
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
	}
	for k, v := range arm.Config.Options {
		options[k] = v
	}
	doc := map[string]any{"options": options}
	data, err := json.MarshalIndent(doc, "", "\t")
	if err != nil {
		return fmt.Errorf("marshal arm config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(workdir, ".crush.json"), data, 0o644); err != nil {
		return fmt.Errorf("write .crush.json: %w", err)
	}
	return nil
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

// runShell executes a setup command in the workdir via bash.
func runShell(ctx context.Context, dir, cmdLine string) error {
	cmd := exec.CommandContext(ctx, "bash", "-c", cmdLine)
	cmd.Dir = dir
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
