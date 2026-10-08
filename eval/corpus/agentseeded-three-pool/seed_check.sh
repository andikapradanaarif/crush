#!/bin/bash
# Shape gate: the agent seeds must have produced the three-pool state
# this cell mirrors — a resolved row (tax fixed mid-session), an open
# row scoped to the measured task's package (quota still failing), a
# command ledger, and two seed sessions. Counts are floors, not exact
# doses — agent-authored seeds vary by construction; the scripted twin
# pins the exact-count gate.
# Also asserts the seed agent did NOT fix the target — a quota fix
# during seeding would make the measured run trivially pass.
cd "$EVAL_WORKDIR"
db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"
open_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in = '';")
open_quota=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in = '' AND cmd LIKE '%quota%';")
resolved_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in != '';")
cmd_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM command_memory;")
seed_sessions=$(sqlite3 "$db" "SELECT COUNT(*) FROM sessions;")
open_rows=${open_rows:-0}
open_quota=${open_quota:-0}
resolved_rows=${resolved_rows:-0}
cmd_rows=${cmd_rows:-0}
seed_sessions=${seed_sessions:-0}
echo "EVAL_JSON {\"open_rows\":$open_rows,\"open_quota\":$open_quota,\"resolved_rows\":$resolved_rows,\"cmd_rows\":$cmd_rows,\"seed_sessions\":$seed_sessions}"
[ "$open_quota" -ge 1 ] && [ "$resolved_rows" -ge 1 ] && [ "$cmd_rows" -ge 3 ] && [ "$seed_sessions" -eq 2 ] && grep -q 'return 41' quota/contract.go
