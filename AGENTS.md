# rho-paddle

Contributor notes for the Paddle plugin.

- Go module: `github.com/data-insights-ai/rho-paddle`; depends on rho-billing
  (`provider`, `subscription`) and nothing else outside the standard library.
- Stateless: no storage, workers or retries. Constructors do no I/O.
- Keep credentials, webhook secrets and customer payloads out of source,
  fixtures and logs.
- Verify raw webhook bytes before parsing; never retry an uncertain
  mutation blindly.
- `sh scripts/check.sh` is the gate; it unsets sandbox credentials and
  exports `GOWORK=off`, so it tests the rho-billing `go.mod` pins, not a
  checkout next door. Depending on an unreleased rho-billing change means
  releasing it first.

## Hosts

| Host | Pinned | Notes |
|---|---|---|
| account-platform | v0.5+ | Paddle source of subscriptions, credit purchases and overage |
| sigma-scg-platform | v0.4.1 | uses the removed storage-backed adapter; stays pinned until migrated |
