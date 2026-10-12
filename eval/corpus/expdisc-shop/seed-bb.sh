#!/bin/bash
# Seed gate: the bb defect must still stand after warm seeding — a
# seed session that fixed report_test.go, or broke it differently,
# leaves a different question than the cell measures. Since seeds
# replay byte-identically, an ungated contamination persists
# deterministically into every measured run.
cd "$EVAL_WORKDIR"

# Same env-file defense as the checks — must be non-empty to win.
export GOFLAGS=" "

sha() { openssl dgst -sha256 -r "$1" 2>/dev/null | cut -d' ' -f1; }
defect_standing=0
[ "$(sha internal/reports/report_test.go)" = "97b94bf688b2a0cfd6184f73e92a82380615ec800cbebc519006dca0fa0e46f3" ] &&
	! go test -count=1 ./internal/reports >/dev/null 2>&1 &&
	defect_standing=1

echo "EVAL_JSON {\"defect_standing\":$defect_standing}"
[ "$defect_standing" = 1 ]
