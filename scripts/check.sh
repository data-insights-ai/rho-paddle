#!/bin/sh
# Unit gate: no database, no Paddle credentials, no network.
# Database-backed tests skip themselves when BILLING_TEST_DATABASE_URL is unset,
# so this is the whole suite minus its integration half. Run
# scripts/check-local.sh before opening a pull request.
set -eu
cd "$(dirname "$0")/.."
unset BILLING_TEST_DATABASE_URL
unset PADDLE_SANDBOX_TEST
unset PADDLE_SANDBOX_KEY_FILE
go test -race -count=1 -timeout 10m ./...
go vet ./...
go test ./internal/signature -run '^$' -fuzz '^FuzzVerifyRejectsMalformedInputWithoutPanic$' -fuzztime=3s -parallel=2
# The host module proves a consumer can build against this adapter. It
# pins its own rho-billing, so a release that moves ours leaves it behind
# and `go test` there fails with "updates to go.mod needed", which says
# nothing about the cause. Name the cause instead.
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
