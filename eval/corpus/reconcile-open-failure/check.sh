#!/bin/bash
# Scored: the run must have added the spec test asserting Double(3)==7.
# The suite state is diagnostic only: a model that obeys the reconcile
# retry fixes Double and turns the suite green — the mechanism
# succeeding, not a disobeyed task. Whether the row was still open at
# the boundary is asserted by the experiment's
# edge_firings.reconcile.fired coverage, not here.
cd "$EVAL_WORKDIR"
if [ ! -f double_test.go ]; then
	echo "EVAL_JSON {\"test_file\":\"missing\"}"
	exit 1
fi
if ! grep -q "Double(3)" double_test.go; then
	echo "EVAL_JSON {\"test_file\":\"present\",\"spec_assertion\":\"absent\"}"
	exit 1
fi
suite=fail; go test ./... >/dev/null 2>&1 && suite=pass
echo "EVAL_JSON {\"test_file\":\"present\",\"spec_assertion\":\"present\",\"suite\":\"$suite\"}"
