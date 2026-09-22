# Paddle sandbox tests

The ordinary test suite never loads credentials and never contacts Paddle. HTTP
is faked. Sandbox tests are opt-in and skip unless you enable them explicitly.

## Running them

```sh
PADDLE_SANDBOX_TEST=1 \
PADDLE_SANDBOX_KEY_FILE=/path/to/private/sandbox-api-key \
BILLING_TEST_DATABASE_URL=postgres://... \
go test . -run '^TestSandbox' -count=1 -v
```

The key file lives outside the repository, with mode `0600` and a private
parent directory. Credentials never go in source, command arguments, fixtures
or checked-in environment files — `scripts/check.sh` unsets both sandbox
variables so the gate cannot pick them up by accident.

Tests create everything they need and archive it afterwards. No identifier from
anyone's merchant account appears in this repository, and no test depends on an
object someone made by hand: a suite built that way cannot be run by anyone else,
and it puts the account owner's data one bad assertion away from being mutated.

Some tests are the exception, and only because the provider has no server-side
way to capture a card payment — that needs Paddle.js and a browser. They skip
unless you name an already-captured object, and naming one is the authorisation
to use it:

| Variable | Needed by |
| --- | --- |
| `PADDLE_SANDBOX_CAPTURED_CUSTOMER` | reading a customer's completed transactions |
| `PADDLE_SANDBOX_CAPTURED_TRANSACTION_ID` | reading a captured recurring transaction |
| `PADDLE_SANDBOX_CAPTURED_SUBSCRIPTION_ID` | charging an automatic subscription |
| `PADDLE_SANDBOX_REFUND_TRANSACTION_ID` | a full refund — this transaction **is** refunded |
| `PADDLE_SANDBOX_PARTIAL_REFUND_TRANSACTION_ID` | a partial refund — this transaction **is** refunded |

`PADDLE_SANDBOX_CHECKOUT_PAGE_URL` optionally selects a public host page.

## Merchant prerequisites

These are properties of the Paddle account, not of this code, and a fresh
sandbox will fail without them:

- **A default payment link must be set on the merchant.** Otherwise every
  transaction create returns HTTP 400
  `transaction_default_checkout_url_not_set` — including when an explicit
  `checkout.url` is supplied. Any URL works for tests.
- **Capture needs a browser.** There is no documented server-side API that
  marks an automatic transaction paid from a sandbox API key alone. Completing
  a payment requires Paddle.js and a client-side token, so first capture is a
  browser handoff. `internal/cmd/sandboxcheckout` serves a local overlay page
  for this. Hosts own their own Paddle.js page and domain setup; this library
  implements no frontend.

## What sandbox tests may and may not do

Tests create uniquely labelled products, prices and customers and archive them
during cleanup. Unpaid draft transactions are left behind deliberately, as
recovery evidence.

Nothing is protected by a list of objects to avoid, because nothing needs to be:
a test mutates only what it created itself, or what you explicitly named in one
of the variables above. The lifecycle test cancels a subscription it billed
moments earlier; the refund tests refund the transaction you pointed them at and
nothing else.

## Reading the results honestly

Four things are distinct, and a test name should say which one it proves:

1. **API acceptance** — Paddle returned 200 for a create. No money moved.
2. **Captured payment** — a real payment method completed a transaction.
3. **Completed processing** — the adapter applied the fact to core state.
4. **A simulated webhook** — a signed fixture. This is not provider evidence
   of anything.

A draft transaction is not a payment. An issued manual invoice is not a
payment. Never treat an uncertain create as rejected because an eventually
consistent list call does not show it yet.

## References

- [Create transaction](https://developer.paddle.com/api-reference/transactions/create-transaction/)
- [List transactions](https://developer.paddle.com/api-reference/transactions/list-transactions/)
