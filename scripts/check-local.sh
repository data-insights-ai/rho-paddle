#!/bin/sh
# Requires a configured isolated test database; never loads Paddle credentials.
set -eu
unset PADDLE_SANDBOX_TEST
unset PADDLE_SANDBOX_KEY_FILE
: "${BILLING_TEST_DATABASE_URL:?Set BILLING_TEST_DATABASE_URL for PostgreSQL tests}"
go test -race -count=1 -timeout 20m ./...
go vet ./...
go test ./internal/signature -run '^$' -fuzz '^FuzzVerifyRejectsMalformedInputWithoutPanic$' -fuzztime=3s -parallel=2
(cd testdata/host && GOWORK=off go test -count=1 ./...)
