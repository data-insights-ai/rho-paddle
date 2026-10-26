# Changelog

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html);
pre-1.0, the exported API may change between minor versions.

## [0.11.0] - 2026-10-26

### Added

- rho-billing v0.10.0: a checkout with `CheckoutRequest.Business` bills
  the customer's business. The customer's active business of that name is
  reused, created when missing, and given the tax number when it gains or
  changes one; the transaction carries its `business_id`. The tax number
  travels only in request bodies, never in a URL or a log. A business
  without a customer is refused (`ErrBusinessNeedsCustomer`) before Paddle
  is asked. Checkouts without a business are unchanged.

## [0.10.0] - 2026-10-25

### Added

- rho-billing v0.9.0: `Charge` with `ChargeRequest.Custom` bills a one-time
  non-catalog price under the subscription's product (usage at the host's
  rate); every charge is validated before it is sent.

## [0.9.0] - 2026-10-23

### Added

- rho-billing v0.8.0: custom items with `TrialDays` send a Paddle
  `trial_period` in days; `SetNextBilling` patches `next_billed_at` with
  `proration_billing_mode: do_not_bill` (Paddle refuses it together with an
  item change, so hosts call it after `ReplaceItems`).

## [0.8.0] - 2026-10-23

### Added

- Custom items (rho-billing v0.7.0): `Checkout` sends
  `CheckoutRequest.Custom` as Paddle non-catalog prices, recurring or
  one-time, under a non-catalog product of tax category `saas`; the item's
  reference goes into the price's custom data.
- `ReplaceItems`: replaces a subscription's items with custom items under
  the product of its first item, `proration_billing_mode: do_not_bill`, so
  the next renewal bills the new price.
- `CancelAtPeriodEnd`: `POST /subscriptions/{id}/cancel` effective from the
  next billing period.

## [0.7.0] - 2026-10-22

### Added

- `ClaimCustomer`: `CreateCustomer` for an address the host has proven to be
  the buyer's own. An existing Paddle customer with that address and no
  `custom_data.reference` gets this account's reference (its other custom
  data kept) and is answered; one whose reference is another account's is
  still refused with `ErrCustomerExists`.

## [0.6.1] - 2026-10-22

### Security

- `CreateCustomer` no longer answers an existing Paddle customer just
  because Paddle reports the email address as taken: that customer is
  answered only when its `custom_data.reference` equals the request's
  reference exactly, else `ErrCustomerExists`. v0.6.0 linked whichever
  customer had the address; do not use it.

## [0.6.0] - 2026-10-22

### Added

- `CreateCustomer` (rho-billing v0.6 `provider.CustomerRequest`): creates a
  Paddle customer with the account's reference as custom data; an email
  address Paddle already has answers that customer.
- `GetTransaction`: a transaction's status, customer, subscription and
  reference, for links the host did not start (renewal recovery).

### Changed

- Requires rho-billing v0.6.0.

## [0.5.1] - 2026-10-21

### Added

- `APIError`: a refused request's status, Paddle error code, request id
  and Paddle's explanation (`Detail`, control characters removed, capped at
  300 bytes, dropped if it contains the API key). `Detail` is not part of
  `Error()`, so it reaches a log only when the host puts it there.

## [0.5.0] - 2026-10-20

### Changed

- Rebuilt as the stateless Paddle plugin of rho-billing v0.5 `provider`:
  checkout, customer portal, catalog, one-time charges and a webhook
  handler that decodes `subscription.*` and `transaction.completed`.
  Duplicates and ignored kinds answer 200; host failures 500.

### Removed

- Postgres storage, workers, recovery scans, purchases, refunds,
  adjustments and the rho-billing v0.3 integration. Hosts that use them stay
  on v0.4.1.

## [0.4.1] - 2026-09-22

### Fixed

- `go.mod` pinned `rho-billing v0.3.5` while this adapter's behaviour needs
  `v0.3.7`. The two releases in between are what let a payment settle after
  the quote it was made against expires; without them, a consumer of v0.4.0
  that does not itself require the newer version still refuses those
  payments, which is the opposite of what v0.4.0 says it does. Anyone who
  required `rho-billing v0.3.7` directly was unaffected, because module
  resolution takes the highest requirement.

  The pin was stale because the test runs that would have caught it were
  the database-backed ones, and they only ran nightly in a separate
  workflow. `scripts/check.sh` now runs the whole suite against the pinned
  rho-billing (`GOWORK=off`, so a local checkout cannot stand in for it)
  and CI runs it on every push, with and without a database.

## [0.4.0] - 2026-09-22

### Changed

- **The provider decides the money.** Paddle is the merchant of record: it
  holds the price list, applies the discounts, charges the card and shows
  the customer the figure before they agree to it. This adapter used to
  recompute that figure and refuse the payment when the two disagreed.
  Every such refusal was a customer who had paid and received nothing,
  over a disagreement about arithmetic that was not ours to do.

  Gone: the comparison against the quote, the refusal of a price id other
  than the one quoted, and the upper bound on collection time. A repriced
  line applies and leaves the binding alone; a payment collected after the
  quote expired applies. What remains refused is a payment that cannot be
  read as money, one from another customer or in another currency, and one
  collected before the purchase it pays for existed.

## [0.3.12] - 2026-09-21

### Fixed

- `testdata/host` pinned an older `rho-billing` than this module, so its
  build failed with "updates to go.mod needed", which says nothing about
  the cause. `scripts/check.sh` compares the two and names it.

## [0.3.11] - 2026-09-21

### Changed

- The payment record holds what the provider collected rather than what
  the quote expected, and a difference is no longer a rejection.

## [0.3.10] - 2026-09-21

### Fixed

- A partial discount with tax charged on top was read as inconsistent
  totals and refused.

## [0.3.9] - 2026-09-21

### Added

- The transaction that failed in production, pinned as a test.

## [0.3.8] - 2026-09-21

### Fixed

- A provider discount was not read, so a purchase it reduced to zero was
  refused as a zero-value transaction.

## [0.3.7] - 2026-09-19

### Fixed

- `Transaction` and `Transactions` rejected a prorated plan change: the
  provider lists the credited old item with a negative quantity, and the
  decoder treated any non-positive quantity as an invalid response. A whole
  billing history disappeared behind one such transaction. Only a zero
  quantity is refused now.

## [0.3.6] - 2026-09-19

### Fixed

- `PreviewSubscriptionUpdate` rejected every preview that carried a credit:
  the provider states the credit component as a negative amount. Charge and
  credit are reported as magnitudes; `Action` says which way the net goes.

## [0.3.5] - 2026-09-19

Requires `github.com/data-insights-ai/rho-billing` v0.3.2.

### Added

- `Transaction.Total`, `Tax`, `BilledAt` and `InvoiceNumber`: what a billing
  history needs to list a transaction.
- `Client.InvoiceURL`: a short-lived link to the provider's invoice PDF.
- `OriginAny` for `Transactions`: a customer's whole history, every origin,
  where recovery scans keep to one.

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
