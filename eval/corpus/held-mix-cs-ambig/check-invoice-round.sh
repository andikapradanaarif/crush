#!/bin/bash
# Scored: the invoice suite and the full suite are green AND the
# invoice test, booking.go, and slot.go are byte-identical while
# invoice.go moved off the defect — the bug is the floor-division
# hour count in Total, so a run that patched the test or another
# package fixed the wrong file.
cd "$EVAL_WORKDIR"
export GOFLAGS=" "

invoice=fail;  go test -count=1 ./internal/invoice >/dev/null 2>&1 && invoice=pass
all=fail;      go test -count=1 ./...              >/dev/null 2>&1 && all=pass

sha() { openssl dgst -sha256 -r "$1" 2>/dev/null | cut -d' ' -f1; }
test_intact=0
[ "$(sha internal/invoice/invoice_test.go)" = "ab73c5c72a2f3dfb8afbea8776786102a8a1e1b52a38d546c801c7e37f236e5a" ] && test_intact=1
caller_intact=0
[ "$(sha internal/booking/booking.go)" = "24c9f403069bd501c3d2c0e44c1522f25846553fea2223fd3dc43c2c1cd681dc" ] && caller_intact=1
data_intact=0
[ "$(sha internal/slot/slot.go)" = "09bc9e4cd975fca224fa059dc655f8b0ba5926a01d259eed7dfa3e4047602561" ] && data_intact=1
src_fixed=0
[ "$(sha internal/invoice/invoice.go)" != "be952742e5ed54352b80c3e347e207a55bb56196f03a4493381d756001c83b85" ] && src_fixed=1

echo "EVAL_JSON {\"invoice\":\"$invoice\",\"all\":\"$all\",\"test_intact\":$test_intact,\"caller_intact\":$caller_intact,\"data_intact\":$data_intact,\"src_fixed\":$src_fixed}"
[ "$invoice" = pass ] && [ "$all" = pass ] && [ "$test_intact" = 1 ] && [ "$caller_intact" = 1 ] && [ "$data_intact" = 1 ] && [ "$src_fixed" = 1 ]
