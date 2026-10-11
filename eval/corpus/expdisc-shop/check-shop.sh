#!/bin/bash
# Scored: the pristine shop builds and its whole suite is green —
# the sanity cell that keeps the shared fixture honest (a defect
# must come from the per-family overlays, not the base).
cd "$EVAL_WORKDIR"
build=pass; go build ./... >/dev/null 2>&1 || build=fail
all=fail;    go test ./...  >/dev/null 2>&1 && all=pass
echo "EVAL_JSON {\"build\":\"$build\",\"all\":\"$all\"}"
[ "$build" = pass ] && [ "$all" = pass ]
