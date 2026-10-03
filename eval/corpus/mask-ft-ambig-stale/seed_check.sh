#!/bin/bash
# Post-seed state gate for the stale-memory cell: the seeded fix must
# have landed (decoy green — the recorded `go test -count=1 ./decoy`
# failure is now stale) while the task's own failure stays live at
# root. The memory row is the premise, so the DESIGNED row itself is
# asserted: the open `go test -count=1 ./decoy` row at root — not any
# -count=1 row. A disobedient seed that resolved the decoy row and
# left a different open -count=1 row (e.g. re-running the still-red
# task) is the corruption this gate exists to reject.
cd "$EVAL_WORKDIR"
decoy=fail; go test ./decoy >/dev/null 2>&1 && decoy=pass
root=fail;  go test .       >/dev/null 2>&1 && root=pass
db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"
open_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in = '' AND cmd = 'go test -count=1 ./decoy' AND cwd IN ('.','');")
echo "EVAL_JSON {\"decoy\":\"$decoy\",\"root\":\"$root\",\"open_stale_rows\":$open_rows}"
[ "$decoy" = pass ] && [ "$root" = fail ] && [ "$open_rows" -ge 1 ]
