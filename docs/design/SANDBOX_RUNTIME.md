# Sandbox Runtime — Containment for Agent Execution

> **Status:** Parked — accepted design, held pending the #90 benefit
> measurement. When it resumes: phase D (workspace-root confinement)
> + phase A (OS sandbox on exec) first, phase B (whole-process
> sandbox) as the headless improvement. Container runtimes rejected —
> see Non-goals.
> **Ship when:** per phase; each phase is independently useful.
> **Measured by:** sandbox-deny and escalation-grant counters in
> `crush stats` + `edge_firings`-style telemetry per session.

## Goal

Bound what the agent can do *after* a permission grant — today a
single "allow" gives a command full user privileges: filesystem,
network, credentials. As autonomy grows (bounded retries,
background agents, unattended `crush run`), the permission prompt
stops being the containment story. The sandbox changes the
*default*, not the ceiling: `full-access` remains a mode, so
machine-level tasks ("install this", "edit ~/.zshrc") keep working.

## Threat model

- **Primary:** agent mistakes — `rm -rf` on the wrong path, a test
  writing outside the workspace, `curl | sh` from a sketchy README.
- **Secondary:** prompt injection via fetched content driving
  destructive or exfiltrating shell commands.
- **Explicitly not the target:** a malicious model. A determined
  frontier model inside workspace-write still has room to maneuver;
  this bounds accidents and drive-bys, not adversaries.

## Modes

Borrowing the vocabulary users already know (Codex):

| Mode | Reads | Writes | Network (sandboxed cmds) |
|---|---|---|---|
| `read-only` | anywhere | none | denied |
| `workspace-write` | anywhere | workspace root only | denied except allowlist |
| `full-access` | anywhere | anywhere | open — today's behavior |

`sandbox_mode` option (`optionSpec`, `shellconfig/options.go`) +
`options.sandbox.mode` in JSON. **Default: `full-access`** — an
explicit opt-down posture, not a silent behavior change; the flag
is the evidence-gated flip like the rest of the series.

## Phase D — workspace-root confinement (ships first)

The file tools write in-process, so an exec sandbox alone leaves
the most common accident open. Phase D is a permission-layer rule:
with `sandbox_mode != full-access`, file-mutating tools refuse
paths outside the workspace root. Bash is unrestricted in this
phase — it narrows the *file-write* blast radius before any OS
machinery lands. Cheap, no new dependencies, independently useful.

## Phase A — OS sandbox on the exec seam

The seam already exists: `internal/shell` wraps
`interp.DefaultExecHandler` and `isolateProcess` (`exec_unix.go`)
already sets `SysProcAttr`. The sandbox is another layer on the
same wrap:

- **macOS:** `sandbox-exec` + a Seatbelt profile — text policy:
  deny write outside workspace root, deny network per the table
  above.
- **Linux:** `bubblewrap` (`bwrap`) — bind-mount workspace rw,
  read-only root elsewhere, `--unshare-net` for the deny case.
- **Windows:** degrades to permission-only in v1 — real sandboxing
  there (restricted tokens / Windows Sandbox) is its own project;
  the mode surfaces as "unavailable" rather than silently off.

**Network policy:** deny by default for sandboxed commands, with a
built-in command allowlist — `git`, `npm`/`pnpm`/`yarn`, `brew`,
`pip`/`uv`, `go` toolchain commands keep network (the 95%
legitimate case); `curl`/`wget` stay denied (the exfiltration
shape). Everything else escalates — next section. Deliberately
not a host allowlist: per-host rules are fiddly and don't survive
"download from this one weird mirror" reality.

## Escalation — deny feeds permission, one system

A sandbox denial is not a dead end. The denied command converts
into a permission request — "needs `full-access`: `brew install
x`" — and an approve re-runs it elevated (or the session flips
mode, per existing `GrantPersistent`). This is the load-bearing
decision: sandbox + permission are **one system**, not two
parallel gates — the sandbox is the default posture, the
permission layer is the override path. It also means the
telemetry is honest: deny count and grant count are both
measurable, and a high grant rate is itself the signal that the
policy is wrong, not that the sandbox works.

## MCP / LSP scope — per-server boundary

MCP servers and LSP processes get a per-server `sandbox` field
(default `false` = trusted/outside — backward compatible, and
user-configured tooling like git-aware MCPs needs host access).
The untrusted surface is agent-generated *shell commands*;
declared tooling is already a user-trust decision. Per-server
sandboxing is the config surface for the MCP that fetches
untrusted content or shells out itself.

## Phase B — whole-process sandbox (headless improvement)

`crush run` and CI: launch the *crush process itself* under
`sandbox-exec`/`bwrap`, covering file tools and in-process writes
without per-call policy. The unattended case is where containment
pays most — an overnight chain has no human at the permission
prompt. Config/secrets reads and provider API access need
explicit allowlist entries; MCP/LSPs either join the sandbox
per their flag or must be spawned/trusted outside.

## Non-goals

- **No container-per-task runtime.** A Docker dependency inverts
  what Crush is — a fast local CLI. Users who want container
  isolation run *crush itself* in a devcontainer; that's the
  pattern, not containers per command.
- **No VM/microVM.** Devin's model; wrong weight class for a CLI.
- **No adversarial-model containment** — see threat model.
- **No Windows sandbox in v1** — degrades honestly, doesn't pretend.

## Measurement

- `sandbox_denied` / `sandbox_escalated` / `escalation_granted`
  counters per session, surfaced in `crush stats` — a high grant
  rate means the policy (allowlist, mode default) is wrong.
- Eval arm (EVAL_HARNESS): same task, `workspace-write` vs
  `full-access` — escalation-count and completion-rate delta is
  the evidence for whether the default can ever flip.
