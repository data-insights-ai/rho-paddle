# Security

## Reporting a vulnerability

Report suspected vulnerabilities to **security@data-insights.ai**. Do not open
a public issue for a security report.

Include the affected version or commit, what an attacker can do, and the
smallest reproduction you have. We acknowledge reports within three working
days and will tell you whether we consider the report in scope and when we
expect a fix.

Never include a live Paddle API key, webhook secret or real customer payload in
a report. Do not test against a Paddle account you do not own.

## Scope

This is an adapter library. It has no listener and no process of its own: the
host owns HTTP routing, authentication, worker scheduling and secret storage.

In scope:

- Webhook signature verification accepting a payload it should reject —
  tampering, a wrong destination secret, a stale or malformed timestamp,
  malformed or oversized signature encoding.
- A provider fact being applied to the wrong account, merchant or environment.
- An uncertain mutation being converted into a blind retry, which can charge a
  customer twice.
- A refund, adjustment or payment being applied more than once, or a reversal
  losing its allocation provenance.
- Credentials, webhook secrets, customer payloads or authenticated portal URLs
  reaching logs, errors or diagnostics.

Out of scope:

- Vulnerabilities in Paddle itself. Report those to Paddle.
- Anything requiring the host to have already skipped authorization.
- The absence of a Paddle capability that this adapter deliberately fails
  closed on — see [docs/CAPABILITIES.md](docs/CAPABILITIES.md).

## How this library handles secrets

Sandbox credentials are read from a key file with mode `0600`, referenced by
environment variable, never from source, command arguments or committed files.
`scripts/check.sh` unsets the sandbox variables so the ordinary gate
cannot reach a provider even if the environment is set.

Webhook verification runs on the raw request bytes before any parsing, so a
payload is never decoded before it is trusted. Receipt is committed before the
handler reports success.

Transport pins the API origin, refuses redirects, bounds response bodies and
redacts reflected keys from diagnostics, so a provider error cannot carry a
secret into a log line. Portal URLs are temporary authenticated links: use them
and discard them, never log or persist them.

## Supported versions

Pre-1.0. Only the latest released version receives fixes.
