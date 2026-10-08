#!/bin/bash
# The measured fix: the whole suite passes and the target's
# Contract is the contract value — not a stub that compiles past
# the prompt, not a deleted test.
cd "$EVAL_WORKDIR"
go test ./... || exit 1
grep -q 'return 42' quota/contract.go || exit 1
