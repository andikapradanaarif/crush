#!/bin/bash
# Scored: the pristine shop builds and its whole suite is green —
# the sanity cell that keeps the shared fixture honest (a defect
# must come from the per-family overlays, not the base).
cd "$EVAL_WORKDIR"

# An agent's `go env -w` writes into the pinned HOME's env file and
# would otherwise leak flags into these invocations — a GOFLAGS=-count=0
# (or -run=^$) seed turns every go test into a vacuous "ok [no tests
# to run]". The override must be non-empty: an empty GOFLAGS reads as
# unset and the env file wins.
export GOFLAGS=" "

build=pass; go build ./... >/dev/null 2>&1 || build=fail
all=fail;    go test -count=1 ./...  >/dev/null 2>&1 && all=pass
echo "EVAL_JSON {\"build\":\"$build\",\"all\":\"$all\"}"
[ "$build" = pass ] && [ "$all" = pass ]
