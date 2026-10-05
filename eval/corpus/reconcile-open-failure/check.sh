#!/bin/bash
# Scored: the run must have added the intentionally-failing spec test
# and left it — the suite stays red by design, which is exactly the
# reconcile edge's observed-open premise. A green suite means the
# model disobeyed the "do not implement" guard; a missing test file
# means it never did the work.
cd "$EVAL_WORKDIR"
if [ ! -f double_test.go ]; then
	echo "EVAL_JSON {\"test_file\":\"missing\"}"
	exit 1
fi
suite=fail; go test ./... >/dev/null 2>&1 && suite=pass
echo "EVAL_JSON {\"test_file\":\"present\",\"suite\":\"$suite\"}"
[ "$suite" = fail ]
