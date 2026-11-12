//go:build !windows

package shell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestProcessIsolation_SignalDoesNotReachParent verifies that a child
// sending SIGINT to its own process group does not deliver the signal to
// the parent (test) process. Without Setsid isolation, the child shares
// the parent's process group and the signal would kill the test.
func TestProcessIsolation_SignalDoesNotReachParent(t *testing.T) {
	t.Parallel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	defer signal.Stop(sigCh)

	var stderr bytes.Buffer
	err := Run(t.Context(), RunOptions{
		Command: `sh -c 'kill -INT -$$'`,
		Cwd:     t.TempDir(),
		Env:     os.Environ(),
		Stderr:  &stderr,
	})

	if err == nil {
		t.Log("child exited 0 after self-signal (unexpected but not a failure)")
	}

	select {
	case sig := <-sigCh:
		t.Fatalf("SIGINT leaked to parent process: %v (stderr=%q)", sig, stderr.String())
	case <-time.After(200 * time.Millisecond):
	}
}

// TestProcessIsolation_TTYAccessDoesNotBlock verifies that a child trying
// to read from /dev/tty does not block or suspend the parent. With Setsid,
// the child has no controlling terminal, so /dev/tty access fails immediately
// with ENXIO.
func TestProcessIsolation_TTYAccessDoesNotBlock(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{
			Command: `sh -c 'cat < /dev/tty'`,
			Cwd:     t.TempDir(),
			Env:     os.Environ(),
			Stderr:  &stderr,
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from /dev/tty access in detached session, got nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("/dev/tty access blocked for >3s; session isolation may be broken (stderr=%q)", stderr.String())
	}
}

// TestProcessIsolation_ZshJobControlDoesNotSuspend simulates the exact
// scenario that caused the original bug: running zsh with job control
// enabled. Without session isolation, zsh tries to become the foreground
// process group leader on the controlling terminal, gets SIGTTIN, and
// suspends. Skips if zsh is not installed.
func TestProcessIsolation_ZshJobControlDoesNotSuspend(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not installed")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{
			Command: `zsh -i -c 'echo ok'`,
			Cwd:     t.TempDir(),
			Env:     os.Environ(),
			Stdout:  &stdout,
			Stderr:  &stderr,
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Logf("zsh exited with error (may be expected): %v", err)
		}
		out := strings.TrimSpace(stdout.String())
		if !strings.Contains(out, "ok") {
			t.Errorf("expected 'ok' in stdout, got %q (stderr=%q)", out, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("zsh -i -c did not complete within 5s; likely suspended on TTY (stderr=%q)", stderr.String())
	}
}

// TestReapOrphans_KillsGroupAfterShellExit covers the field-observed
// orphan shape: a script backgrounds a job with `&` and exits, leaving
// the grandchild running in the dead child's process group. The pgid
// must stay tracked after Wait returns, and ReapOrphans must kill the
// surviving group — a Delete-on-Wait registry misses this case.
func TestReapOrphans_KillsGroupAfterShellExit(t *testing.T) {
	t.Setenv("CRUSH_EVAL_TELEMETRY", filepath.Join(t.TempDir(), "tel.json"))

	var stderr bytes.Buffer
	err := Run(t.Context(), RunOptions{
		// Redirecting the grandchild's fds matters: unredirected `&`
		// jobs inherit the stdout/stderr pipes, which makes Run itself
		// block on pipe EOF until they exit — the stall this registry
		// exists to prevent would hide inside the test's own Run call.
		Command: `sh -c 'sleep 60 >/dev/null 2>&1 &'`,
		Cwd:     t.TempDir(),
		Env:     os.Environ(),
		Stderr:  &stderr,
	})
	if err != nil {
		t.Fatalf("run failed: %v (stderr=%q)", err, stderr.String())
	}

	// The sh child is gone; its group lives on via `sleep 60 &`.
	var pgid int
	liveChildren.Range(func(k, _ any) bool {
		pgid, _ = k.(int)
		return false
	})
	if pgid == 0 {
		t.Fatal("no pgid tracked after shell exit — orphan group would escape reaping")
	}
	if err := syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("orphan group %d not alive before reap: %v", pgid, err)
	}

	ReapOrphans()

	// SIGKILL is not synchronous; poll briefly for the group to drain.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("orphan process group %d still alive after ReapOrphans", pgid)
}

// TestReapOrphans_NoTelemetryGate verifies reaping stays inert outside
// eval runs — an interactive user's detached jobs must survive exit.
func TestReapOrphans_NoTelemetryGate(t *testing.T) {
	t.Setenv("CRUSH_EVAL_TELEMETRY", "")

	err := Run(t.Context(), RunOptions{
		Command: `sh -c 'sleep 60 >/dev/null 2>&1 &'`,
		Cwd:     t.TempDir(),
		Env:     os.Environ(),
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	var pgid int
	liveChildren.Range(func(k, _ any) bool {
		pgid, _ = k.(int)
		return false
	})
	if pgid == 0 {
		t.Skip("orphan group already drained — nothing to check")
	}
	defer func() {
		// Clean up the test's own orphan without going through the gate.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		liveChildren.Delete(pgid)
	}()

	ReapOrphans()
	if err := syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("ReapOrphans fired without CRUSH_EVAL_TELEMETRY: %v", err)
	}
}

// TestReapOrphans_GroupDrainedCleansEntry verifies a fully-finished
// command leaves no registry entry — the entry deletes on ESRCH so a
// recycled pgid can never be targeted.
func TestReapOrphans_GroupDrainedCleansEntry(t *testing.T) {
	t.Setenv("CRUSH_EVAL_TELEMETRY", filepath.Join(t.TempDir(), "tel.json"))

	err := Run(t.Context(), RunOptions{
		Command: `sh -c 'true'`,
		Cwd:     t.TempDir(),
		Env:     os.Environ(),
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	var n int
	liveChildren.Range(func(_, _ any) bool { n++; return true })
	if n != 0 {
		t.Fatalf("%d pgid entries leaked after clean exit", n)
	}
}

// TestProcessIsolation_ChildProcessGroupKill verifies that when context
// is cancelled, the signal reaches the entire child process group (not
// just the direct child).
func TestProcessIsolation_ChildProcessGroupKill(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := Run(ctx, RunOptions{
		Command: `sh -c 'sleep 60 & wait'`,
		Cwd:     t.TempDir(),
		Env:     os.Environ(),
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	// Allow up to 4s: 500ms context timeout + 2s kill timeout + margin.
	if elapsed > 4*time.Second {
		t.Fatalf("cancellation took %v; expected prompt process group kill", elapsed)
	}
}
