#!/bin/bash
# Pass when the tool under cmd/ runs to completion and prints the marker.
set -e
cd "$EVAL_WORKDIR"
out="$(go run ./cmd/tool 2>&1)" || exit 1
echo "$out" | grep -q "tool ok"
