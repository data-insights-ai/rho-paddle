# Changelog

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html);
pre-1.0, the exported API may change between minor versions.

## [0.3.4] - 2026-09-19

Requires `github.com/data-insights-ai/rho-billing` v0.3.2.

### Added

- `Client.PreviewSubscriptionUpdate`: what `UpdateSubscription` with the same
  input would charge or credit now, and what recurs afterwards, without
  applying it. A host shows this before asking the customer to confirm a
  plan change. The update and its preview share one request builder.

## [0.3.3] - 2026-09-19

Requires `github.com/data-insights-ai/rho-billing` v0.3.2.

### Added

- `PriceSpec.Quantity` (`QuantityBounds{Min, Max}`): the quantity range a
  transaction line may carry. Undeclared leaves the provider's default of
  1 to 100. A host that binds each purchase to exactly one unit declares
  1 and 1, so the checkout cannot be changed to a quantity the host would
  refuse to fulfil after the money has been taken. `ReconcileCatalog`
  brings an existing price's bounds in line by editing them; unlike amount,
  currency, tax mode and billing cycle, they are not commercial terms.

## [0.3.2] - 2026-09-18

Requires `github.com/data-insights-ai/rho-billing` v0.3.2.

### Fixed

- Paddle re-issues transaction line item ids (`txnitm_`) whenever it
  recomputes a transaction: the checkout sending its items, an address being
  set, tax being applied. The `TransactionProcessor` required the id bound at
  checkout to appear on the paid transaction, so every real checkout failed
  with a conflict at `transaction.paid` and the customer never received what
  they paid for. Provider lines are now matched by id when it still holds and
  otherwise by price and quantity, and the binding is re-keyed to the paid
  transaction's ids in the same database transaction as the payment fact, so
  later refunds and adjustments reference ids the provider still knows. A
  different price under a new id remains a conflict.

## [0.3.1] - 2026-09-18

Requires `github.com/data-insights-ai/rho-billing` v0.3.1.

### Fixed

- `NewWebhookHandler` answered 503 to a request the verifier rejected: a bad or
  missing signature, a stale timestamp, an unparseable payload. The verifier's
  errors are distinct values from the core's, and the handler mapped only the
  core's. A 5xx asks the provider to redeliver, so a forgery was retried until
  the provider gave up. Rejections are 400 now, and a test holds it there.

## [0.3.0] - 2026-09-17

Requires `github.com/data-insights-ai/rho-billing` v0.3.0.

Republished from a squashed history: the first release commit carried sandbox
object identifiers in its tests. 0.1.0, 0.2.0 and 0.2.1 are retracted.

### Removed

- The sandbox tests that could only run against a specific captured payment.
  Capturing a card has no server-side API, so those tests could never provision
  their own subject and instead named objects in one private merchant account.
  Refund and charge behaviour stays covered by the faked-HTTP and PostgreSQL
  tests; what is gone is the end-to-end evidence against a real captured
  payment.

## [0.2.1] - 2026-09-17

### Changed

- Sandbox tests provision their own provider objects and archive them, instead
  of depending on identifiers from one private merchant account. The suite now
  runs against any sandbox with nothing configured but a key. The few tests that
  need an already-captured card payment — which has no server-side API — skip
  unless one is named, and the "preserved object" guards are gone: a test
  mutates only what it created or what the operator explicitly pointed it at.

## [0.2.0] - 2026-09-17

Requires `github.com/data-insights-ai/rho-billing` v0.2.0.

### Added

- `NewWebhookHandler`: the provider notification endpoint, including the status
  contract with Paddle's retry machinery — 204 for a fresh *and* a duplicate
  receipt, 400 for what redelivery cannot fix, 503 for the host's own failures.
  Every host was reimplementing this, and both directions of getting it wrong
  are expensive.
- `Client.ReconcileCatalog`: declarative products and prices, matched on a host
  key in provider custom data. Missing objects are created, matching ones left
  alone, and one that differs is reported as `ErrCatalogDrift` rather than
  edited — a live price is what existing subscriptions renew against. Nothing
  is ever deleted.
- `Client.LinkCustomer`: find-or-create for a provider customer, verifying the
  host account binding. An email is not proof of ownership, so a customer bound
  to another account is `ErrCustomerForeign` and an unbound one is
  `ErrCustomerUnlinked`.
- `CheckoutService.Start`: the whole first-capture flow in one call — link,
  plan, claim, dispatch — with a replayed operation and an already-queued
  message treated as the successes they are.

### Fixed

- `CreateCustomer` fails with `customer_already_exists` once an email has been
  used, so a checkout flow built on it broke for every returning customer: an
  upgrade, a resubscribe, a retry after a declined card. Use `LinkCustomer`.
- Provider-stored emails are lowercased, so comparing them byte-for-byte
  rejected the provider's own record.



## [0.1.0] - 2026-09-17

First release. Requires `github.com/data-insights-ai/rho-billing` v0.1.0.

### Added

- Client for Paddle customers, checkouts, transactions, portal sessions,
  subscriptions and adjustments, scoped by merchant and environment.
- Webhook verification on raw request bytes, with receipt committed before
  success and duplicate events converging safely.
- Durable outbox services for checkout and refund dispatch, with recovery of a
  create whose response was lost.
- Inbox processors that apply authenticated transaction and adjustment facts to
  core purchase state. The transaction processor makes no provider calls; the
  adjustment processor looks up original adjustments for chargeback reversals.
- Subscription lifecycle: update with proration, immediate and next-period
  charges, pause, resume and cancel, each capability-checked before dispatch.
- `ErrUncertain` for mutations that may have reached Paddle, and
  `ErrRateLimited` carrying `RetryAfter` for HTTP 429.
- Opt-in sandbox tests, and `testdata/host` as a separate module compiling
  against the public API only.
- Dispatch classifies a failed provider call as refused-before-send, uncertain,
  or definitely rejected, and releases the send permission for the first so it
  is retried rather than terminally rejected.
- Provider wire shapes live in `internal/paddlewire`; no exported type carries
  Paddle JSON. `Event` exposes typed accessors (`CustomerID`, `TransactionID`,
  `SubscriptionID`, `Correlation`) instead of a raw payload, so a Paddle schema
  change cannot break host resolvers at runtime.

[0.1.0]: https://github.com/data-insights-ai/rho-paddle/releases/tag/v0.1.0
