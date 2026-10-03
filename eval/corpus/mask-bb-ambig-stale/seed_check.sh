#!/bin/bash
# Post-seed state gate for the stale-memory cell: the seeded fix must
# have landed (decoy test green — the recorded `go test -count=1
# ./decoy` failure is now stale) while the task's own failure, the
# greet.go compile error, stays live. The memory row is the premise,
# so it is asserted too: an OPEN -count=1 row must survive — a
# disobedient seed that re-ran the verbatim command resolved it, and
# a gate blind to the row would score "no open failure" as "stale
# open failure".
cd "$EVAL_WORKDIR"
decoy=fail; go test ./decoy  >/dev/null 2>&1 && decoy=pass
build=fail; go build ./...  >/dev/null 2>&1 && build=pass
db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"
open_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in = '' AND cmd LIKE '%count=1%';")
echo "EVAL_JSON {\"decoy\":\"$decoy\",\"build\":\"$build\",\"open_stale_rows\":$open_rows}"
[ "$decoy" = pass ] && [ "$build" = fail ] && [ "$open_rows" -ge 1 ]
