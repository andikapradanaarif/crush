#!/bin/bash
# Pass when the rename landed everywhere: builds, tests green, old
# identifiers gone, new identifiers present.
set -e
cd "$EVAL_WORKDIR"

go build ./...
go test ./... >/dev/null

# Old names must be gone from Go sources and the YAML file.
if grep -rn "TimeoutMS\|timeout_ms" --include="*.go" --include="*.yaml" . | grep -v "^Binary"; then
	echo "old timeout_ms identifiers still present" >&2
	exit 1
fi

# New names must be present in code and config.
grep -rn "TimeoutSeconds" --include="*.go" . >/dev/null
grep -q "timeout_seconds" config.yaml

# Turn 2's work must exist: a Validate method on the config type.
grep -q "func.*Validate" internal/config/config.go
