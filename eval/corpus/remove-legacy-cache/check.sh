#!/bin/bash
# Pass when the cache package is gone, no file imports it, and the
# build/tests stay green.
set -e
cd "$EVAL_WORKDIR"

go build ./...
go test ./... >/dev/null

if [ -d internal/cache ]; then
	echo "internal/cache still exists" >&2
	exit 1
fi

if grep -rn "internal/cache" --include="*.go" . ; then
	echo "internal/cache still imported" >&2
	exit 1
fi

# Services must now use internal/store.
grep -q "catalog/internal/store" internal/service/catalog.go
grep -q "catalog/internal/store" internal/service/search.go

# Turn 2's work must exist: a test exercising Stats/miss counters.
grep -rln "Stats()" --include="*_test.go" .
