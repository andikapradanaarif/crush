#!/bin/bash
# Pass when the whole module compiles — the single compile error is
# the referent the vague prompt leaves to discovery.
set -e
cd "$EVAL_WORKDIR"
go build ./...
