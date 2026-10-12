#!/bin/bash
# Seed gate: the ft defect must still stand after warm seeding — a
# seed session that fixed pricing.go, or mangled it into a different
# break, leaves a different question than the cell measures. Since
# seeds replay byte-identically, an ungated contamination persists
# deterministically into every measured run.
cd "$EVAL_WORKDIR"

# Same env-file defense as the checks — must be non-empty to win.
export GOFLAGS=" "

sha() { openssl dgst -sha256 -r "$1" 2>/dev/null | cut -d' ' -f1; }
defect_standing=0
[ "$(sha internal/pricing/pricing.go)" = "2cd4414e0e8e5dc4cfe74dd835d67d9e0f49d6002f618f47fbb749c5270c261a" ] &&
	! go test -count=1 ./internal/orders >/dev/null 2>&1 &&
	defect_standing=1

echo "EVAL_JSON {\"defect_standing\":$defect_standing}"
[ "$defect_standing" = 1 ]
