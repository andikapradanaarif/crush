#!/bin/bash
# Dose gate: one open narrow failure row (stamp suite) plus passing
# command rows — the same lone-candidate shape as the en sibling.
cd "$EVAL_WORKDIR"
db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"
open_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in = '';")
cmd_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM command_memory;")
seed_sessions=$(sqlite3 "$db" "SELECT COUNT(*) FROM sessions;")
open_rows=${open_rows:-0}
cmd_rows=${cmd_rows:-0}
seed_sessions=${seed_sessions:-0}
echo "EVAL_JSON {\"open_rows\":$open_rows,\"cmd_rows\":$cmd_rows,\"seed_sessions\":$seed_sessions}"
[ "$open_rows" -eq 1 ] && [ "$cmd_rows" -ge 3 ] && [ "$seed_sessions" -eq 2 ]
