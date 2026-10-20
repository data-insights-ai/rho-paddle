# Paddle sandbox tests

The ordinary suite never loads credentials and never contacts Paddle; HTTP
is faked. Sandbox tests are opt-in:

```sh
PADDLE_SANDBOX_TEST=1 PADDLE_SANDBOX_KEY_FILE=/path/to/private/sandbox-api-key \
  go test . -run '^TestSandbox' -count=1 -v
```

The key file lives outside the repository with mode `0600`. Credentials never
go in source, arguments, fixtures or checked-in environment files;
`scripts/check.sh` unsets both variables.

`TestSandboxCatalog` reads the catalog and changes nothing: it proves the key
works and Paddle's answers decode, not that money moved. Capturing a payment
needs Paddle.js in a browser; a checkout created here is a draft until then.

A fresh sandbox needs a default payment link on the merchant, or every
transaction create answers `transaction_default_checkout_url_not_set`.
