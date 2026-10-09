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
# #244: the seed legitimately views decoy/decoy.go (it edits it), so
# the peek premise is path-scoped, not a global count — a seed that
# opened greet.go saw the build break's body before measurement.
seed_peeks=$(sqlite3 "$db" "SELECT COUNT(*) FROM read_files WHERE path LIKE '%greet.go' OR path LIKE '%main.go';")
seed_peeks=${seed_peeks:-0}
echo "EVAL_JSON {\"decoy\":\"$decoy\",\"build\":\"$build\",\"open_stale_rows\":$open_rows,\"seed_peeks\":$seed_peeks}"
[ "$decoy" = pass ] && [ "$build" = fail ] && [ "$open_rows" -ge 1 ] && [ "$seed_peeks" -eq 0 ]
