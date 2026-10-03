#!/bin/bash
# Post-seed state gate for the stale-memory cell: the seeded fix must
# have landed (decoy green — the recorded `go test -count=1 ./decoy`
# failure is now stale) while the task's own failure stays live at
# root. The memory row is the premise, so it is asserted too: an OPEN
# -count=1 row must survive. A disobedient seed that re-ran the
# verbatim command resolved it — a gate blind to the row would then
# score "no open failure" as "stale open failure".
cd "$EVAL_WORKDIR"
decoy=fail; go test ./decoy >/dev/null 2>&1 && decoy=pass
root=fail;  go test .       >/dev/null 2>&1 && root=pass
db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"
open_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in = '' AND cmd LIKE '%count=1%';")
echo "EVAL_JSON {\"decoy\":\"$decoy\",\"root\":\"$root\",\"open_stale_rows\":$open_rows}"
[ "$decoy" = pass ] && [ "$root" = fail ] && [ "$open_rows" -ge 1 ]
