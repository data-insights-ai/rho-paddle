# rho-paddle

The Paddle Billing plugin of [rho-billing](https://github.com/data-insights-ai/rho-billing)'s
`provider` interface. Stateless: it talks to Paddle and verifies its
webhooks; the host keeps every record and decides what an event means.

Module: `github.com/data-insights-ai/rho-paddle`. License: Apache 2.0.

| Call | What it does |
|---|---|
| `New` | Sandbox or live, API key, webhook secrets. No network. |
| `Checkout` | A transaction for catalog prices with the host's reference; answers Paddle's checkout URL. |
| `CustomerPortal` | A link to Paddle's self-service pages for a customer. |
| `Catalog` | Every product with its prices (active and archived), with the host's references from custom data. |
| `Charge` | A one-time price billed with a subscription's next renewal. |
| `Webhooks` | An `http.Handler`: verifies `Paddle-Signature` on the raw body (5-minute window), decodes `subscription.*` and `transaction.completed`, and calls the host. |
| `Verify`, `Decode` | The same, for hosts with their own handler. |

The host's reference travels as custom data `reference` on transactions,
subscriptions, products and prices.

## Webhooks

Paddle delivers at least once, late and out of order. The handler answers
200 when the host's function succeeded or the event kind is not one the
host needs, 401 for a bad signature and 500 when the host failed, which
Paddle retries. The host treats an event it has seen as success and keeps
subscriptions with `subscription.Apply`, which ignores stale snapshots.

## Uncertain outcomes

`ErrUncertain` means a mutation may have reached Paddle. Paddle has no
idempotency keys: record a charge before asking for it, and reconcile an
uncertain one instead of repeating it. `ErrRateLimited` carries Paddle's
delay; the client never waits or retries by itself.

## Checks

`sh scripts/check.sh` runs the suite against the pinned rho-billing with no
credentials. Sandbox tests are opt-in: `docs/SANDBOX.md`.
