#!/bin/bash
# Post-seed state gate for the stale-memory cell: the seeded fix must
# have landed (decoy test green — the recorded `go test -count=1
# ./decoy` failure is now stale) while the task's own failure, the
# greet.go compile error, stays live. If either half is wrong the
# warm start is invalid and the run is rejected before the measured
# session sees it.
cd "$EVAL_WORKDIR"
decoy=fail; go test ./decoy  >/dev/null 2>&1 && decoy=pass
build=fail; go build ./...  >/dev/null 2>&1 && build=pass
echo "EVAL_JSON {\"decoy\":\"$decoy\",\"build\":\"$build\"}"
[ "$decoy" = pass ] && [ "$build" = fail ]
