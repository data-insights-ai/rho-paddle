#!/bin/sh
# The gate: the whole suite, vet, the signature fuzz target, and the host
# module that proves a consumer can build against this adapter.
#
# Never loads Paddle credentials; sandbox tests are opt-in (docs/SANDBOX.md).
#
# The database-backed half skips itself when BILLING_TEST_DATABASE_URL is
# unset, which is convenient and was very nearly expensive: CI ran this
# without a database, reported green, and the half that was not running
# stayed broken for hours until a nightly job said so. So this says which
# half it ran, and the workflow runs it both ways.
set -eu
cd "$(dirname "$0")/.."
unset PADDLE_SANDBOX_TEST
unset PADDLE_SANDBOX_KEY_FILE

# The gate runs against the rho-billing this module pins, never against a
# checkout next door. A go.work that substitutes ../rho-billing is how you
# work on both at once, and it is also how a suite passes on your machine
# and fails in CI: you tested unreleased code, consumers get the pinned
# version. That happened, and the tests it hid were about money. So the
# workspace is switched off here and the difference is stated rather than
# discovered.
if [ -n "$(go env GOWORK)" ]; then
	echo "check: ignoring $(go env GOWORK); the gate tests the pinned rho-billing, not the checkout next door"
fi
export GOWORK=off
if [ -n "${BILLING_TEST_DATABASE_URL:-}" ]; then
	echo "check: with a database; the whole suite runs"
else
	echo "check: no BILLING_TEST_DATABASE_URL; the database-backed tests skip themselves"
fi
go test -race -count=1 -timeout 20m ./...
go vet ./...
go test ./internal/signature -run '^$' -fuzz '^FuzzVerifyRejectsMalformedInputWithoutPanic$' -fuzztime=3s -parallel=2
# The host module pins its own rho-billing, so a release that moves ours
# leaves it behind and `go test` there fails with "updates to go.mod
# needed", which says nothing about the cause. Name the cause instead.
required=$(go list -m -f '{{.Version}}' github.com/data-insights-ai/rho-billing)
pinned=$(cd testdata/host && go list -m -f '{{.Version}}' github.com/data-insights-ai/rho-billing)
if [ "$required" != "$pinned" ]; then
	echo "testdata/host pins rho-billing $pinned but this module needs $required." >&2
	echo "Run: (cd testdata/host && GOWORK=off go mod tidy) and commit the result." >&2
	exit 1
fi
(cd testdata/host && go test -count=1 ./...)
