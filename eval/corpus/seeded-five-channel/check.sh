#!/bin/bash
# The measured fix: the whole suite passes and quota's Contract
# is the contract value — not a stub that compiles past the
# prompt, not a deleted test. tax must stay fixed: an agent that
# re-broke the sibling fails here too.
cd "$EVAL_WORKDIR"
go test ./... || exit 1
grep -q 'return 42' quota/quota.go || exit 1
