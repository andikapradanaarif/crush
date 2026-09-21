#!/bin/bash
# Pass when the staged build-out landed: all stages/types exist and
# the tree builds, vets, and tests clean.
set -e
cd "$EVAL_WORKDIR"

go build ./...
go vet ./...
go test ./... >/dev/null

# Each turn's deliverable — path-agnostic greps over the module.
grep -rq "Dedupe\|dedupe" internal/
grep -rq "MinLen" internal/
grep -rq "func (p \*Pipeline) Stages\|func.*Stages() \[\]string" internal/
grep -rq "func Sample" internal/
grep -rq "type Report" internal/
grep -rq "Summary()" internal/
grep -rq "Errors" internal/
# The stem-lite stage strips punctuation — some signal of it.
grep -rEq "Trim|punct|unicode" internal/
# main uses Sample for the no-args path and reads -config.
grep -q "Sample" main.go
grep -q "config" main.go
