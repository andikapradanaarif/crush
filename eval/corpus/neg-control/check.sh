#!/bin/bash
# Pass only when -shout exists AND no-flag output is unchanged —
# the run's conclusive outcome doesn't matter to the gate's noop
# alarm (it reads ResolvedOptions, not the verdict), but a real
# task keeps the record conclusive and cheap.
set -e
cd "$EVAL_WORKDIR"
out="$(go run . -shout 2>&1)" || exit 1
plain="$(go run . 2>&1)" || exit 1
[ "$out" = "HELLO, WORLD" ] && [ "$plain" = "hello, world" ]
