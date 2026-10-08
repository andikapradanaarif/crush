#!/bin/bash
# The measured fix: badpkg builds and Broken returns the contract
# value, not a stub that compiles past the prompt.
cd "$EVAL_WORKDIR"
go build ./... || exit 1
grep -qE 'return 42' badpkg/bad.go || exit 1
