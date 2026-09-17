# Paddle capabilities

What this adapter supports, and which operations fail closed because Paddle
cannot do them. The mechanism — `billing.Capability`, `billing.Support` and
`RequireSupported` — is described in `docs/PROVIDER_CAPABILITIES.md` in
`rho-billing`; this page is the Paddle-specific half.

## Covered

Customer create and lookup, checkout preparation, one-time collection, webhook
verification, full and partial refunds, manual-invoice and automatic-collection
subscriptions, quantity and proration updates, immediate and next-period usage
charges, and pause, resume and cancel.

## Limits that fail closed

- **Automatic top-up.** Paddle has no native auto-top-up API. Unattended
  collection is an explicit `ChargeSubscription` call made by the host.
- **Manual collection on charge.** A charge against a manual-collection
  subscription is rejected before any HTTP request. An issued invoice is not
  payment.
- **Resume to next billing period.** Unsupported; resume is immediate.
- **Server-only card capture.** Completing a checkout needs Paddle.js and a
  client-side token. There is no server-side pay API, so first capture is a
  browser handoff.

## Rate limiting

HTTP 429 maps to `ErrRateLimited` carrying `RetryAfter`, parsed from either
form the header allows. It is not an unknown outcome. A dispatch releases its
unused send permission with `ReleaseOutbox` and retries after the delay, so a
rate limit never becomes a terminal rejection.

## Outcome classes

A failed provider call is one of three things, and the distinction decides
whether money may have moved:

| Class | Cause | Outbox result |
|---|---|---|
| Refused before sending | 429, 401, 403, 408 | released and retried |
| Uncertain | timeout, 5xx, unreadable success | unknown, resolved by lookup |
| Rejected | a definite provider refusal | terminal |
