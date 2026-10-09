#!/bin/bash
# Post-seed state gate for the stale-file cell: root must still fail
# and decoy must still fail — the seed was instructed not to fix
# anything — and the OPEN `go test -count=1 .` row at root must be
# stale: last_seen predating main_test.go's mtime. The mtime IS the
# assertion — it is exactly the condition anyPathNewer checks when
# the selector weighs stale_suspect (the row implicates main_test.go
# via the failure output's file:line hint). A post-touch test re-run
# refreshes last_seen and the row is no longer stale even though it
# stays open; a disobedient fix turns root green. The touch is a
# bare `touch` — mtime only, no file content — so the seeded agent
# never sees the bug: on deepseek, exposing Add's body in the edit
# view made the don't-fix instruction lose to the verify instinct
# every time (#238).
cd "$EVAL_WORKDIR"
root=fail;    go test .       >/dev/null 2>&1 && root=pass
decoy=fail;   go test ./decoy >/dev/null 2>&1 && decoy=pass
db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"
open_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in = '' AND cmd = 'go test -count=1 .' AND cwd IN ('.','');")
last_seen=$(sqlite3 "$db" "SELECT COALESCE(MAX(last_seen), 0) FROM failure_memory WHERE resolved_in = '' AND cmd = 'go test -count=1 .' AND cwd IN ('.','');")
mtime_s=$(stat -f %m main_test.go 2>/dev/null || stat -c %Y main_test.go 2>/dev/null)
# Empty values would make [ -lt ] fail with "integer expression
# expected" — default both sides before comparing.
open_rows=${open_rows:-0}
last_seen=${last_seen:-0}
touch_ms=$(( ${mtime_s:-0} * 1000 ))
# #244: the stale premise also needs the seed to have never READ
# main_test.go — a seed that viewed the broken body saw the answer,
# and "the agent never looked" was incentive argument, not evidence.
# read_files rows are the tracker's record of view/read calls.
seed_peeks=$(sqlite3 "$db" "SELECT COUNT(*) FROM read_files WHERE path LIKE '%main_test.go';")
seed_peeks=${seed_peeks:-0}
row_older_than_touch=false
[ "$last_seen" -gt 0 ] && [ "$last_seen" -lt "$touch_ms" ] && row_older_than_touch=true
echo "EVAL_JSON {\"root\":\"$root\",\"decoy\":\"$decoy\",\"open_stale_rows\":$open_rows,\"row_older_than_touch\":$row_older_than_touch,\"seed_peeks\":$seed_peeks}"
[ "$root" = fail ] && [ "$decoy" = fail ] &&
  [ "$open_rows" -ge 1 ] && [ "$row_older_than_touch" = true ] && [ "$seed_peeks" -eq 0 ]
