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
