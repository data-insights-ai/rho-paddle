#!/bin/sh
# The gate: the whole suite, vet and the signature fuzz target, against the
# rho-billing this module pins. Never loads Paddle credentials; sandbox
# tests are opt-in (docs/SANDBOX.md).
set -eu
cd "$(dirname "$0")/.."
unset PADDLE_SANDBOX_TEST
unset PADDLE_SANDBOX_KEY_FILE
if [ -n "$(go env GOWORK)" ]; then
	echo "check: ignoring $(go env GOWORK); the gate tests the pinned rho-billing, not the checkout next door"
fi
export GOWORK=off
go test -race -count=1 ./...
go vet ./...
go test ./internal/signature -run '^$' -fuzz '^FuzzVerifyRejectsMalformedInputWithoutPanic$' -fuzztime=3s -parallel=2
