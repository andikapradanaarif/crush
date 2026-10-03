#!/bin/bash
# Post-seed state gate for the stale-memory cell: the seeded fix must
# have landed (decoy test green — the recorded `go test -count=1
# ./decoy` failure is now stale) while the task's own failure, the
# greet.go compile error, stays live. The memory row is the premise,
# so the DESIGNED row itself is asserted: the open `go test -count=1
# ./decoy` row at root — not any -count=1 row, which a disobedient
# seed could leave behind while having resolved the decoy row.
cd "$EVAL_WORKDIR"
decoy=fail; go test ./decoy  >/dev/null 2>&1 && decoy=pass
build=fail; go build ./...  >/dev/null 2>&1 && build=pass
db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"
open_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in = '' AND cmd = 'go test -count=1 ./decoy' AND cwd IN ('.','');")
echo "EVAL_JSON {\"decoy\":\"$decoy\",\"build\":\"$build\",\"open_stale_rows\":$open_rows}"
[ "$decoy" = pass ] && [ "$build" = fail ] && [ "$open_rows" -ge 1 ]
