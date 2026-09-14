#!/bin/bash
# Pass when a shared order package exists, all three handlers call it,
# and the build stays green.
set -e
cd "$EVAL_WORKDIR"

go build ./...
go vet ./...

# The shared package must exist.
test -d internal/order
grep -rq "package order" internal/order/

# Every handler must import the shared package.
for f in order.go return.go exchange.go; do
	grep -q "shop/internal/order" "internal/handlers/$f"
done

# Turn 2's work must exist: a test for the shared validator.
grep -rq "func Test" internal/order/
