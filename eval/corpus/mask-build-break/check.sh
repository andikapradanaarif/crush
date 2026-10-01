#!/bin/bash
# Pass when the whole module compiles — the compile error in
# greet.go is the task's referent. The decoy package's failing test
# is seeded failure memory, not part of the check: it compiles, so
# `go build ./...` ignores it.
set -e
cd "$EVAL_WORKDIR"
go build ./...
