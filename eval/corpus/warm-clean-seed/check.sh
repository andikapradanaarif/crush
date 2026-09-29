#!/bin/bash
# Pass only when -shout exists AND no-flag output is unchanged —
# hardcoding the uppercase string must fail, the flag must be wired.
# The seeds left a GREEN command history: this trajectory probes
# that failure memory stays silent (and harmless) when nothing failed.
set -e
cd "$EVAL_WORKDIR"
out="$(go run . -shout 2>&1)" || exit 1
plain="$(go run . 2>&1)" || exit 1
[ "$out" = "HELLO, WORLD" ] && [ "$plain" = "hello, world" ]
