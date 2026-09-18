# rho-paddle

Paddle Billing adapter for `github.com/data-insights-ai/rho-billing`. It maps Paddle
HTTP, webhooks and subscription commands onto the core library. It is not
a general Paddle SDK.

Module: `github.com/data-insights-ai/rho-paddle`. License: Apache 2.0. Go 1.26.
Depends on `github.com/data-insights-ai/rho-billing`. Release: `v0.3.1`.

The host owns routes, authorization, worker scheduling and secret
storage. Constructors do no I/O.

## Install

```
require github.com/data-insights-ai/rho-billing v0.3.1
require github.com/data-insights-ai/rho-paddle v0.3.1
```

To develop against a local `rho-billing` checkout, put one beside this
repository and use a workspace — `go work init . ../rho-billing`. The module
file carries no `replace`, so the published module resolves its dependency
normally.

## Boundary

Paddle owns customers, payment methods, tax, invoices and collection.
This adapter sends commands and turns Paddle facts into core purchase,
subscription and adjustment records. It does not store cards or invoice
PDFs.

`ErrUncertain` means a mutation may have reached Paddle (timeout after
POST, malformed success). Record unknown and reconcile. Do not retry
the mutation blindly. HTTP 429 is `ErrRateLimited` with `RetryAfter`, parsed
from either `Retry-After` form. The client itself never waits or retries.

A dispatched checkout or refund distinguishes three outcomes. Refused before
the mutation could take effect (429, 401, 403, 408) returns the message to the
queue with `integration.ErrBackpressure` and retries after the provider's
delay. A response that may reflect a mutation stays unknown for recovery. Only
a definite provider rejection is terminal.

## Operations

| Call | What it does |
|---|---|
| `New` | Merchant + sandbox/live. No network. |
| `CreateCustomer` | Paddle customer with host operation correlation. |
| `CreateCheckout` | Automatic catalog-price transaction. |
| `Transaction` / `Transactions` | Read by known id or list. |
| `CreatePortalSession` | Temporary portal URLs. Do not log or persist them. |
| `Subscription` | Read. |
| `UpdateSubscription` | Items + proration mode. |
| `ChargeSubscription` | One-off catalog prices on an automatic subscription. GET the subscription first; empty/manual collection is unsupported. Timeout after POST is `ErrUncertain`; recover with `Transactions` (`origin=subscription_charge`, `subscription_id`). |
| `PauseSubscription` / `ResumeSubscription` / `CancelSubscription` | Capability check first. Resume does not accept `next_billing_period` (Paddle: `immediately` or RFC3339). Next-period pause/cancel require `scheduled_change` on the response. Timeout after submit is `ErrUncertain`. |
| `CreateAdjustment` / `Adjustment` / `Adjustments` | Refunds. Full refunds omit `items` on the wire. Known-id recovery uses GET by adjustment id. |
| `NewRefundService` / `PlanRefund` / `Dispatch` / `RecoverRefund` | Durable outbox refunds. |
| `NewWebhookVerifier` + `integration.ReceiveWebhook` | Verify raw body, then persist. |
| `NewCheckoutService` / `PlanCheckout` / `Dispatch` / `RecoverCheckout` | Durable outbox checkout. |
| `CreateNotificationSetting` / `DeleteNotificationSetting` | Notification destination. |
| `NewTransactionProcessor` / `NewAdjustmentProcessor` | Apply authenticated inbox claims. The transaction processor makes no provider calls; the adjustment processor looks up original adjustments for chargeback reversals, so it needs a `Client` and a lease that allows for it. |

Manual invoice collection is unsupported on charge. Automatic top-up
is fail-closed: Paddle has no native auto-top-up API. Host collection
is `ChargeSubscription`.

## Tests

```sh
go test ./...
BILLING_TEST_DATABASE_URL='postgres://…' sh scripts/check-local.sh
```

`check-local.sh` unsets Paddle sandbox env. HTTP is fake. Opt-in live
sandbox tests need `PADDLE_SANDBOX_TEST=1` and a key file (mode 0600),
never credentials in git or argv.

Opt-in sandbox tests, and the merchant setup they need:
[docs/SANDBOX.md](docs/SANDBOX.md). Which operations fail closed and why:
[docs/CAPABILITIES.md](docs/CAPABILITIES.md).

## Documentation

- [docs/CAPABILITIES.md](docs/CAPABILITIES.md) — what Paddle supports, what
  fails closed, and how a failed call is classified.
- [docs/SANDBOX.md](docs/SANDBOX.md) — opt-in sandbox tests.
- [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md),
  [CHANGELOG.md](CHANGELOG.md).

## Status

`v0.3.1`. Pre-1.0: the exported API may change between minor versions.
