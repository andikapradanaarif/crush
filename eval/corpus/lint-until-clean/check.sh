#!/bin/bash
# Pass when lint.sh reports clean and the build/tests stay green.
set -e
cd "$EVAL_WORKDIR"

go build ./...
go test ./... >/dev/null

bash ./lint.sh
