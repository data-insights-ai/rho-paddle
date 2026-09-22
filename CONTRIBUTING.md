# Contributing

## Before you change behaviour

Read the [README](README.md) and, in `rho-billing`, `docs/ARCHITECTURE.md`.
This module is an adapter: the invariants that matter — two systems of record,
unknown outcomes, provider scope, transaction boundaries — are defined there
and enforced here.

## Local setup

Go 1.26, Docker, and a checkout of `rho-billing` next to this one. The module
file replaces the billing dependency with `../rho-billing`.

```sh
export BILLING_TEST_DATABASE_URL='postgres://billing:billing-local-only@127.0.0.1:55438/billing?sslmode=disable'
sh scripts/check.sh
```

`scripts/check.sh` is the gate: race tests, vet, the signature fuzz target and
the external host module, plus the durable dispatch, recovery and inbox tests
against Postgres when `BILLING_TEST_DATABASE_URL` is set. Without it that half
skips itself, and the script says so on the first line.

Run it with a database before opening a pull request. CI runs it both ways, so
a skip is not a pass.

It unsets `PADDLE_SANDBOX_TEST` and `PADDLE_SANDBOX_KEY_FILE` and contacts no
provider.

Sandbox tests are opt-in and separate: [docs/SANDBOX.md](docs/SANDBOX.md).

## CI

`.github/workflows/check.yml` runs `scripts/check.sh` twice on every push:
once against a Postgres service, once without one. The first is the gate. The
second is there because the database-backed tests are supposed to skip
themselves rather than fail, and that is only true if something checks it.

The Postgres half used to run nightly in its own workflow while every push
reported green without it. A change that broke it looked fine until the next
morning, and four releases went out over tests nobody had run.

Both are a single checkout: `rho-billing` is a published module and resolves
from the proxy like any other dependency. No secret is needed, and neither job
contacts Paddle.

## What the review looks for

**Never retry an uncertain monetary mutation.** A timeout after a POST, or a
malformed success, means the mutation may have reached Paddle. It becomes
`ErrUncertain` and is resolved by lookup. A blind retry here charges a customer
twice, and this is the single thing reviewed hardest.

**Verify before you parse.** Signature verification runs on the raw request
bytes. Never decode, normalize or route a webhook payload before it is
trusted, and commit receipt before reporting success.

**Capability before dispatch.** Resolve whether Paddle supports an operation
before sending it. A Go method existing is not provider support. Unsupported
and unresolved both fail closed, before any HTTP request.

**Keep the wire private.** Wire types, transport and signature code stay in
`internal/`. The public API exposes core types, not Paddle JSON. A public
signature must never force a consumer to import an internal package.

**Tests drive the shipped path.** A behaviour change needs a named test that
fails before and passes after. Provider behaviour is tested with fakes for
deterministic faults and the real sandbox for provider semantics — a simulated
signed event is not evidence that a payment happened.

**Comments explain constraints, not mechanics.** Write one when the rule cannot
be read off the code. Do not narrate the next line.

**Never commit a credential**, a webhook secret, a customer payload or an
authenticated portal URL — in source, tests, fixtures, commit messages or
documentation.

## Pull requests

One concern per pull request. Say what changed and why, and paste the
`check.sh` result (say whether it ran with a database). If a sandbox scenario was involved, say which evidence
class it proves (API acceptance, captured payment, completed processing, or a
simulated webhook) rather than calling all four "passed".
