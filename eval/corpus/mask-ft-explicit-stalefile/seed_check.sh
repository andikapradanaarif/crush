#!/bin/bash
# Post-seed state gate for the stale-file cell: the cosmetic marker
# must have landed on notes.txt, root must still fail, and decoy must
# still fail — the seed was instructed not to fix anything. The
# memory row is asserted as well: the OPEN `go test -count=1 .` row
# at root — an exact match so a stray -count=1 row can neither
# satisfy the count nor dominate MAX(last_seen) — whose last_seen
# must predate the marker's mtime; a post-marker re-run refreshes
# last_seen and the row is no longer stale even though it stays
# open. The marker is a new file rather than an edit to main_test.go
# so the seeded agent never sees the bug — on deepseek, touching the
# failing file made the don't-fix instruction lose to the verify
# instinct every time (#238).
cd "$EVAL_WORKDIR"
root=fail;    go test .       >/dev/null 2>&1 && root=pass
decoy=fail;   go test ./decoy >/dev/null 2>&1 && decoy=pass
touched=no;   grep -qx 'touched' notes.txt 2>/dev/null && touched=yes
db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"
open_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in = '' AND cmd = 'go test -count=1 .' AND cwd IN ('.','');")
last_seen=$(sqlite3 "$db" "SELECT COALESCE(MAX(last_seen), 0) FROM failure_memory WHERE resolved_in = '' AND cmd = 'go test -count=1 .' AND cwd IN ('.','');")
mtime_s=$(stat -f %m notes.txt 2>/dev/null || stat -c %Y notes.txt 2>/dev/null)
# Empty values would make [ -lt ] fail with "integer expression
# expected" — default both sides before comparing.
open_rows=${open_rows:-0}
last_seen=${last_seen:-0}
touch_ms=$(( ${mtime_s:-0} * 1000 ))
row_older_than_touch=false
[ "$last_seen" -gt 0 ] && [ "$last_seen" -lt "$touch_ms" ] && row_older_than_touch=true
echo "EVAL_JSON {\"root\":\"$root\",\"decoy\":\"$decoy\",\"touched\":\"$touched\",\"open_stale_rows\":$open_rows,\"row_older_than_touch\":$row_older_than_touch}"
[ "$root" = fail ] && [ "$decoy" = fail ] && [ "$touched" = yes ] &&
  [ "$open_rows" -ge 1 ] && [ "$row_older_than_touch" = true ]
