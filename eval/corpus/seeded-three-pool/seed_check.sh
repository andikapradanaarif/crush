#!/bin/bash
# Dose gate: the scripted seeds must have produced exactly the
# three-pool state this trajectory's LOO arms ablate over —
# one resolved failure, one open failure, and the command rows
# both sessions recorded. A seed that diverged (network flake in
# 'go test', mvdan parse rejection) starves a pool and the measured
# run would answer a different question than the cell poses.
cd "$EVAL_WORKDIR"
db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"
resolved_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in != '';")
open_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in = '';")
cmd_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM command_memory;")
seed_sessions=$(sqlite3 "$db" "SELECT COUNT(*) FROM sessions WHERE title LIKE 'seed %';")
resolved_rows=${resolved_rows:-0}
open_rows=${open_rows:-0}
cmd_rows=${cmd_rows:-0}
seed_sessions=${seed_sessions:-0}
echo "EVAL_JSON {\"resolved_rows\":$resolved_rows,\"open_rows\":$open_rows,\"cmd_rows\":$cmd_rows,\"seed_sessions\":$seed_sessions}"
[ "$resolved_rows" -eq 1 ] && [ "$open_rows" -eq 1 ] && [ "$cmd_rows" -ge 4 ] && [ "$seed_sessions" -eq 2 ]
