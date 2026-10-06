#!/bin/bash
# Post-seed state gate: the seed's fail -> fix -> re-run must have
# produced the two knowledge rows this cell's arm coverage asserts
# render — a resolved failure row and the `go test` command ledger
# row — and must have fixed add.go rather than weakening the spec
# test. A disobedient seed that never reached green leaves no
# resolved row and the treatment's min_tail.sections.* starves into
# inconclusive AFTER the measured run is spent; one that resolved by
# editing add_test.go fails check.sh for the seed's sin, not the
# measured run's. Gating here turns both into a cheap pre-measure
# inconclusive — the mask cells learned this lesson at powered-run
# prices.
cd "$EVAL_WORKDIR"
db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"
resolved_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in != '';")
cmd_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM command_memory WHERE kind = 'test';")
resolved_rows=${resolved_rows:-0}
cmd_rows=${cmd_rows:-0}
spec=present
{ grep -qE 'Add\(2, *3\)' add_test.go && grep -qE '(^|[^0-9])5([^0-9]|$)' add_test.go; } || spec=absent
echo "EVAL_JSON {\"resolved_rows\":$resolved_rows,\"test_cmd_rows\":$cmd_rows,\"spec_assertion\":\"$spec\"}"
[ "$resolved_rows" -ge 1 ] && [ "$cmd_rows" -ge 1 ] && [ "$spec" = present ]
