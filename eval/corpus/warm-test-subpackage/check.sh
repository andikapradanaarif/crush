#!/bin/bash
# Pass when the whole suite is green — the failure lives in one
# subpackage, not the root the prompt suggests looking at.
set -e
cd "$EVAL_WORKDIR"
go test ./...
