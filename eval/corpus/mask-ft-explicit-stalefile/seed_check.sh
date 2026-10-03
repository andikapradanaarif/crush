#!/bin/bash
# Post-seed state gate for the stale-file cell: the cosmetic touch
# must have landed on main_test.go, root must still fail, and decoy
# must still fail — the seed was instructed not to fix it. The memory
# row is asserted as well: the OPEN `go test -count=1 .` row at root
# — an exact match so a stray -count=1 row can neither satisfy the
# count nor dominate MAX(last_seen) — whose last_seen must predate
# the touch's mtime; a post-touch re-run refreshes last_seen and the
# row is no longer stale even though it stays open.
cd "$EVAL_WORKDIR"
root=fail;    go test .       >/dev/null 2>&1 && root=pass
decoy=fail;   go test ./decoy >/dev/null 2>&1 && decoy=pass
touched=no;   grep -q '// touched' main_test.go && touched=yes
db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"
open_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in = '' AND cmd = 'go test -count=1 .' AND cwd IN ('.','');")
last_seen=$(sqlite3 "$db" "SELECT COALESCE(MAX(last_seen), 0) FROM failure_memory WHERE resolved_in = '' AND cmd = 'go test -count=1 .' AND cwd IN ('.','');")
mtime_s=$(stat -f %m main_test.go 2>/dev/null || stat -c %Y main_test.go 2>/dev/null)
touch_ms=$((mtime_s * 1000))
echo "EVAL_JSON {\"root\":\"$root\",\"decoy\":\"$decoy\",\"touched\":\"$touched\",\"open_stale_rows\":$open_rows,\"row_older_than_touch\":$([ \"$last_seen\" -lt \"$touch_ms\" ] && echo true || echo false)}"
[ "$root" = fail ] && [ "$decoy" = fail ] && [ "$touched" = yes ] &&
  [ "$open_rows" -ge 1 ] && [ "$last_seen" -lt "$touch_ms" ]
