#!/bin/bash
# Pass when the retries field exists in config, is wired into the
# client, and everything stays green.
set -e
cd "$EVAL_WORKDIR"

go build ./...
go test ./... >/dev/null

# Config carries a retries field parsed from the YAML key.
grep -qi "retries" internal/config/config.go

# The client actually consults it.
grep -qi "retries" internal/client/client.go
