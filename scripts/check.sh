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
(cd testdata/host && GOWORK=off go test -count=1 ./...)
