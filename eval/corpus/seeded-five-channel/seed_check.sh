#!/bin/bash
# Dose gate: the scripted seeds must have produced the five-channel
# state this cell's LOO arms ablate over — one open failure, one
# resolved failure, the command rows, a promoted referent mapping
# (two clean accepted episodes in distinct sessions), digest file
# hints, and materialized session digests. A seed that diverged
# starves a channel and the measured run would answer a different
# question than the cell poses.
cd "$EVAL_WORKDIR"
db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"
open_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in = '';")
resolved_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in != '';")
cmd_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM command_memory;")
referent_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM referent_memory;")
referent_eps=$(sqlite3 "$db" "SELECT COUNT(*) FROM referent_episodes;")
read_files=$(sqlite3 "$db" "SELECT COUNT(*) FROM read_files;")
digest_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM session_digests;")
seed_sessions=$(sqlite3 "$db" "SELECT COUNT(*) FROM sessions;")
open_rows=${open_rows:-0}
resolved_rows=${resolved_rows:-0}
cmd_rows=${cmd_rows:-0}
referent_rows=${referent_rows:-0}
referent_eps=${referent_eps:-0}
read_files=${read_files:-0}
digest_rows=${digest_rows:-0}
seed_sessions=${seed_sessions:-0}
echo "EVAL_JSON {\"open_rows\":$open_rows,\"resolved_rows\":$resolved_rows,\"cmd_rows\":$cmd_rows,\"referent_rows\":$referent_rows,\"referent_eps\":$referent_eps,\"read_files\":$read_files,\"digest_rows\":$digest_rows,\"seed_sessions\":$seed_sessions}"
[ "$open_rows" -eq 1 ] && [ "$resolved_rows" -eq 1 ] && [ "$cmd_rows" -ge 5 ] && \
  [ "$referent_rows" -eq 1 ] && [ "$referent_eps" -eq 2 ] && \
  [ "$read_files" -eq 4 ] && [ "$digest_rows" -eq 4 ] && [ "$seed_sessions" -eq 4 ]
