module github.com/data-insights-ai/rho-paddle

go 1.27.1

require github.com/data-insights-ai/rho-billing v0.9.0

// Withdrawn before the first stable release. They remain fetchable from the
// module proxy, which is immutable, but nothing should select them.
retract (
	v0.2.1 // Superseded by a rewritten, squashed history.
	v0.2.0 // Superseded; same.
	v0.1.0 // Superseded; sandbox tests carried provider object identifiers.
)

retract v0.6.0 // CreateCustomer answered any customer whose email address Paddle reported as taken
