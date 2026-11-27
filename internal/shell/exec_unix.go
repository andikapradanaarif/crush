//go:build !windows

package shell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// isolateProcess sets SysProcAttr so the child runs in its own session,
// fully detached from Crush's controlling terminal. This prevents shells
// like zsh from grabbing the TTY for job control, which would cause
// "suspended (tty input)" and corrupt Bubble Tea's terminal rendering.
// It also prevents child processes from sending signals to Crush's process
// group.
func isolateProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// liveChildren tracks every spawned child pid (== its process group id)
// so survivors can be reaped at process exit. Script-internal `&` jobs
// stay in the child's group and escape ctx cancellation — including the
// common orphan shape where the shell itself exits while its `&`
// grandchildren keep running — so the entry is held while the GROUP is
// alive, not just while the direct child is.
var liveChildren sync.Map // pgid (int) → struct{}

// ReapOrphans kills each tracked child's process group. Harness runs
// only — gated on CRUSH_EVAL_TELEMETRY, since an interactive session's
// deliberately detached jobs are the user's own and should survive exit.
func ReapOrphans() {
	if os.Getenv("CRUSH_EVAL_TELEMETRY") == "" {
		return
	}
	liveChildren.Range(func(k, _ any) bool {
		if pid, ok := k.(int); ok {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		liveChildren.Delete(k)
		return true
	})
}

// processGroupExecHandler returns an ExecHandlerFunc that replaces
// interp.DefaultExecHandler with one that fully isolates child processes
// from Crush's session and controlling terminal.
//
// Without this, shells like zsh that enable job control when sourcing
// framework files will attempt to take over the TTY, causing SIGTTIN/SIGTTOU
// signals and corrupting the parent terminal state.
//
// The implementation mirrors interp.DefaultExecHandler with two additions:
// isolateProcess(&cmd) after construction, and negative-PID signal targeting
// in the cancellation callback so the entire child process group is killed.
func processGroupExecHandler(killTimeout time.Duration) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		hc := interp.HandlerCtx(ctx)
		path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
		if err != nil {
			fmt.Fprintln(hc.Stderr, err)
			return interp.ExitStatus(127)
		}

		cmd := exec.Cmd{
			Path:   path,
			Args:   args,
			Env:    execEnvList(hc.Env),
			Dir:    hc.Dir,
			Stdin:  hc.Stdin,
			Stdout: hc.Stdout,
			Stderr: hc.Stderr,
		}
		isolateProcess(&cmd)

		// Bound how long Wait blocks after the process exits: a detached
		// descendant holding a stdio pipe open would otherwise hang us
		// forever. It is killTimeout + 1s rather than mvdan's bare
		// killTimeout, so on cancel the SIGKILL escalation below normally
		// closes the pipes before it expires.
		cmd.WaitDelay = killTimeout + time.Second

		err = cmd.Start()
		if err == nil {
			liveChildren.Store(cmd.Process.Pid, struct{}{})
			group := -cmd.Process.Pid
			leaderDone := make(chan struct{})
			cleanupDone := make(chan struct{})
			stop := context.AfterFunc(ctx, func() {
				defer close(cleanupDone)
				if killTimeout <= 0 {
					_ = syscall.Kill(group, syscall.SIGKILL)
					return
				}

				if syscall.Kill(group, syscall.SIGINT) == syscall.ESRCH {
					return
				}
				timer := time.NewTimer(killTimeout)
				defer timer.Stop()
				select {
				case <-leaderDone:
					if syscall.Kill(group, 0) == syscall.ESRCH {
						return
					}
					<-timer.C
				case <-timer.C:
				}
				_ = syscall.Kill(group, syscall.SIGKILL)
			})

			err = cmd.Wait()
			close(leaderDone)
			// The leader can exit on SIGINT while descendants are still
			// alive. Keep the shell tracked until escalation has run.
			if !stop() {
				<-cleanupDone
			}
			// The child exited, but its process group may still host
			// &-detached grandchildren — the exact orphan shape
			// ReapOrphans exists for. Keep the pgid tracked while the
			// group is alive (kill(0) succeeds or EPERMs); delete only
			// on ESRCH so the entry can't later resolve to a recycled
			// pgid.
			if err := syscall.Kill(group, 0); errors.Is(err, syscall.ESRCH) {
				liveChildren.Delete(cmd.Process.Pid)
			}
		}

		return exitStatusFromError(ctx, hc.Stderr, err)
	}
}
