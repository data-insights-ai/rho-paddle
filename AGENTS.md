# rho-paddle

Contributor notes for the Paddle adapter.

Read `README.md`, `CONTRIBUTING.md` and `docs/SANDBOX.md` before changing
behaviour. The core module is `github.com/data-insights-ai/rho-billing`.

- Go module: `github.com/data-insights-ai/rho-paddle`.
- Keep credentials, webhook secrets and customer payloads out of source and logs.
- Constructors perform no provider calls, migrations or worker startup.
- Never retry an uncertain monetary mutation blindly.
- Verify raw webhook bytes before parsing and commit receipt before success.
- Provider IDs are scoped by merchant and environment.
- `sh scripts/check.sh` unsets sandbox credentials.

## The rho-billing dependency

A local `go.work` (gitignored) substitutes `../rho-billing`, so a plain
`go test ./...` here compiles against that working tree. CI has no
`go.work`; it resolves the version in `go.mod` from the module proxy, and
so does every consumer. The two are not the same code, and when they
differ, local runs are the ones that lie.

This has cost real time twice. Tests written against an unreleased
rho-billing change passed here and could not possibly pass in CI, because
`go.mod` pinned a version two releases behind them. The failure looks like
a broken test, not like a stale pin, so it sends you reading the test.

Rules, in order:

1. **`scripts/check.sh` is the gate and it exports `GOWORK=off`.** It
   prints on its first line which rho-billing it used. Never judge
   releasability from a bare `go test`.
2. **Depending on an unreleased rho-billing change means releasing it
   first.** Tag rho-billing, then
   `GOWORK=off go get github.com/data-insights-ai/rho-billing@vX.Y.Z`,
   then `(cd testdata/host && GOWORK=off go mod tidy)`. `check.sh` fails
   if the two `go.mod` files disagree, but it cannot tell that both are
   behind.
3. **A tag here freezes the pin that was in `go.mod` at that commit.**
   Shipping a release whose behaviour needs a newer rho-billing than it
   requires is a broken release for anybody who does not also require the
   newer one themselves. Check the pin before tagging.
4. Use the workspace freely while working across both repositories. It
   just does not decide whether anything is releasable.
