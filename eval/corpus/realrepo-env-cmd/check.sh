#!/bin/bash
# Pass when `crush env` prints all eight key=value rows, the --json flag
# emits them as one JSON object, and the tree still builds. On the start
# state `env` is an unknown command — go run exits non-zero and the
# check fails.
set -e
cd "$EVAL_WORKDIR"

go build ./...

out="$(go run . env 2>&1)"
for k in goos goarch version cwd shell terminal go_version module; do
	echo "$out" | grep -q "^$k="
done

jout="$(go run . env --json 2>&1)"
echo "$jout" | grep -q '"goos"'
echo "$jout" | grep -q '"module"'
