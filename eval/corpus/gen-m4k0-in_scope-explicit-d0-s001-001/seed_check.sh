#!/bin/bash
# Dose gate: the scripted seeds must have produced exactly the
# three-pool state this cell was authored around.
cd "$EVAL_WORKDIR"
db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"
open_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in = '';")
resolved_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in != '';")
cmd_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM command_memory;")
seed_sessions=$(sqlite3 "$db" "SELECT COUNT(*) FROM sessions WHERE title LIKE 'seed %';")
open_rows=${open_rows:-0}
resolved_rows=${resolved_rows:-0}
cmd_rows=${cmd_rows:-0}
seed_sessions=${seed_sessions:-0}
echo "EVAL_JSON {\"open_rows\":$open_rows,\"resolved_rows\":$resolved_rows,\"cmd_rows\":$cmd_rows,\"seed_sessions\":$seed_sessions}"
[ "$open_rows" -eq 2 ] && [ "$resolved_rows" -eq 1 ] && [ "$cmd_rows" -ge 5 ] && [ "$seed_sessions" -eq 4 ]
