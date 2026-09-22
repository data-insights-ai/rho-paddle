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
# GOWORK=off on both sides: a workspace substitutes a local checkout and
# reports no version at all, which would compare two different things.
required=$(GOWORK=off go list -m -f '{{.Version}}' github.com/data-insights-ai/rho-billing)
pinned=$(cd testdata/host && GOWORK=off go list -m -f '{{.Version}}' github.com/data-insights-ai/rho-billing)
if [ "$required" != "$pinned" ]; then
	echo "testdata/host pins rho-billing $pinned but this module needs $required." >&2
	echo "Run: (cd testdata/host && GOWORK=off go mod tidy) and commit the result." >&2
	exit 1
fi
(cd testdata/host && GOWORK=off go test -count=1 ./...)
