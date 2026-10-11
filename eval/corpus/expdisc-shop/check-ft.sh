#!/bin/bash
# Scored: the orders suite and the full suite are green AND the two
# orders files are untouched — the defect lives in pricing, so a
# run that patched the test or the innocent caller fixed the wrong
# file. EVAL_JSON reports the whole surface so wrong-target fixes
# read as scope, not silence.
cd "$EVAL_WORKDIR"

orders=fail;  go test ./internal/orders >/dev/null 2>&1 && orders=pass
all=fail;     go test ./...            >/dev/null 2>&1 && all=pass

sha() { openssl dgst -sha256 -r "$1" 2>/dev/null | cut -d' ' -f1; }
test_intact=0
[ "$(sha internal/orders/orders_test.go)" = "3fd10d0a174a43beae898de97ff812411a4944bd0c49dc0b49fa1fa9c91cc541" ] && test_intact=1
caller_intact=0
[ "$(sha internal/orders/orders.go)" = "4782f2aba8d8bf44034c9d986e2782ebb7737aa8961d816087b2dd45d4d726d0" ] && caller_intact=1

echo "EVAL_JSON {\"orders\":\"$orders\",\"all\":\"$all\",\"test_intact\":$test_intact,\"caller_intact\":$caller_intact}"
[ "$orders" = pass ] && [ "$all" = pass ] && [ "$test_intact" = 1 ] && [ "$caller_intact" = 1 ]
