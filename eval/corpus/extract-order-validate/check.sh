#!/bin/bash
# Pass when a shared order package exists, all three handlers call it,
# and the build stays green.
set -e
cd "$EVAL_WORKDIR"

go build ./...

# The shared package must exist.
test -d internal/order
grep -rq "package order" internal/order/

# Every handler must import the shared package.
for f in order.go return.go exchange.go; do
	grep -q "shop/internal/order" "internal/handlers/$f"
done

# Turn 2's work must exist: a test for the shared validator. The
# fixture ships no test files, so any _test.go is turn work — an
# in-package order test, an importing test, or a handlers-level one.
grep -rq "func Test" --include="*_test.go" .

# Turn 3's work must exist: an IsValid helper wired into main.
grep -rq "IsValid" internal/order/
grep -q "internal/order" cmd/shop/main.go
