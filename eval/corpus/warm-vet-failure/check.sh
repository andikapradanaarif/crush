#!/bin/bash
# Pass when go vet reports clean — the format-string diagnostic is
# the referent the vague prompt leaves to discovery.
set -e
cd "$EVAL_WORKDIR"
go vet ./...
