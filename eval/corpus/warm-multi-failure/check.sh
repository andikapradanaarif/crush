#!/bin/bash
# Pass only when BOTH checks are clean — run each so a partial fix
# still fails rather than short-circuiting on the first.
cd "$EVAL_WORKDIR"
go test ./...
t=$?
go vet ./...
v=$?
[ "$t" -eq 0 ] && [ "$v" -eq 0 ]
