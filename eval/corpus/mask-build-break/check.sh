#!/bin/bash
# Scored: `go build ./...` — the greet.go compile error is the
# task's referent, declared here. EVAL_JSON also reports the decoy
# package's test result (the seeded memory's referent — invisible
# to the build) so wrong-target effort is on record.
cd "$EVAL_WORKDIR"
build=fail; go build ./...   >/dev/null 2>&1 && build=pass
decoy=fail; go test ./decoy  >/dev/null 2>&1 && decoy=pass
echo "EVAL_JSON {\"build\":\"$build\",\"decoy_test\":\"$decoy\"}"
[ "$build" = pass ]
