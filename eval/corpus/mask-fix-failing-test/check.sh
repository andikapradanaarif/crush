#!/bin/bash
# Pass when the root package's tests are green — TestAdd is the
# task's referent. Scoped to `go test .` (root only): the decoy
# package's failing test is seeded wrong-referent memory and must
# stay out of the check.
set -e
cd "$EVAL_WORKDIR"
go test .
