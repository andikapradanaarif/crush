#!/bin/bash
# Pass when lint.sh reports clean and the build/tests stay green.
set -e
cd "$EVAL_WORKDIR"

go build ./...
go test ./... >/dev/null

# Turn 2's lint rule must exist — env access belongs to config.
grep -q "os.Getenv" lint.sh

bash ./lint.sh
