module github.com/data-insights-ai/rho-paddle

go 1.26.0

require (
	github.com/data-insights-ai/rho-billing v0.3.0
	github.com/jackc/pgx/v5 v5.11.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/text v0.39.0 // indirect
)

// Withdrawn before the first stable release. They remain fetchable from the
// module proxy, which is immutable, but nothing should select them.
retract (
	v0.1.0 // Superseded; sandbox tests carried provider object identifiers.
	v0.2.0 // Superseded; same.
	v0.2.1 // Superseded by a rewritten, squashed history.
)
